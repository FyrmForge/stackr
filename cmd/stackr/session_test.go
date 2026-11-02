package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/cliconfig"
)

func readConfig(t *testing.T) config {
	t.Helper()
	b, err := os.ReadFile(os.Getenv("STACKR_CONFIG"))
	if err != nil {
		t.Fatal(err)
	}
	var c config
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func writeConfig(t *testing.T, c config) {
	t.Helper()
	b, _ := json.Marshal(c)
	if err := os.WriteFile(os.Getenv("STACKR_CONFIG"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// logout revokes the key a browser login minted and forgets it; a key the
// user pasted in has no id here and is only forgotten.
func TestLogoutRevokesItsKey(t *testing.T) {
	r := fake(t, nil)
	c := readConfig(t)
	c.KeyID = "k1"
	writeConfig(t, c)
	t.Setenv("STACKR_KEY", "env-key") // never the one revoked
	if code, _, errw := cli(t, "logout"); code != 0 {
		t.Fatalf("logout = %d %s", code, errw)
	}
	if w := writes(r); len(w) != 1 || w[0] != "DELETE /api/v1/me/keys/k1 " {
		t.Errorf("sent %q, want one DELETE of k1", w)
	}
	if c := readConfig(t); c.Key != "" || c.KeyID != "" {
		t.Errorf("config kept the login: %+v", c)
	}
	r.reqs = nil
	if code, _, _ := cli(t, "logout"); code != 0 || len(r.reqs) != 0 {
		t.Errorf("second logout sent %q", r.reqs)
	}
}

// --profile picks a saved login for one run, profile use moves the current
// one, and neither disturbs the other profile's login.
func TestProfiles(t *testing.T) {
	r := fake(t, map[string]string{"/me": `{"email":"w@x"}`})
	c := readConfig(t)
	c.Profiles = map[string]cliconfig.Profile{"work": {Server: "http://work.invalid", Key: "wk", Org: "w"}}
	writeConfig(t, c)

	if code, out, _ := cli(t, "profile", "ls"); code != 0 || !strings.Contains(out, "default") || !strings.Contains(out, "work") {
		t.Errorf("profile ls = %d %q", code, out)
	}
	if code, _, errw := cli(t, "--profile", "work", "status"); code != 1 || !strings.Contains(errw, "work.invalid") {
		t.Errorf("--profile work status = %d %q, want a dial to work.invalid", code, errw)
	}
	t.Setenv("STACKR_PROFILE", "nope")
	if code, _, errw := cli(t, "status"); code != 2 || !strings.Contains(errw, `no profile "nope"`) {
		t.Errorf("STACKR_PROFILE=nope = %d %q", code, errw)
	}
	t.Setenv("STACKR_PROFILE", "")
	if len(r.reqs) != 0 {
		t.Errorf("default server was asked: %q", r.reqs)
	}
	if code, _, errw := cli(t, "profile", "use", "work"); code != 0 {
		t.Fatalf("profile use = %d %s", code, errw)
	}
	if c := readConfig(t); c.Name() != "work" || c.Server != "http://work.invalid" || c.Profiles["default"].Key != "k" {
		t.Errorf("after use: %+v", c)
	}
	// a link written under --profile does not move the current profile
	if code, _, errw := cli(t, "--profile", "default", "link", "--stack", "shop"); code != 0 {
		t.Fatalf("link = %d %s", code, errw)
	}
	if c := readConfig(t); c.Name() != "work" || len(c.Links) != 1 {
		t.Errorf("after link: %+v", c)
	}
}

// A config file that cannot be read stops every command but login and
// logout, which replace it; the message says how to start over.
func TestCorruptConfig(t *testing.T) {
	fake(t, nil)
	if err := os.WriteFile(os.Getenv("STACKR_CONFIG"), []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errw := cli(t, "status"); code != 1 || !strings.Contains(errw, "delete it") {
		t.Errorf("status = %d %q", code, errw)
	}
	if code, _, errw := cli(t, "logout"); code != 0 {
		t.Errorf("logout = %d %q", code, errw)
	}
	if c := readConfig(t); c.Key != "" {
		t.Errorf("logout left %+v", c)
	}
}

// link and unlink bind a directory and release it.
func TestLinkUnlink(t *testing.T) {
	fake(t, nil)
	if code, _, errw := cli(t, "link", "--stack", "shop", "--env", "dev"); code != 0 {
		t.Fatalf("link = %d %s", code, errw)
	}
	if c := readConfig(t); len(c.Links) != 1 {
		t.Fatalf("links after link: %+v", c.Links)
	}
	if code, _, errw := cli(t, "unlink"); code != 0 {
		t.Fatalf("unlink = %d %s", code, errw)
	}
	if c := readConfig(t); len(c.Links) != 0 {
		t.Errorf("links after unlink: %+v", c.Links)
	}
}

// A param change that redeploys prints the job ids under --json, so CI can
// follow them; --wait is the same follow, inline.
func TestParamsSetJSONHasJobs(t *testing.T) {
	fake(t, map[string]string{"/params": `[{"env":"dev","tile":"web","job":"j1"}]`})
	code, out, errw := cli(t, "--json", "params", "set", "app.mode=prod", "--env", "dev", "--stack", "shop")
	if code != 0 || !strings.Contains(out, `"job": "j1"`) || errw != "redeploying web (dev): stackr job log j1 --follow\n" {
		t.Errorf("params set --json = %d %q %q", code, out, errw)
	}
}

// lockedBuf is a writer the test reads while run still holds it.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// The browser login prints the approve link with its loopback port; the
// panel's page hands the code back there, and the CLI trades it for a key
// whose id it keeps for logout.
func TestBrowserLogin(t *testing.T) {
	t.Setenv("STACKR_NO_BROWSER", "1")
	t.Setenv("STACKR_KEY", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/exchange":
			_, _ = io.WriteString(w, `{"token":"tok","key":{"id":"k9"}}`)
		case "/api/v1/orgs":
			_, _ = io.WriteString(w, `[{"slug":"acme"}]`)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("STACKR_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	var out, errw lockedBuf
	done := make(chan int, 1)
	go func() {
		done <- run(context.Background(), []string{"login", srv.URL}, strings.NewReader(""), &out, &errw, false)
	}()
	var link *url.URL
	for i := 0; i < 200 && link == nil; i++ {
		for _, l := range strings.Split(errw.String(), "\n") {
			if u, err := url.Parse(strings.TrimSpace(l)); err == nil && u.Path == "/cli/authorize" {
				link = u
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if link == nil {
		t.Fatalf("no approve link in %q", errw.String())
	}
	cb := "http://127.0.0.1:" + link.Query().Get("port") + "/?code=c1&state=" + link.Query().Get("state")
	if res, err := http.Get(cb + "x"); err != nil || res.StatusCode != http.StatusBadRequest {
		t.Fatalf("a wrong state = %v %v, want 400", res, err)
	}
	if res, err := http.Get(cb); err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("callback = %v %v", res, err)
	}
	if code := <-done; code != 0 {
		t.Fatalf("login = %d %s", code, errw.String())
	}
	if c := readConfig(t); c.Key != "tok" || c.KeyID != "k9" || c.Org != "acme" {
		t.Errorf("config after login: key set %v, id %q, org %q", c.Key != "", c.KeyID, c.Org)
	}
}

// A new login revokes the browser-minted key it replaces; the profile
// commands run even when STACKR_PROFILE names a profile that is gone.
func TestReloginRevokesOldKeyAndProfileCmdsRun(t *testing.T) {
	r := fake(t, map[string]string{"/orgs": `[{"slug":"acme"}]`})
	c := readConfig(t)
	c.KeyID = "old1"
	writeConfig(t, c)
	if code, _, errw := cli(t, "login", c.Server, "--with-key", "newkey"); code != 0 {
		t.Fatalf("login = %d %s", code, errw)
	}
	if w := writes(r); len(w) != 1 || w[0] != "DELETE /api/v1/me/keys/old1 " {
		t.Errorf("sent %q, want one DELETE of old1", w)
	}
	if c := readConfig(t); c.Key != "newkey" || c.KeyID != "" {
		t.Errorf("config after relogin: %+v", c)
	}
	t.Setenv("STACKR_PROFILE", "gone")
	if code, _, errw := cli(t, "profile", "ls"); code != 0 {
		t.Errorf("profile ls = %d %s", code, errw)
	}
}

// A follow whose stream ends without an end event says how to pick it up.
func TestFollowStreamEndedEarly(t *testing.T) {
	fake(t, nil)
	code, _, errw := cli(t, "job", "log", "j1", "--follow")
	if code != 1 || !strings.Contains(errw, "stream ended before job") {
		t.Errorf("follow = %d %q", code, errw)
	}
}

// With a terminal, a confirmation is asked and a yes goes ahead.
func TestConfirmOnATerminal(t *testing.T) {
	r := fake(t, nil)
	var out, errw bytes.Buffer
	if code := run(context.Background(), []string{"job", "cancel", "j1"}, strings.NewReader("y\n"), &out, &errw, true); code != 0 ||
		!strings.Contains(errw.String(), "Cancel job j1?") {
		t.Fatalf("yes = %d %q", code, errw.String())
	}
	if w := writes(r); len(w) != 1 {
		t.Errorf("sent %q, want the cancel", w)
	}
	r.reqs = nil
	errw.Reset()
	if code := run(context.Background(), []string{"job", "cancel", "j1"}, strings.NewReader("n\n"), &out, &errw, true); code != 1 || len(writes(r)) != 0 {
		t.Errorf("no = %d, sent %q", code, r.reqs)
	}
}

// tile logs --json is NDJSON with or without --follow.
func TestLogsJSONIsNDJSON(t *testing.T) {
	fake(t, map[string]string{"/logs": `{"log":"a\nb\n"}`})
	code, out, errw := cli(t, "--json", "tile", "logs", "api", "--stack", "shop", "--env", "dev")
	if code != 0 || out != "{\"line\":\"a\"}\n{\"line\":\"b\"}\n" {
		t.Errorf("logs --json = %d %q %q", code, out, errw)
	}
}

// A rung's --set value is JSON, except for a string knob, which keeps
// digits as text.
func TestKnobValueByType(t *testing.T) {
	r := fake(t, map[string]string{
		"/settings":    `[{"key":"protect_user","type":"string"},{"key":"mem_limit_mb","type":"int"}]`,
		"/stacks/shop": `{"settings":""}`,
	})
	if code, _, errw := cli(t, "stack", "defaults", "--stack", "shop", "--set", "protect_user=123", "--set", "mem_limit_mb=512"); code != 0 {
		t.Fatalf("set = %d %s", code, errw)
	}
	w := writes(r)
	if len(w) != 1 || !strings.HasSuffix(w[0], `{"mem_limit_mb":512,"protect_user":"123"}`) {
		t.Errorf("sent %q", w)
	}
}

package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

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

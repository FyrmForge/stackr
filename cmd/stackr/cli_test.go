package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/FyrmForge/hamr/pkg/server"
	"github.com/spf13/cobra"

	"github.com/FyrmForge/stackr/internal/api"
	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// cli runs the CLI in a temp config, piped (no terminal), returning the
// exit code, stdout and stderr.
func cli(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errw bytes.Buffer
	code := run(context.Background(), args, strings.NewReader(""), &out, &errw, false)
	return code, out.String(), errw.String()
}

func useServer(t *testing.T, server string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("STACKR_CONFIG", p)
	b, _ := json.Marshal(config{Server: server, Key: "k", Org: "acme"})
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// recorder is a fake server that records each request and answers {}.
type recorder struct {
	mu   sync.Mutex
	reqs []string // "METHOD path body"
}

func (r *recorder) serve(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.reqs = append(r.reqs, req.Method+" "+req.URL.Path+" "+string(b))
		r.mu.Unlock()
		_, _ = io.WriteString(w, `{"tile":{}}`)
	}))
	t.Cleanup(srv.Close)
	useServer(t, srv.URL)
}

// Every API operation has a CLI verb or a written reason not to, and no
// verb names an operation the API lacks.
func TestEveryRouteHasAVerb(t *testing.T) {
	b, err := os.ReadFile("../../docs/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Paths map[string]map[string]struct {
			OperationID string `json:"operationId"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(b, &spec); err != nil {
		t.Fatal(err)
	}
	ops := map[string]bool{}
	for _, p := range spec.Paths {
		for _, o := range p {
			ops[o.OperationID] = true
		}
	}
	covered := map[string]bool{}
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, op := range strings.Split(c.Annotations["op"], ",") {
			if op == "" {
				continue
			}
			if !ops[op] {
				t.Errorf("%s names op %s, which the API does not have", c.CommandPath(), op)
			}
			covered[op] = true
		}
		for _, s := range c.Commands() {
			walk(s)
		}
	}
	walk((&app{}).root())
	var missing []string
	for op := range ops {
		if !covered[op] && skipped[op] == "" {
			missing = append(missing, op)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("no CLI verb and no reason for: %s", strings.Join(missing, " "))
	}
	for op := range skipped {
		if !ops[op] {
			t.Errorf("skip list names %s, which the API does not have", op)
		}
	}
}

// B1, B21, B22: a set sends only the flags given, and nothing given is a
// usage error with no request at all.
func TestSetSendsOnlyWhatWasGiven(t *testing.T) {
	r := &recorder{}
	r.serve(t)
	if code, _, errw := cli(t, "tile", "set", "api", "--stack", "shop", "--env", "dev", "--port", "8080"); code != 0 {
		t.Fatalf("set = %d %s", code, errw)
	}
	want := `PATCH /api/v1/orgs/acme/stacks/shop/envs/dev/tiles/api {"container_port":8080}`
	if len(r.reqs) != 1 || r.reqs[0] != want {
		t.Errorf("sent %q, want exactly %q", r.reqs, want)
	}
	r.reqs = nil
	code, _, errw := cli(t, "tile", "set", "api", "--stack", "shop", "--env", "dev")
	if code != 2 || !strings.Contains(errw, "nothing to set") || len(r.reqs) != 0 {
		t.Errorf("empty set = %d %q, sent %q; want exit 2 and no request", code, errw, r.reqs)
	}
	// A given empty value is sent: clearing is a deliberate act.
	r.reqs = nil
	cli(t, "tile", "set", "api", "--stack", "shop", "--env", "dev", "--command", "")
	if len(r.reqs) != 1 || !strings.HasSuffix(r.reqs[0], ` {"command":""}`) {
		t.Errorf("clear sent %q", r.reqs)
	}
}

// -y skips the prompt and nothing more; without it a non-interactive run
// refuses and says what it would have done.
func TestYesIsNotForce(t *testing.T) {
	r := &recorder{}
	r.serve(t)
	code, _, errw := cli(t, "env", "rm", "--stack", "shop", "--env", "dev")
	if code != 1 || !strings.Contains(errw, "refusing without --yes (non-interactive): Remove environment dev") ||
		len(r.reqs) != 0 {
		t.Errorf("no -y = %d %q, sent %q", code, errw, r.reqs)
	}
	if code, _, errw := cli(t, "env", "rm", "--stack", "shop", "--env", "dev", "-y"); code != 0 {
		t.Fatalf("-y = %d %s", code, errw)
	}
	if len(r.reqs) != 1 || r.reqs[0] != "DELETE /api/v1/orgs/acme/stacks/shop/envs/dev " {
		t.Errorf("-y sent %q, want a bare DELETE", r.reqs)
	}
}

func TestUsageExitsTwo(t *testing.T) {
	useServer(t, "http://127.0.0.1:1")
	for _, args := range [][]string{
		{"bogus"},
		{"tile", "bogus"},
		{"tile", "set", "--nope"},
		{"key", "add"},
	} {
		if code, _, _ := cli(t, args...); code != 2 {
			t.Errorf("%v = %d, want 2", args, code)
		}
	}
	if code, out, _ := cli(t); code != 0 || !strings.Contains(out, "Usage:") {
		t.Errorf("bare stackr = %d, want help and 0", code)
	}
	code, _, errw := cli(t, "--json", "bogus")
	var e map[string]any
	if json.Unmarshal([]byte(errw), &e) != nil || code != 2 || e["code"] != float64(2) {
		t.Errorf("--json error = %q", errw)
	}
}

// End to end against a real server: log in with a key, deploy, follow
// the job's events over real HTTP to its end.
func TestDeployFollows(t *testing.T) {
	env := servicetest.New(t)
	srv, err := server.New(server.WithDevMode(true), server.WithGzipConfig(api.Gzip))
	if err != nil {
		t.Fatal(err)
	}
	api.RegisterRoutes(srv, &api.Deps{Orch: env.Orch, Access: middleware.NewAccess(env.Orch), DevMode: true})
	ts := httptest.NewServer(srv.Echo())
	t.Cleanup(ts.Close)
	org := env.Org(t, "acme")
	u := env.User(t, "o@x", false)
	env.Member(t, org, u, "owner")
	key := env.APIKey(t, u, org)
	env.Healthy(env.Tile(t, org).Env)
	t.Setenv("STACKR_CONFIG", filepath.Join(t.TempDir(), "config.json"))

	if code, _, errw := cli(t, "login", ts.URL, "--with-key", key); code != 0 {
		t.Fatalf("login = %d %s", code, errw)
	}
	code, out, errw := cli(t, "deploy", "api", "--stack", "shop", "--env", "dev")
	if code != 0 || !strings.Contains(out, "deploy: done") {
		t.Fatalf("deploy = %d\n%s\n%s", code, out, errw)
	}
	code, out, _ = cli(t, "--json", "tile", "ls", "--stack", "shop", "--env", "dev")
	if code != 0 || !strings.Contains(out, `"slug": "api"`) {
		t.Errorf("tile ls --json = %d %s", code, out)
	}
	if code, _, errw := cli(t, "rollback", "--stack", "shop", "--env", "dev"); code != 2 ||
		!strings.Contains(errw, "--tag is required") {
		t.Errorf("rollback without --tag = %d %s", code, errw)
	}
}

// Step 3b: the run verbs reach their routes.
func TestRunVerbsRoutes(t *testing.T) {
	r := &recorder{}
	r.serve(t)
	at := []string{"--stack", "shop", "--env", "dev"}
	const p = "/api/v1/orgs/acme/stacks/shop/envs/dev/tiles/nightly"
	for args, want := range map[string]string{
		"pause":         "POST " + p + `/pause {"paused":true}`,
		"resume":        "POST " + p + `/pause {"paused":false}`,
		"runs --run r1": "GET " + p + "/runs/r1 ",
		"stop --run r1": "DELETE " + p + "/runs/r1 ",
		"run":           "POST " + p + "/run ",
	} {
		r.reqs = nil
		f := strings.Fields(args)
		cli(t, append(append([]string{"tile", f[0], "nightly"}, f[1:]...), at...)...)
		if len(r.reqs) != 1 || r.reqs[0] != want {
			t.Errorf("tile %s sent %q, want %q", args, r.reqs, want)
		}
	}
}

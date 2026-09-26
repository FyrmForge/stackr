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
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/FyrmForge/hamr/pkg/server"
	"github.com/spf13/cobra"

	"github.com/FyrmForge/stackr/internal/api"
	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
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
	r.reqs = nil
	if code, _, errw := cli(t, "tile", "rm", "api", "--stack", "shop", "--env", "dev", "-y", "--no-wait"); code != 0 {
		t.Fatalf("tile rm -y = %d %s", code, errw)
	}
	if len(r.reqs) != 1 || r.reqs[0] != "DELETE /api/v1/orgs/acme/stacks/shop/envs/dev/tiles/api " {
		t.Errorf("tile rm sent %q, want a bare DELETE of the tile", r.reqs)
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

// DECIDE 159: the first arg is the tile only where the verb's Use puts it
// there; "domain add <host>" takes --tile.
func TestTileArgOnlyWhereUseNamesIt(t *testing.T) {
	r := &recorder{}
	r.serve(t)
	at := []string{"--stack", "shop", "--env", "dev", "--tile", "api", "-y"}
	const p = "/api/v1/orgs/acme/stacks/shop/envs/dev/tiles/"
	for args, want := range map[string]string{
		"domain add a.example.com":           "POST " + p + "api/domains ",
		"domain caddy a.example.com":         "GET " + p + "api/domains ",
		"domain rm a.example.com":            "GET " + p + "api/domains ",
		"get web":                            "GET " + p + "web ",
		"rename web www":                     "PUT " + p + "web/name ",
		"domain ls web":                      "GET " + p + "web/domains ",
		"set web --port 8080":                "PATCH " + p + "web ",
		"domain set a.example.com --path /x": "GET " + p + "api/domains ",
		"allow --set acme:*":                 "PUT " + p + "api/allow ",
		"access --slice db --access read":    "PUT " + p + "api/slice-access ",
		"env-pairs pg dev=dev":               "PUT " + p + "pg/env-pairs ",
	} {
		r.reqs = nil
		cli(t, append(append([]string{"tile"}, strings.Fields(args)...), at...)...)
		if len(r.reqs) == 0 || !strings.HasPrefix(r.reqs[0], want) {
			t.Errorf("tile %s sent %q, want first %q", args, r.reqs, want)
		}
	}
}

// Step 7a: the domain noun reaches the org, stack and admin routes; rm
// lists first, then deletes.
func TestDomainResourceRoutes(t *testing.T) {
	r := &recorder{}
	r.serve(t)
	const org = "/api/v1/orgs/acme"
	stackBody := `{"acme_email":"","host":"shop.io","include_env_on_default":false}`
	for _, c := range []struct{ args, want string }{
		{
			"ls",
			"GET /api/v1/admin/domain-resources ",
		},
		{
			"ls --org acme",
			"GET " + org + "/domain-resources ",
		},
		{
			"add --level instance --host example.com",
			`POST /api/v1/admin/domain-resources {"acme_email":"","host":"example.com","include_env_on_default":false}`,
		},
		{
			"add --level org --owner acme --host acme.io --include-env --acme-email ops@acme.io",
			"POST " + org + `/domain-resources {"acme_email":"ops@acme.io","host":"acme.io","include_env_on_default":true}`,
		},
		{
			"add --level stack --owner acme/shop --host shop.io",
			"POST " + org + "/stacks/shop/domain-resources " + stackBody,
		},
		{
			"add --level stack --owner shop --host shop.io",
			"POST " + org + "/stacks/shop/domain-resources " + stackBody,
		},
		{
			"rm r1 --org acme -y",
			"DELETE " + org + "/domain-resources/r1 ",
		},
		{
			"rm r1 -y",
			"DELETE /api/v1/admin/domain-resources/r1 ",
		},
	} {
		r.reqs = nil
		if code, _, errw := cli(t, append([]string{"domain"}, strings.Fields(c.args)...)...); code != 0 {
			t.Errorf("domain %s = %d %s", c.args, code, errw)
			continue
		}
		if len(r.reqs) == 0 || r.reqs[len(r.reqs)-1] != c.want {
			t.Errorf("domain %s sent %q, want last %q", c.args, r.reqs, c.want)
		}
	}
	r.reqs = nil
	if code, _, _ := cli(t, "domain", "add", "--level", "env", "--host", "x.io"); code != 2 || len(r.reqs) != 0 {
		t.Errorf("--level env = %d, sent %q; want exit 2 and no request", code, r.reqs)
	}
}

// domain ls: one row per resource, the owner whichever id is set; the
// header on a terminal only, like every table.
func TestDomainLsTable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[
			{"id":"r1","level":"instance","org_id":null,"stack_id":null,"host":"example.com",
			 "include_env_on_default":false,"acme_email":"","declared":false},
			{"id":"r2","level":"stack","org_id":null,"stack_id":"s1","host":"shop.io",
			 "include_env_on_default":true,"acme_email":"ops@shop.io","declared":true}
		]`)
	}))
	t.Cleanup(srv.Close)
	useServer(t, srv.URL)
	header := []string{
		"ID",
		"LEVEL",
		"OWNER",
		"HOST",
		"INCLUDE-ENV",
		"ACME",
		"DECLARED",
	}
	rows := [][]string{
		{
			"r1",
			"instance",
			"(none)",
			"example.com",
			"false",
			"(none)",
			"false",
		},
		{
			"r2",
			"stack",
			"s1",
			"shop.io",
			"true",
			"ops@shop.io",
			"true",
		},
	}
	args := []string{
		"domain",
		"ls",
		"--org",
		"acme",
	}
	for _, tty := range []bool{true, false} {
		var out, errw bytes.Buffer
		code := run(context.Background(), args, strings.NewReader(""), &out, &errw, tty)
		if code != 0 {
			t.Fatalf("domain ls (tty %v) = %d %s", tty, code, errw.String())
		}
		want := rows
		if tty {
			want = append([][]string{header}, rows...)
		}
		lines := strings.Split(strings.TrimSpace(out.String()), "\n")
		if len(lines) != len(want) {
			t.Fatalf("domain ls (tty %v) =\n%s", tty, out.String())
		}
		for i, l := range lines {
			if got := strings.Fields(l); !slices.Equal(got, want[i]) {
				t.Errorf("domain ls (tty %v) line %d = %q, want %q", tty, i, got, want[i])
			}
		}
	}
}

// Step 7b task 6: the managed and slice verbs send the whole list or map;
// a verb given nothing to send fails with no request.
func TestSliceVerbsRoutes(t *testing.T) {
	r := &recorder{}
	r.serve(t)
	at := []string{"--stack", "shop", "--env", "dev"}
	const env = "/api/v1/orgs/acme/stacks/shop/envs/dev"
	for _, c := range []struct {
		args []string
		want string
	}{
		{
			[]string{"tile", "allow", "pg", "--set", "acme:shop:*", "--set", "acme:blog:*:api"},
			"PUT " + env + `/tiles/pg/allow {"allow":["acme:shop:*","acme:blog:*:api"]}`,
		},
		{
			[]string{"tile", "allow", "pg", "--set", ""},
			"PUT " + env + `/tiles/pg/allow {"allow":[]}`,
		},
		{
			[]string{"tile", "env-pairs", "pg", "staging=dev", "prod=prod"},
			"PUT " + env + `/tiles/pg/env-pairs {"env_pairs":{"prod":"prod","staging":"dev"}}`,
		},
		{
			[]string{"tile", "env-pairs", "pg", "--clear"},
			"PUT " + env + `/tiles/pg/env-pairs {"env_pairs":{}}`,
		},
		{
			[]string{"tile", "access", "api", "--slice", "db", "--access", "read"},
			"PUT " + env + `/tiles/api/slice-access {"access":"read","slice":"db"}`,
		},
		{
			[]string{"tile", "access", "api", "--slice", "db", "--rm"},
			"PUT " + env + `/tiles/api/slice-access {"access":"default","slice":"db"}`,
		},
		{
			[]string{"slice", "add", "db", "--from", "infra:dev:pg", "--access", "read"},
			"POST " + env + `/slices {"default_access":"read","name":"db","provision_from":"infra:dev:pg"}`,
		},
		{
			[]string{"slice", "get", "db"},
			"GET " + env + "/tiles/db/slice ",
		},
		{
			[]string{"slice", "bindings", "db"},
			"GET " + env + "/tiles/db/bindings ",
		},
		{
			[]string{"slice", "on-remove", "db", "drop"},
			"PUT " + env + `/tiles/db/on-remove {"on_remove":"drop"}`,
		},
	} {
		r.reqs = nil
		if code, _, errw := cli(t, append(c.args, at...)...); code != 0 {
			t.Errorf("%s = %d %s", c.args, code, errw)
		}
		if len(r.reqs) != 1 || r.reqs[0] != c.want {
			t.Errorf("%s sent %q, want %q", c.args, r.reqs, c.want)
		}
	}
	for _, args := range [][]string{
		{"tile", "allow", "pg"},
		{"tile", "env-pairs", "pg"},
		{"tile", "env-pairs", "pg", "--clear", "dev=dev"},
		{"tile", "env-pairs", "pg", "dev"},
		{"slice", "add", "db"},
		{"tile", "access", "api", "--slice", "db"},
	} {
		r.reqs = nil
		if code, _, _ := cli(t, append(args, at...)...); code == 0 || len(r.reqs) != 0 {
			t.Errorf("%s = %d, sent %q; want an error and no request", args, code, r.reqs)
		}
	}
	r.reqs = nil
	both := []string{"tile", "access", "api", "--slice", "db", "--rm", "--access", "read"}
	if code, _, errw := cli(t, append(both, at...)...); code != 2 || len(r.reqs) != 0 {
		t.Errorf("--rm with --access = %d %q, sent %q; want exit 2 and no request", code, errw, r.reqs)
	}
}

// Against a real server: --add and --rm read the allow list and send it
// whole; a pattern that is not there is a usage error.
func TestAllowEditsTheWholeList(t *testing.T) {
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
	tl := env.Tile(t, org)
	pg, err := env.Orch.CreateManagedTile(context.Background(), service.Tile{EnvironmentID: tl.Env, Name: "pg"}, "postgres")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("STACKR_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	if code, _, errw := cli(t, "login", ts.URL, "--with-key", key); code != 0 {
		t.Fatalf("login = %d %s", code, errw)
	}
	at := []string{"--stack", "shop", "--env", "dev"}
	allow := func() []string {
		t.Helper()
		ms, err := env.Orch.ManagedInstances(context.Background(), tl.Env)
		if err != nil || len(ms) != 1 || ms[0].TileID != pg.ID {
			t.Fatalf("instances = %v, %v", ms, err)
		}
		return ms[0].Allow
	}
	for _, c := range []struct {
		args []string
		want []string
	}{
		{[]string{"--add", "acme:shop:*"}, []string{"acme:shop:*"}},
		{[]string{"--add", "acme:blog:*", "--add", "acme:shop:*"}, []string{"acme:shop:*", "acme:blog:*"}},
		{[]string{"--rm", "acme:shop:*"}, []string{"acme:blog:*"}},
	} {
		if code, _, errw := cli(t, append(append([]string{"tile", "allow", "pg"}, c.args...), at...)...); code != 0 {
			t.Fatalf("allow %s = %d %s", c.args, code, errw)
		}
		if got := allow(); !slices.Equal(got, c.want) {
			t.Errorf("allow %s = %v, want %v", c.args, got, c.want)
		}
	}
	if code, _, errw := cli(t, append([]string{"tile", "allow", "pg", "--rm", "acme:shop:*"}, at...)...); code != 2 ||
		!strings.Contains(errw, "not in the allow list") {
		t.Errorf("rm a missing pattern = %d %s", code, errw)
	}
	code, out, errw := cli(t, append([]string{"slice", "add", "db", "--from", "shop:dev:pg"}, at...)...)
	if code != 0 || !strings.Contains(out, "shop:dev:pg") {
		t.Fatalf("slice add = %d %s %s", code, out, errw)
	}
	if code, _, errw := cli(t, append([]string{"slice", "bindings", "db"}, at...)...); code != 0 || !strings.Contains(errw, "(none)") {
		t.Errorf("slice bindings = %d %s", code, errw)
	}
}

// tile exec prints the output and exits with the X-Exit-Code trailer.
func TestReleaseGetShowsPins(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/releases") {
			_, _ = io.WriteString(w, `[{"id":"r12","number":12}]`)
			return
		}
		_, _ = io.WriteString(w, `{"release":{"id":"r12","number":12,"created_by":"push"},"pins":{"api":{"slug":"api","repo":"postgres:16-alpine","digest":"sha256:ab"},"_config":{"slug":"_config","repo":"https://x/y","branch":"main","commit_sha":"c1"}}}`)
	}))
	t.Cleanup(srv.Close)
	useServer(t, srv.URL)
	code, out, _ := cli(t, "release", "get", "12", "--stack", "shop")
	if code != 0 || !strings.Contains(out, "number\t12") || !strings.Contains(out, "api\tpostgres:16-alpine") {
		t.Errorf("release get = %d %q, want the release and its pins", code, out)
	}
}

func TestEnvTrafficNamesAndRates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[{"from":"a","to":"b","from_name":"api","to_name":"infra/staging/pg-db","bps":695995.3}]`)
	}))
	t.Cleanup(srv.Close)
	useServer(t, srv.URL)
	code, out, _ := cli(t, "env", "traffic", "--stack", "shop", "--env", "dev")
	if code != 0 || !strings.Contains(out, "api\tinfra/staging/pg-db\t679.7 KB/s") {
		t.Errorf("traffic = %d %q, want the names and a rate", code, out)
	}
}

func TestTileExecExitCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Trailer", "X-Exit-Code")
		_, _ = io.WriteString(w, "out\nerr\n")
		w.Header().Set("X-Exit-Code", "3")
	}))
	t.Cleanup(srv.Close)
	useServer(t, srv.URL)
	code, out, errw := cli(t, "tile", "exec", "api", "--stack", "shop", "--env", "dev", "--container", "c1", "--", "sh")
	if code != 3 || out != "out\nerr\n" || errw != "" {
		t.Errorf("exec = %d %q %q, want 3, the output, nothing on stderr", code, out, errw)
	}
}

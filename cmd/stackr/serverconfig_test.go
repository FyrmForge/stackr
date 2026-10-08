package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// The server config verbs send what the API reads.
func TestServerConfigRoutes(t *testing.T) {
	file := filepath.Join(t.TempDir(), "stackr-server.yml")
	if err := os.WriteFile(file, []byte("version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &recorder{}
	r.serve(t)
	const cfg = "/api/v1/admin/config"
	for _, c := range []struct{ args, want string }{
		{"bind", "GET /api/v1/admin/config-repo "},
		{
			"bind --repo acme/server --connector c1 --branch prod --auto",
			`PUT /api/v1/admin/config-repo {"auto":true,"branch":"prod","connector_id":"c1","path":"","repo":"acme/server"}`,
		},
		{"bind --unbind -y", `PUT /api/v1/admin/config-repo {"auto":false,"branch":"","connector_id":"","path":"","repo":""}`},
		{"plan", "POST " + cfg + "/plan "},
		{"preview -f " + file, "POST " + cfg + `/plan-preview {"file":"version: 1\n"}`},
		{"plans", "GET " + cfg + "/plans "},
		{"plan-show p1", "GET " + cfg + "/plans/p1 "},
		{"reject p1", "POST " + cfg + "/plans/p1/reject "},
		{"export", "GET " + cfg + "/export "},
		{"export -o " + file + " --force", "GET " + cfg + "/export "},
	} {
		r.reqs = nil
		if code, _, errw := cli(t, append([]string{"server"}, strings.Fields(c.args)...)...); code != 0 {
			t.Errorf("server %s = %d %s", c.args, code, errw)
			continue
		}
		if len(r.reqs) == 0 || r.reqs[len(r.reqs)-1] != c.want {
			t.Errorf("server %s sent %q, want last %q", c.args, r.reqs, c.want)
		}
	}
	r.reqs = nil
	if code, _, errw := cli(t, "server", "export", "-o", file); code != 1 || len(r.reqs) != 0 ||
		!strings.Contains(errw, "pass --force") {
		t.Errorf("export over a file = %d %q, sent %q; want exit 1 naming --force and no request", code, errw, r.reqs)
	}
	if code, _, _ := cli(t, "server", "bind", "--branch", "x"); code != 2 {
		t.Errorf("bind with a branch and no repo = %d, want a usage error", code)
	}
}

// planServer answers GETs of plan p1 with the given plan JSON and records
// every request; POSTs answer a job-less {}.
type planServer struct {
	mu   sync.Mutex
	reqs []string
	row  string // the plan row as JSON
}

func (p *planServer) serve(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		p.mu.Lock()
		p.reqs = append(p.reqs, req.Method+" "+req.URL.Path+" "+string(b))
		p.mu.Unlock()
		if req.Method == http.MethodGet || strings.HasSuffix(req.URL.Path, "/plan-file") {
			_, _ = io.WriteString(w, p.row)
			return
		}
		_, _ = io.WriteString(w, `{"id":"j1","kind":"apply","state":"queued"}`)
	}))
	t.Cleanup(srv.Close)
	useServer(t, srv.URL)
}

func (p *planServer) last() string {
	if len(p.reqs) == 0 {
		return ""
	}
	return p.reqs[len(p.reqs)-1]
}

const (
	riskyRow = `{"id":"p1","status":"pending","source":"local","plan":"{\"changes\":[` +
		`{\"kind\":\"settings\",\"field\":\"panel_domain\",\"impact\":\"panel moves to new.example.com\"},` +
		`{\"kind\":\"route-delete\",\"tile\":\"old.example.com\",\"key\":\"route:old.example.com\",\"optional\":true}]}"}`
	quietRow = `{"id":"p1","status":"pending","plan":"{\"changes\":[{\"kind\":\"settings\",\"field\":\"x\"}]}"}`
)

// approve: --remove names removal rows, a risky plan gets its own confirm and
// the body says both; a quiet plan sends no body; blocked and unknown ticks
// send nothing.
func TestServerApprove(t *testing.T) {
	ps := &planServer{row: riskyRow}
	ps.serve(t)
	const approve = "POST /api/v1/admin/config/plans/p1/approve "

	if code, _, errw := cli(t, "server", "approve", "p1", "--remove", "route:old.example.com", "--no-wait", "-y"); code != 0 ||
		ps.last() != approve+`{"confirm":true,"ticked":["route:old.example.com"]}` {
		t.Errorf("risky approve = %d %q, sent %q", code, errw, ps.reqs)
	}
	if !strings.Contains(cliErr(t, "server", "plan-show", "p1"), "panel moves to new.example.com") {
		t.Error("the impact line is not printed")
	}
	if !strings.Contains(cliErr(t, "server", "plan-show", "p1"), "--remove route:old.example.com") {
		t.Error("the removal row does not say how to tick it")
	}

	ps.reqs = nil
	if code, _, errw := cli(t, "server", "approve", "p1", "--remove", "route:nope", "-y"); code != 2 ||
		!strings.Contains(errw, "route:nope") || strings.Contains(strings.Join(ps.reqs, "\n"), "POST") {
		t.Errorf("unknown --remove = %d %q, sent %q; want a usage error and no POST", code, errw, ps.reqs)
	}
	ps.reqs = nil
	if code, _, errw := cli(t, "server", "approve", "p1"); code == 0 || !strings.Contains(errw, "--yes") ||
		strings.Contains(strings.Join(ps.reqs, "\n"), "POST") {
		t.Errorf("approve without -y = %d %q, sent %q; want a refusal and no POST", code, errw, ps.reqs)
	}

	ps.row = quietRow
	ps.reqs = nil
	if code, _, errw := cli(t, "server", "approve", "p1", "--no-wait", "-y"); code != 0 || ps.last() != approve {
		t.Errorf("quiet approve = %d %q, sent %q; want no body", code, errw, ps.reqs)
	}

	ps.row = `{"id":"p1","status":"pending","plan":"{\"changes\":[],\"blockers\":[\"dest needs a secret\"]}"}`
	ps.reqs = nil
	if code, _, errw := cli(t, "server", "approve", "p1", "-y"); code == 0 || !strings.Contains(errw, "blocked") ||
		strings.Contains(strings.Join(ps.reqs, "\n"), "POST") {
		t.Errorf("blocked approve = %d %q, sent %q", code, errw, ps.reqs)
	}
}

// apply <file> stores a local plan, shows it, and approves it like approve.
func TestServerApplyFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "stackr-server.yml")
	if err := os.WriteFile(file, []byte("version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ps := &planServer{row: riskyRow}
	ps.serve(t)
	code, _, errw := cli(t, "server", "apply", file, "--remove", "route:old.example.com", "--no-wait", "-y")
	if code != 0 {
		t.Fatalf("apply = %d %s", code, errw)
	}
	want := []string{
		`POST /api/v1/admin/config/plan-file {"file":"version: 1\n"}`,
		`POST /api/v1/admin/config/plans/p1/approve {"confirm":true,"ticked":["route:old.example.com"]}`,
	}
	if strings.Join(ps.reqs, "|") != strings.Join(want, "|") {
		t.Errorf("apply sent %q, want %q", ps.reqs, want)
	}
	if !strings.Contains(errw, "local file, not the repo") {
		t.Errorf("apply does not say the plan is local: %q", errw)
	}

	ps.row = `{"id":"p2","status":"clean","plan":"{}"}`
	ps.reqs = nil
	if code, _, errw := cli(t, "server", "apply", file, "-y"); code != 0 || len(ps.reqs) != 1 ||
		!strings.Contains(errw, "nothing to apply") {
		t.Errorf("apply of a clean plan = %d %q, sent %q; want one request and a note", code, errw, ps.reqs)
	}
}

// The org approve takes --remove and the risky confirm too.
func TestOrgApproveRemoveAndConfirm(t *testing.T) {
	ps := &planServer{row: riskyRow}
	ps.serve(t)
	const approve = "POST /api/v1/orgs/acme/config/plans/p1/approve "
	if code, _, errw := cli(t, "org", "approve", "p1", "--remove", "route:old.example.com", "--no-wait", "-y"); code != 0 ||
		ps.last() != approve+`{"confirm":true,"ticked":["route:old.example.com"]}` {
		t.Errorf("org approve = %d %q, sent %q", code, errw, ps.reqs)
	}
	ps.reqs = nil
	if code, _, _ := cli(t, "org", "approve", "p1", "--remove", "share:x", "-y"); code != 2 ||
		strings.Contains(strings.Join(ps.reqs, "\n"), "POST") {
		t.Errorf("org approve of an unknown --remove = %d, sent %q", code, ps.reqs)
	}
}

func cliErr(t *testing.T, args ...string) string {
	t.Helper()
	_, _, errw := cli(t, args...)
	return errw
}

// A clean file previews as "no changes" on stdout, not as silence; a plan
// with changes or a blocker does not claim it.
func TestServerPreviewSaysNoChanges(t *testing.T) {
	file := filepath.Join(t.TempDir(), "stackr-server.yml")
	if err := os.WriteFile(file, []byte("version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		answer string
		want   bool
	}{
		{`{"changes":[]}`, true},
		{`{"changes":[],"notes":["x"]}`, true},
		{`{"changes":[{"kind":"settings","field":"a","old":"1","new":"2"}]}`, false},
		{`{"changes":[],"blockers":["no"]}`, false},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, c.answer)
		}))
		useServer(t, srv.URL)
		code, out, errw := cli(t, "server", "preview", "-f", file)
		srv.Close()
		if code != 0 || strings.Contains(out, "no changes") != c.want {
			t.Errorf("preview of %s = %d out %q err %q, want no-changes line %v", c.answer, code, out, errw, c.want)
		}
	}
}

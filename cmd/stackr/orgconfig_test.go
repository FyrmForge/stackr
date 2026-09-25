package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The org config verbs send what the API reads.
func TestOrgConfigRoutes(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "stackr-org.yml")
	if err := os.WriteFile(file, []byte("version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &recorder{}
	r.serve(t)
	const cfg = "/api/v1/orgs/acme/config"
	for _, c := range []struct{ args, want string }{
		{
			"config-repo --repo acme/org --connector c1 --auto",
			`PUT /api/v1/orgs/acme/config-repo {"auto":true,"branch":"","connector_id":"c1","path":"","repo":"acme/org"}`,
		},
		{
			"plan",
			"POST " + cfg + "/plan ",
		},
		{
			"preview -f " + file,
			"POST " + cfg + `/plan-preview {"file":"version: 1\n"}`,
		},
		{
			"plans",
			"GET " + cfg + "/plans ",
		},
		{
			"plan-show p1",
			"GET " + cfg + "/plans/p1 ",
		},
		{
			"approve p1 --no-wait",
			"POST " + cfg + "/plans/p1/approve ",
		},
		{
			"reject p1",
			"POST " + cfg + "/plans/p1/reject ",
		},
		{
			"export",
			"GET " + cfg + "/export ",
		},
		{
			"export -o " + file + " --force",
			"GET " + cfg + "/export ",
		},
	} {
		r.reqs = nil
		if code, _, errw := cli(t, append([]string{"org"}, strings.Fields(c.args)...)...); code != 0 {
			t.Errorf("org %s = %d %s", c.args, code, errw)
			continue
		}
		if len(r.reqs) == 0 || r.reqs[len(r.reqs)-1] != c.want {
			t.Errorf("org %s sent %q, want last %q", c.args, r.reqs, c.want)
		}
	}
	r.reqs = nil
	if code, _, errw := cli(t, "org", "export", "-o", file); code != 1 || len(r.reqs) != 0 ||
		!strings.Contains(errw, "pass --force") {
		t.Errorf("export over a file = %d %q, sent %q; want exit 1 naming --force and no request", code, errw, r.reqs)
	}
}

// preview --detailed-exitcode: 0 no changes, 2 changes, 1 blocked, and
// nothing on stderr for the code alone.
func TestOrgPreviewExitCode(t *testing.T) {
	file := filepath.Join(t.TempDir(), "stackr-org.yml")
	if err := os.WriteFile(file, []byte("version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		answer string
		code   int
	}{
		{`{"changes":[]}`, 0},
		{`{"changes":[{"kind":"org","old":"a","new":"b"}]}`, 2},
		{`{"changes":[{"kind":"org"}],"blockers":["no"]}`, 1},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, c.answer)
		}))
		useServer(t, srv.URL)
		code, _, errw := cli(t, "org", "preview", "-f", file, "--detailed-exitcode")
		srv.Close()
		if code != c.code || strings.Contains(errw, "error:") {
			t.Errorf("preview of %s = %d %q, want %d and no error line", c.answer, code, errw, c.code)
		}
	}
}

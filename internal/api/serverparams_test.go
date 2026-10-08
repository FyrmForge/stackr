package api_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
)

// The server params scope is an admin's: an owner is 403 on every route of
// it, an admin lists masked, reads secrets with the stronger verb, sets and
// deletes, and an org never sees the rows.
func TestServerParams(t *testing.T) {
	w := newWorld(t)
	const body = `[{"collection":"s3","name":"region","kind":"param","value":"eu"},` +
		`{"collection":"s3","name":"secret_key","kind":"secret","value":"LEAK-sk"}]`
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/admin/params", ""},
		{"GET", "/admin/params/secrets", ""},
		{"PATCH", "/admin/params", body},
		{"DELETE", "/admin/params/s3/region", ""},
	} {
		if code, _ := w.do(t, w.owner, c.method, c.path, c.body); code != 403 {
			t.Errorf("owner %s %s = %d, want 403", c.method, c.path, code)
		}
	}

	code, out := w.do(t, w.admin, "PATCH", "/admin/params", body)
	if code != 200 || strings.TrimSpace(out) != "[]" {
		t.Fatalf("set = %d %s; no tile is redeployed by a server param", code, out)
	}
	code, out = w.do(t, w.admin, "GET", "/admin/params", "")
	var ps []service.Param
	if code != 200 || json.Unmarshal([]byte(out), &ps) != nil || len(ps) != 2 || strings.Contains(out, "LEAK") {
		t.Errorf("masked list = %d %s", code, out)
	}
	if code, out = w.do(t, w.admin, "GET", "/admin/params/secrets", ""); code != 200 || !strings.Contains(out, "LEAK-sk") {
		t.Errorf("secrets = %d %s", code, out)
	}
	if code, out = w.do(t, w.admin, "PATCH", "/admin/params",
		`[{"collection":"s3","name":"secret_key","kind":"param","value":"open"}]`); code != 409 {
		t.Errorf("declassify = %d %s, want 409", code, out)
	}
	if code, out = w.do(t, w.admin, "PATCH", "/admin/params",
		`[{"collection":"S3","name":"x","kind":"param","value":"v"}]`); code != 400 {
		t.Errorf("bad collection = %d %s, want 400", code, out)
	}

	// no org reads it, by the org route or the stronger one
	for _, p := range []string{"/orgs/acme/params", "/orgs/acme/params/secrets"} {
		if code, out = w.do(t, w.owner, "GET", p, ""); code != 200 || strings.Contains(out, "s3") || strings.Contains(out, "LEAK") {
			t.Errorf("org %s = %d %s; a server row reached an org", p, code, out)
		}
	}

	if code, _ = w.do(t, w.admin, "DELETE", "/admin/params/s3/region", ""); code != 200 {
		t.Errorf("delete = %d", code)
	}
	if code, _ = w.do(t, w.admin, "DELETE", "/admin/params/s3/region", ""); code != 404 {
		t.Errorf("delete again = %d, want 404", code)
	}
}

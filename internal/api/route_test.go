package api_test

import (
	"encoding/json"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
)

// External routes are an admin's: create, list, delete; an owner is 403 on
// all three, and a refusal is a 4xx.
func TestExternalRoutes(t *testing.T) {
	w := newWorld(t)
	const body = `{"host":"pve.example.com","mode":"https","target":"10.0.0.5:8006","insecure":true}`
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/admin/routes", ""},
		{"POST", "/admin/routes", body},
		{"DELETE", "/admin/routes/x", ""},
	} {
		if code, _ := w.do(t, w.owner, c.method, c.path, c.body); code != 403 {
			t.Errorf("owner %s %s = %d, want 403", c.method, c.path, code)
		}
	}
	code, out := w.do(t, w.admin, "POST", "/admin/routes", body)
	var r service.ExternalRoute
	if code != 201 || json.Unmarshal([]byte(out), &r) != nil || r.Host != "pve.example.com" || !r.Insecure {
		t.Fatalf("create = %d %s", code, out)
	}
	if code, _ := w.do(t, w.admin, "POST", "/admin/routes", body); code != 409 {
		t.Errorf("duplicate = %d, want 409", code)
	}
	if code, _ := w.do(t, w.admin, "POST", "/admin/routes", `{"host":"a.io","mode":"tcp","target":"x"}`); code != 400 {
		t.Errorf("bad mode = %d, want 400", code)
	}
	code, out = w.do(t, w.admin, "GET", "/admin/routes", "")
	var rs []service.ExternalRoute
	if code != 200 || json.Unmarshal([]byte(out), &rs) != nil || len(rs) != 1 || rs[0].ID != r.ID {
		t.Errorf("list = %d %s", code, out)
	}
	if code, _ := w.do(t, w.admin, "DELETE", "/admin/routes/"+r.ID, ""); code != 204 && code != 200 {
		t.Errorf("delete = %d", code)
	}
	if code, _ := w.do(t, w.admin, "DELETE", "/admin/routes/"+r.ID, ""); code != 404 {
		t.Errorf("delete again = %d, want 404", code)
	}
}

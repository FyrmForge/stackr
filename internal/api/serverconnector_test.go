package api_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
)

// The server connector routes are an admin's: an owner is 403 on every one;
// an admin lists (no config), begins, renames, shares and deletes, and an
// org key never reaches a server connector through the org routes.
func TestServerConnectorRoutes(t *testing.T) {
	w := newWorld(t)
	srv := w.env.ServerConnector(t, "main", "sec")
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/admin/connectors", ""},
		{"POST", "/admin/connectors", `{"github_org":""}`},
		{"PUT", "/admin/connectors/" + srv + "/name", `{"name":"x"}`},
		{"PUT", "/admin/connectors/" + srv + "/shares", `{"all":true}`},
		{"DELETE", "/admin/connectors/" + srv, ""},
	} {
		if code, _ := w.do(t, w.owner, c.method, c.path, c.body); code != 403 {
			t.Errorf("owner %s %s = %d, want 403", c.method, c.path, code)
		}
	}

	code, out := w.do(t, w.admin, "GET", "/admin/connectors", "")
	var cs []service.ServerConnector
	if code != 200 || json.Unmarshal([]byte(out), &cs) != nil || len(cs) != 1 || strings.Contains(out, "sec") ||
		!strings.Contains(out, `"org_ids":[]`) {
		t.Fatalf("list = %d %s", code, out)
	}

	body := `{"org_ids":["` + w.acme + `"],"all":false}`
	if code, out = w.do(t, w.admin, "PUT", "/admin/connectors/"+srv+"/shares", body); code != 200 ||
		!strings.Contains(out, w.acme) {
		t.Fatalf("share = %d %s", code, out)
	}
	if code, _ = w.do(t, w.admin, "PUT", "/admin/connectors/"+srv+"/shares", `{"org_ids":["nope"]}`); code != 400 && code != 422 {
		t.Errorf("share with an unknown org = %d, want 4xx", code)
	}
	if code, out = w.do(t, w.admin, "PUT", "/admin/connectors/"+srv+"/name", `{"name":"primary"}`); code != 200 ||
		!strings.Contains(out, "primary") {
		t.Errorf("rename = %d %s", code, out)
	}

	// the org's list shows the shared row, marked; its writes cannot reach it
	if code, out = w.do(t, w.owner, "GET", "/orgs/acme/connectors", ""); code != 200 ||
		!strings.Contains(out, srv) || !strings.Contains(out, `"shared":true`) {
		t.Errorf("org list = %d %s, want the shared connector", code, out)
	}
	if code, _ = w.do(t, w.owner, "DELETE", "/orgs/acme/connectors/"+srv, ""); code != 404 {
		t.Errorf("org delete of a server connector = %d, want 404", code)
	}
	if code, _ = w.do(t, w.owner, "PUT", "/orgs/acme/connectors/"+srv+"/name", `{"name":"mine"}`); code != 404 {
		t.Errorf("org rename of a server connector = %d, want 404", code)
	}

	code, out = w.do(t, w.admin, "POST", "/admin/connectors", `{"github_org":"acme-inc"}`)
	if code != 201 || !strings.Contains(out, `"manifest"`) || !strings.Contains(out, "github.com") {
		t.Fatalf("begin = %d %s", code, out)
	}
	if code, _ = w.do(t, w.admin, "DELETE", "/admin/connectors/"+srv, ""); code != 204 {
		t.Errorf("delete = %d, want 204", code)
	}
	if code, _ = w.do(t, w.admin, "DELETE", "/admin/connectors/"+srv, ""); code != 404 {
		t.Errorf("second delete = %d, want 404", code)
	}
}

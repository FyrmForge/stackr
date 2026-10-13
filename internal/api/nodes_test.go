package api_test

import (
	"encoding/json"
	"strings"
	"testing"
)

// The node writes are for server admins; the reads are open to any member
// and carry no addresses; the placement routes are for org owners.
func TestNodeAPI(t *testing.T) {
	w := newWorld(t)
	for _, p := range []struct{ method, path, body string }{
		{"PUT", "/admin/nodes/x/tags", `{"add":["egress"]}`},
		{"POST", "/admin/nodes/x/orgs", `{"org":"acme"}`},
		{"DELETE", "/admin/nodes/x/orgs/acme", ""},
		{"POST", "/admin/nodes/x/rejoin", ""},
		{"POST", "/admin/nodes/x/forget", ""},
		{"DELETE", "/admin/nodes/x", ""},
	} {
		if code, _ := w.do(t, "", p.method, p.path, p.body); code != 401 {
			t.Errorf("%s %s anonymous = %d, want 401", p.method, p.path, code)
		}
		if code, _ := w.do(t, w.owner, p.method, p.path, p.body); code != 403 {
			t.Errorf("%s %s owner = %d, want 403", p.method, p.path, code)
		}
	}
	for _, path := range []string{"/nodes", "/cluster", "/orgs/acme/lost-envs"} {
		if code, _ := w.do(t, "", "GET", path, ""); code != 401 {
			t.Errorf("GET %s anonymous = %d, want 401", path, code)
		}
		if code, body := w.do(t, w.owner, "GET", path, ""); code != 200 {
			t.Errorf("GET %s owner = %d %s", path, code, body)
		}
	}
	// a stranger's org is not the owner's: placement routes stay in the org
	if code, _ := w.do(t, w.stranger, "PUT", "/orgs/acme/pin", `{"node":""}`); code != 404 {
		t.Errorf("stranger pin = %d, want 404", code)
	}

	// the controller is the one node a fresh install has
	code, body := w.do(t, w.owner, "GET", "/nodes", "")
	var ns []struct {
		Name, Role, Range string
	}
	if err := json.Unmarshal([]byte(body), &ns); err != nil || code != 200 || len(ns) != 1 || ns[0].Role != "controller" || ns[0].Range != "" {
		t.Fatalf("nodes for a member = %d %s (%v): want the controller, no range", code, body, err)
	}
	if _, body = w.do(t, w.admin, "GET", "/nodes/"+ns[0].Name, ""); !strings.Contains(body, `"wg_address":"10.207.255.6"`) {
		t.Errorf("admin sees the address: %s", body)
	}

	// an owner pins the org to the controller and reads it back; an env is not pinned
	if code, body = w.do(t, w.owner, "PUT", "/orgs/acme/pin", `{"node":"`+ns[0].Name+`"}`); code != 200 || !strings.Contains(body, `"node":"`+ns[0].Name+`"`) {
		t.Errorf("pin = %d %s", code, body)
	}
	if code, body = w.do(t, w.owner, "GET", "/orgs/acme/pin", ""); code != 200 || !strings.Contains(body, `"scope":"acme"`) {
		t.Errorf("get pin = %d %s", code, body)
	}
	if code, _ = w.do(t, w.owner, "PUT", "/orgs/acme/ingress", `{"node":"`+ns[0].Name+`"}`); code != 200 {
		t.Errorf("ingress set = %d", code)
	}
	if code, body = w.do(t, w.owner, "GET", "/orgs/acme/exits", ""); code != 200 || !strings.Contains(body, `"exits":[]`) {
		t.Errorf("exits = %d %s", code, body)
	}
	// a viewer-level read of the same routes is enough, a write is not
	if code, _ = w.do(t, "", "PUT", "/orgs/acme/exits", `{"nodes":[]}`); code != 401 {
		t.Errorf("anonymous exits = %d, want 401", code)
	}
}

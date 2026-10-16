package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/errs"
)

// Nodes are hardware for server admins: every node route, the cluster map
// and the node list are admin-only. Members work with placements; the pin,
// ingress and exit routes are for org owners and name placements.
func TestNodeAPI(t *testing.T) {
	w := newWorld(t)
	for _, p := range []struct{ method, path, body string }{
		{"PUT", "/admin/nodes/x/tags", `{"add":["egress"]}`},
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
	for _, path := range []string{"/nodes", "/nodes/x", "/cluster", "/admin/node-defaults"} {
		if code, _ := w.do(t, "", "GET", path, ""); code != 401 {
			t.Errorf("GET %s anonymous = %d, want 401", path, code)
		}
		if code, body := w.do(t, w.owner, "GET", path, ""); code != 403 || strings.Contains(body, "wg_address") {
			t.Errorf("GET %s owner = %d %s, want 403", path, code, body)
		}
	}
	if code, body := w.do(t, w.owner, "GET", "/orgs/acme/lost-envs", ""); code != 200 {
		t.Errorf("lost-envs owner = %d %s", code, body)
	}
	// the removed access routes are gone
	for _, p := range []struct{ method, path string }{
		{"POST", "/admin/nodes/x/orgs"}, {"DELETE", "/admin/nodes/x/orgs/acme"}, {"PUT", "/admin/nodes/x/open-to-all"},
		{"POST", "/orgs/acme/stacks/shop/envs/dev/move"},
	} {
		if code, _ := w.do(t, w.admin, p.method, p.path, "{}"); code != 404 && code != 405 {
			t.Errorf("%s %s = %d, want it gone", p.method, p.path, code)
		}
	}
	// a stranger's org is not the owner's: placement routes stay in the org
	if code, _ := w.do(t, w.stranger, "PUT", "/orgs/acme/pin", `{"placement":""}`); code != 404 {
		t.Errorf("stranger pin = %d, want 404", code)
	}

	// the controller is the one node a fresh install has; only an admin sees it
	code, body := w.do(t, w.admin, "GET", "/nodes", "")
	var ns []struct {
		Name, Role, Range string
	}
	if err := json.Unmarshal([]byte(body), &ns); err != nil || code != 200 || len(ns) != 1 || ns[0].Role != "controller" {
		t.Fatalf("nodes for an admin = %d %s (%v): want the controller", code, body, err)
	}
	if _, body = w.do(t, w.admin, "GET", "/nodes/"+ns[0].Name, ""); !strings.Contains(body, `"wg_address":"10.207.255.6"`) {
		t.Errorf("admin sees the address: %s", body)
	}

	// an owner pins the org to the default placement and reads it back (the
	// verbs read placement names once lane A is merged; until then they stub)
	if _, err := w.env.Orch.Placements(context.Background()); errors.Is(err, errs.ErrNotBuilt) {
		return
	}
	if code, body = w.do(t, w.owner, "PUT", "/orgs/acme/pin", `{"placement":"default"}`); code != 200 {
		t.Errorf("pin = %d %s", code, body)
	}
	if code, body = w.do(t, w.owner, "GET", "/orgs/acme/pin", ""); code != 200 || !strings.Contains(body, `"scope":"acme"`) {
		t.Errorf("get pin = %d %s", code, body)
	}
	if code, _ = w.do(t, w.owner, "PUT", "/orgs/acme/ingress", `{"placement":"default"}`); code != 200 {
		t.Errorf("ingress set = %d", code)
	}
	if code, body = w.do(t, w.owner, "GET", "/orgs/acme/exits", ""); code != 200 || !strings.Contains(body, `"exits":[]`) {
		t.Errorf("exits = %d %s", code, body)
	}
	// a viewer-level read of the same routes is enough, a write is not
	if code, _ = w.do(t, "", "PUT", "/orgs/acme/exits", `{"placements":[]}`); code != 401 {
		t.Errorf("anonymous exits = %d, want 401", code)
	}
}

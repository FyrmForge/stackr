package api_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/errs"
)

// The admin placement routes are for server admins; the dev list is for any
// member of the org.
func TestPlacementRoutesGated(t *testing.T) {
	w := newWorld(t)
	for _, p := range []struct{ method, path, body string }{
		{"GET", "/admin/placements", ""},
		{"POST", "/admin/placements", `{"name":"eu-prod"}`},
		{"PUT", "/admin/placements/x/open", `{"all":true}`},
		{"DELETE", "/admin/placements/x", ""},
		{"GET", "/admin/placements/x/move-plan?to=n", ""},
		{"POST", "/admin/placements/x/move", `{"to":"n","confirm":"x"}`},
		{"POST", "/admin/placements/x/retry", ""},
		{"GET", "/admin/placements/x/move", ""},
	} {
		if code, _ := w.do(t, "", p.method, p.path, p.body); code != 401 {
			t.Errorf("%s %s anonymous = %d, want 401", p.method, p.path, code)
		}
		if code, _ := w.do(t, w.owner, p.method, p.path, p.body); code != 403 {
			t.Errorf("%s %s owner = %d, want 403", p.method, p.path, code)
		}
	}
	if code, _ := w.do(t, "", "GET", "/orgs/acme/placements", ""); code != 401 {
		t.Errorf("dev list anonymous = %d, want 401", code)
	}
	if code, _ := w.do(t, w.stranger, "GET", "/orgs/acme/placements", ""); code != 404 {
		t.Errorf("dev list stranger = %d, want 404", code)
	}
	if code, _ := w.do(t, w.stranger, "PUT", "/orgs/acme/stacks/shop/envs/dev/pin", `{"placement":""}`); code != 404 {
		t.Errorf("env pin stranger = %d, want 404", code)
	}
}

// A round trip per route. It needs lane A's placements and lane M's moves;
// until they are merged the service answers ErrNotBuilt and this skips.
func TestPlacementsAPI(t *testing.T) {
	w := newWorld(t)
	if _, err := w.env.Orch.Placements(context.Background()); errors.Is(err, errs.ErrNotBuilt) {
		t.Skip("placements are not built in this tree yet")
	}
	// a fresh install has the default placement on the controller
	code, body := w.do(t, w.admin, "GET", "/admin/placements", "")
	if code != 200 || !strings.Contains(body, `"name":"default"`) || !strings.Contains(body, `"default":true`) {
		t.Fatalf("admin list = %d %s", code, body)
	}
	if code, body = w.do(t, w.admin, "POST", "/admin/placements", `{"name":"eu-db","orgs":["acme"]}`); code != 201 ||
		!strings.Contains(body, `"name":"eu-db"`) || !strings.Contains(body, `"node":""`) {
		t.Fatalf("create = %d %s", code, body)
	}
	if code, body = w.do(t, w.admin, "PUT", "/admin/placements/eu-db/open", `{"all":false,"orgs":["acme"]}`); code != 204 {
		t.Fatalf("open = %d %s", code, body)
	}

	// the dev list: no node name, whoever asks
	code, body = w.do(t, w.owner, "GET", "/orgs/acme/placements", "")
	if code != 200 || !strings.Contains(body, `"name":"eu-db"`) || !strings.Contains(body, `"has_node":false`) ||
		strings.Contains(body, `"node"`) || strings.Contains(body, "wg_address") {
		t.Errorf("dev list = %d %s", code, body)
	}

	// a pin names a placement; one the org cannot use is refused
	if code, body = w.do(t, w.owner, "PUT", "/orgs/acme/pin", `{"placement":"eu-db"}`); code != 200 {
		t.Errorf("pin = %d %s", code, body)
	}
	if code, body = w.do(t, w.owner, "GET", "/orgs/acme/pin", ""); code != 200 || !strings.Contains(body, "eu-db") {
		t.Errorf("get pin = %d %s", code, body)
	}
	if code, _ = w.do(t, w.owner, "PUT", "/orgs/acme/pin", `{"placement":"nope"}`); code != 404 && code != 422 && code != 409 {
		t.Errorf("unknown pin = %d", code)
	}

	// the move plan and status of a placement with no move
	if code, body = w.do(t, w.admin, "GET", "/admin/placements/eu-db/move-plan?to=nope", ""); code == 200 && !strings.Contains(body, `"checks"`) {
		t.Errorf("move plan = %d %s", code, body)
	}
	if code, _ = w.do(t, w.admin, "GET", "/admin/placements/eu-db/move", ""); code != 404 {
		t.Errorf("move status without a move = %d, want 404", code)
	}
	// the confirm must be the placement's name
	if code, _ = w.do(t, w.admin, "POST", "/admin/placements/eu-db/move", `{"to":"nope","confirm":"wrong"}`); code < 400 {
		t.Errorf("move without the name = %d", code)
	}

	if code, body = w.do(t, w.admin, "DELETE", "/admin/placements/default", ""); code < 400 {
		t.Errorf("delete default = %d %s, want a refusal", code, body)
	}
	if code, body = w.do(t, w.admin, "DELETE", "/admin/placements/eu-db", ""); code != 409 && code != 204 {
		t.Errorf("delete = %d %s", code, body)
	}
}

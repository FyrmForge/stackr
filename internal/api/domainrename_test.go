package api_test

import (
	"encoding/json"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
)

// Rename: an owner renames the org's row and no org route reaches the
// instance's; an admin renames either; a member renames nothing.
func TestDomainResourceRename(t *testing.T) {
	w := newWorld(t)
	u := w.env.User(t, "member@x", false)
	w.env.Member(t, w.acme, u, "member")
	member := w.env.APIKey(t, u, w.acme)
	create := func(key, path, body string) service.DomainResource {
		t.Helper()
		code, out := w.do(t, key, "POST", path, body)
		if code != 201 {
			t.Fatalf("POST %s = %d %s", path, code, out)
		}
		var r service.DomainResource
		if err := json.Unmarshal([]byte(out), &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	inst := create(w.admin, "/admin/domain-resources", `{"host":"example.com"}`)
	og := create(w.owner, "/orgs/acme/domain-resources", `{"host":"acme.io"}`)
	st := create(w.owner, "/orgs/acme/stacks/shop/domain-resources", `{"host":"shop.io"}`)
	rename := func(key, path, host string) (int, service.DomainResource) {
		t.Helper()
		code, out := w.do(t, key, "POST", path, `{"host":"`+host+`"}`)
		var r service.DomainResource
		_ = json.Unmarshal([]byte(out), &r)
		return code, r
	}

	if code, _ := rename(member, "/orgs/acme/domain-resources/"+og.ID+"/rename", "acme.dev"); code != 403 {
		t.Errorf("member rename = %d, want 403", code)
	}
	if code, _ := rename(w.owner, "/orgs/acme/domain-resources/"+inst.ID+"/rename", "other.dev"); code != 404 {
		t.Errorf("owner renames the instance row through the org = %d, want 404", code)
	}
	if code, _ := rename(w.stranger, "/orgs/other/domain-resources/"+og.ID+"/rename", "other.dev"); code != 404 {
		t.Errorf("stranger renames acme's row through their org = %d, want 404", code)
	}
	if code, _ := rename(w.owner, "/admin/domain-resources/"+og.ID+"/rename", "acme.dev"); code != 403 {
		t.Errorf("owner on the admin route = %d, want 403", code)
	}
	if code, _ := rename(w.owner, "/orgs/acme/domain-resources/"+st.ID+"/rename", "shop.dev"); code != 400 {
		t.Errorf("stack row rename = %d, want 400 (its stack file names it)", code)
	}
	if code, _ := rename(w.owner, "/orgs/acme/domain-resources/"+og.ID+"/rename", "example.com"); code != 409 {
		t.Errorf("rename onto the instance host = %d, want 409", code)
	}
	code, r := rename(w.owner, "/orgs/acme/domain-resources/"+og.ID+"/rename", "acme.dev")
	if code != 200 || r.Host != "acme.dev" || r.ID != og.ID {
		t.Errorf("owner rename = %d %+v, want 200 on acme.dev", code, r)
	}
	code, r = rename(w.admin, "/admin/domain-resources/"+inst.ID+"/rename", "main.dev")
	if code != 200 || r.Host != "main.dev" {
		t.Errorf("admin rename = %d %+v, want 200 on main.dev", code, r)
	}
}

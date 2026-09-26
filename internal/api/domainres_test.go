package api_test

import (
	"encoding/json"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
)

// Domain resources: an owner keeps the org's rows and its stacks'; the
// instance's are an admin's, and no org route reaches one.
func TestDomainResources(t *testing.T) {
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
	hosts := func(key, path string) []string {
		t.Helper()
		code, out := w.do(t, key, "GET", path, "")
		if code != 200 {
			t.Fatalf("GET %s = %d %s", path, code, out)
		}
		var rs []service.DomainResource
		if err := json.Unmarshal([]byte(out), &rs); err != nil {
			t.Fatal(err)
		}
		var hs []string
		for _, r := range rs {
			hs = append(hs, r.Host)
		}
		return hs
	}

	if code, _ := w.do(t, w.owner, "POST", "/admin/domain-resources", `{"host":"example.com"}`); code != 403 {
		t.Errorf("owner instance create = %d, want 403", code)
	}
	inst := create(w.admin, "/admin/domain-resources", `{"host":"example.com"}`)
	if code, _ := w.do(t, member, "POST", "/orgs/acme/domain-resources", `{"host":"acme.io"}`); code != 403 {
		t.Errorf("member create = %d, want 403", code)
	}
	og := create(w.owner, "/orgs/acme/domain-resources", `{"host":"acme.io","acme_email":"ops@acme.io"}`)
	st := create(w.owner, "/orgs/acme/stacks/shop/domain-resources", `{"host":"shop.io"}`)
	if og.Level != "org" || st.Level != "stack" || inst.Level != "instance" {
		t.Errorf("levels = %s %s %s", og.Level, st.Level, inst.Level)
	}

	if got := hosts(w.owner, "/orgs/acme/domain-resources"); len(got) != 2 || got[0] != "acme.io" || got[1] != "example.com" {
		t.Errorf("org list = %v, want the org's row then the instance's", got)
	}
	if code, _ := w.do(t, member, "GET", "/orgs/acme/domain-resources", ""); code != 403 {
		t.Errorf("member list = %d, want 403", code)
	}
	if got := hosts(w.admin, "/admin/domain-resources"); len(got) != 3 {
		t.Errorf("admin list = %v, want all three", got)
	}

	code, out := w.do(t, w.owner, "PATCH", "/orgs/acme/domain-resources/"+og.ID, `{"include_env_on_default":true}`)
	var up service.DomainResource
	err := json.Unmarshal([]byte(out), &up)
	if code != 200 || err != nil || !up.IncludeEnvOnDefault || up.ACMEEmail != "" {
		t.Errorf("update = %d %s", code, out)
	}

	if code, _ := w.do(t, w.owner, "DELETE", "/orgs/acme/domain-resources/"+inst.ID, ""); code != 404 {
		t.Errorf("owner deletes the instance row through the org = %d, want 404", code)
	}
	if code, _ := w.do(t, w.stranger, "DELETE", "/orgs/other/domain-resources/"+og.ID, ""); code != 404 {
		t.Errorf("stranger deletes acme's row through their org = %d, want 404", code)
	}
	for _, id := range []string{og.ID, st.ID} {
		if code, out := w.do(t, w.owner, "DELETE", "/orgs/acme/domain-resources/"+id, ""); code != 204 {
			t.Errorf("owner delete %s = %d %s", id, code, out)
		}
	}
	if code, out := w.do(t, w.admin, "DELETE", "/admin/domain-resources/"+inst.ID, ""); code != 204 {
		t.Errorf("admin delete = %d %s", code, out)
	}
}

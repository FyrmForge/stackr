package api_test

import (
	"context"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
)

const apiKeySentinel = "nc-key-never-shown-77"

// An org's DNS connectors are its owners': a member is 403, another org
// 404, and the key is in no answer. Server DNS connectors are an admin's.
func TestDNSConnectorRoutes(t *testing.T) {
	w := newWorld(t)
	w.env.DNS.Zones = []service.DNSZone{{Name: "acme.dev", OurDNS: true}, {Name: "away.com"}}
	// a connector is added in the panel; the API only reaches it
	got, err := w.env.Orch.AddConnector(context.Background(), service.ConnectorAdd{
		OrgID: &w.acme, Kind: "namecheap",
		Values: map[string]string{"user": "jo", "key": apiKeySentinel, "client_ip": "203.0.113.7"},
	})
	if err != nil || len(got.Domains) != 2 {
		t.Fatalf("add = %v %v", got, err)
	}
	id := got.Connector.ID
	base := "/orgs/acme/dns-connectors/" + id

	m := w.env.User(t, "member@x", false)
	w.env.Member(t, w.acme, m, "member")
	member := w.env.APIKey(t, m, w.acme)
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/orgs/acme/dns-connectors", ""},
		{"GET", base + "/domains", ""},
		{"PUT", base + "/domains/acme.dev", ""},
		{"DELETE", base, ""},
		{"GET", base + "/domains/acme.dev/plan", ""},
	} {
		if code, _ := w.do(t, member, c.method, c.path, c.body); code != 403 {
			t.Errorf("member %s %s = %d, want 403", c.method, c.path, code)
		}
		if code, _ := w.do(t, w.stranger, c.method, c.path, c.body); code != 404 {
			t.Errorf("stranger %s %s = %d, want 404", c.method, c.path, code)
		}
	}
	// the members' connector list never shows it either
	if code, out := w.do(t, member, "GET", "/orgs/acme/connectors", ""); code != 200 || strings.Contains(out, id) {
		t.Errorf("member git list = %d %s", code, out)
	}
	// another org's owner reaching it through its own org's path
	if code, _ := w.do(t, w.stranger, "GET", "/orgs/other/dns-connectors/"+id+"/domains", ""); code != 404 {
		t.Errorf("other org path = %d, want 404", code)
	}

	if code, out := w.do(t, w.owner, "PUT", base+"/domains/acme.dev", ""); code != 200 || !strings.Contains(out, `"managed"`) {
		t.Errorf("manage = %d %s", code, out)
	}
	if code, _ := w.do(t, w.owner, "POST", base+"/domains/away.com/takeover", `{"confirm":"away"}`); code != 400 && code != 422 {
		t.Errorf("take over without the domain typed = %d, want 4xx", code)
	}
	if len(w.env.DNS.TookOver) != 0 {
		t.Error("took over without the domain typed")
	}
	for _, p := range []string{"/orgs/acme/dns-connectors", base + "/domains"} {
		code, out := w.do(t, w.owner, "GET", p, "")
		if code != 200 || strings.Contains(out, apiKeySentinel) {
			t.Errorf("GET %s = %d, key shown: %v", p, code, strings.Contains(out, apiKeySentinel))
		}
	}

	if code, out := w.do(t, w.owner, "GET", base+"/domains/acme.dev/plan", ""); code != 200 {
		t.Errorf("plan = %d %s", code, out)
	}

	// server level: admin only
	if code, _ := w.do(t, w.owner, "GET", "/admin/dns-connectors/"+id+"/domains/acme.dev/plan", ""); code != 403 {
		t.Errorf("owner server plan = %d, want 403", code)
	}
	if code, _ := w.do(t, w.owner, "GET", "/admin/dns-connectors", ""); code != 403 {
		t.Errorf("owner GET /admin/dns-connectors = %d, want 403", code)
	}
	if code, _ := w.do(t, w.admin, "GET", "/admin/dns-connectors/"+id+"/domains", ""); code != 404 {
		t.Errorf("server route on an org connector = %d, want 404", code)
	}
}

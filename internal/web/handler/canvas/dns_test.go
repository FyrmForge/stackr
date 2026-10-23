package canvas_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// The org drawer's Connectors tab is an owner's: a member sees why not and
// reaches no action; the owner adds a Namecheap account (key never shown)
// and manages a domain, which only this org may then use.
func TestOrgDNSConnectors(t *testing.T) {
	s := webtest.New(t)
	ctx := context.Background()
	s.DNS.Zones = []service.DNSZone{{Name: "acme.dev", OurDNS: true}}
	const key = "org-key-never-shown-12"

	m := s.User(t, "member@acme.test", false)
	s.Member(t, s.Org, m, "member")
	member := s.Session(t, m)
	if body := s.As(t, member, "GET", "/acme/-/drawer?tab=connectors", nil).Body.String(); !strings.Contains(body, "Only an owner") ||
		strings.Contains(body, "Add a DNS connector") {
		t.Errorf("member tab:\n%s", body)
	}
	if rec := s.As(t, member, "POST", "/acme/-/drawer/dns/namecheap", url.Values{"user": {"jo"}, "key": {key}, "ip": {"203.0.113.7"}}); rec.Code != http.StatusForbidden {
		t.Errorf("member add = %d, want 403", rec.Code)
	}

	rec := s.Do(t, "POST", "/acme/-/drawer/dns/namecheap", url.Values{"user": {"jo"}, "key": {key}, "ip": {"203.0.113.7"}})
	if body := rec.Body.String(); rec.Code != http.StatusOK || !strings.Contains(body, `id="tab-connectors"`) ||
		!strings.Contains(body, "Namecheap · jo") || strings.Contains(body, key) {
		t.Fatalf("add = %d\n%s", rec.Code, body)
	}
	cs, _ := s.Orch.DNSConnectors(ctx, &s.Org)
	if len(cs) != 1 {
		t.Fatalf("connectors = %v", cs)
	}
	base := "/acme/-/drawer/dns/" + cs[0].ID
	if rec := s.As(t, member, "POST", base+"/domains", nil); rec.Code != http.StatusForbidden {
		t.Errorf("member domains = %d, want 403", rec.Code)
	}
	rec = s.Do(t, "POST", base+"/manage", url.Values{"domain": {"acme.dev"}})
	if body := rec.Body.String(); rec.Code != http.StatusOK || !strings.Contains(body, "Managed") || strings.Contains(body, "Available to") {
		t.Fatalf("manage = %d\n%s", rec.Code, body)
	}
	if got, _ := s.Orch.UsableDNSDomains(ctx, s.Org); len(got) != 1 {
		t.Errorf("usable = %v", got)
	}
	rec = s.Do(t, "POST", base+"/delete", nil)
	if cs, _ := s.Orch.DNSConnectors(ctx, &s.Org); rec.Code != http.StatusOK || len(cs) != 0 {
		t.Errorf("delete = %d, left %v", rec.Code, cs)
	}
}

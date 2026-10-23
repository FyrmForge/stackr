package tile_test

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/libdns/libdns"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// A managed domain is picked from a list with a subdomain; a name that has
// a record stackr did not create asks "Replace it", and yes takes it over.
func TestManagedDomainForm(t *testing.T) {
	s := webtest.New(t)
	ctx := context.Background()
	s.DNS.Zones = []service.DNSZone{{Name: "acme.dev", OurDNS: true}}
	s.DNS.Put("acme.dev", libdns.RR{Name: "shop", Type: "A", Data: "203.0.113.9"})
	ns, err := s.Store.Nodes.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ns[0].PublicIP = "198.51.100.1"
	if err := s.Store.Nodes.Update(ctx, ns[0]); err != nil {
		t.Fatal(err)
	}
	c, err := s.Orch.AddDNSConnector(ctx, nil, service.DNSNamecheap, service.DNSAccount{User: "u", Key: "k", ClientIP: "203.0.113.7"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Orch.ManageDNSDomain(ctx, nil, c.Connector.ID, "acme.dev", nil); err != nil {
		t.Fatal(err)
	}
	body := s.Do(t, "GET", drawer+"?tab=settings", nil).Body.String()
	if !strings.Contains(body, `<option value="acme.dev">.acme.dev</option>`) || !strings.Contains(body, "Other (I'll set DNS myself)") {
		t.Fatalf("no managed domain list:\n%s", body)
	}
	form := url.Values{"domain": {"acme.dev"}, "sub": {"shop"}, "https": {"1"}}
	rec := s.Do(t, "POST", drawer+"/domains", form)
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), "Replace it") || !strings.Contains(rec.Body.String(), `&#34;replace&#34;:&#34;1&#34;`) {
		t.Fatalf("taken name = %d\n%s", rec.Code, rec.Body)
	}
	form.Set("replace", "1")
	rec = s.Do(t, "POST", drawer+"/domains", form)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "attached shop.acme.dev") {
		t.Fatalf("replace = %d\n%s", rec.Code, rec.Body)
	}
	if z := s.DNS.Zone("acme.dev"); len(z) != 1 || z[0].Data != "198.51.100.1" {
		t.Errorf("zone = %v", z)
	}
}

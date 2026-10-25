package admin_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/libdns/libdns"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// Manage on a domain with records already pointing at our node opens a
// confirm listing them; a domain with nothing to ask is managed at once.
// Posting the ticked ones adopts them.
func TestAdminManageAsksToAdopt(t *testing.T) {
	s := webtest.New(t)
	ctx := context.Background()
	s.DNS.Zones = []service.DNSZone{{Name: "acme.dev", OurDNS: true}, {Name: "shop.io", OurDNS: true}}
	ns, _ := s.Store.Nodes.List(ctx)
	ns[0].PublicIP = "198.51.100.1"
	if err := s.Store.Nodes.Update(ctx, ns[0]); err != nil {
		t.Fatal(err)
	}
	s.DNS.Put("acme.dev",
		libdns.RR{Name: "www", Type: "A", Data: "198.51.100.1", TTL: 300 * time.Second},
		libdns.RR{Name: "*", Type: "A", Data: "198.51.100.1", TTL: 300 * time.Second},
		libdns.RR{Name: "old", Type: "A", Data: "203.0.113.9", TTL: 300 * time.Second})
	root := s.Session(t, s.User(t, "root@x.test", true))
	c, err := s.Orch.AddConnector(ctx, service.ConnectorAdd{Kind: "namecheap", Values: map[string]string{"user": "jo", "key": panelKey, "client_ip": "203.0.113.7"}})
	if err != nil {
		t.Fatal(err)
	}
	base := "/-/admin/dns/" + c.Connector.ID

	rec := s.As(t, root, "POST", base+"/preview", url.Values{"domain": {"acme.dev"}})
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "Manage acme.dev?") || !strings.Contains(body, `name="adopt" value="www|A|198.51.100.1"`) ||
		strings.Contains(body, "old") || strings.Contains(body, `value="*|`) {
		t.Fatalf("preview = %d\n%s", rec.Code, body)
	}
	if ds, _ := s.Orch.DNSDomains(ctx, nil, c.Connector.ID); ds[0].State == service.DNSManaged {
		t.Error("the preview managed the domain")
	}

	// nothing to ask: managed straight away
	rec = s.As(t, root, "POST", base+"/preview", url.Values{"domain": {"shop.io"}})
	if body := rec.Body.String(); rec.Code != http.StatusOK || strings.Contains(body, "Manage shop.io?") || !strings.Contains(body, "Saved.") {
		t.Fatalf("preview of an empty zone = %d\n%s", rec.Code, body)
	}

	rec = s.As(t, root, "POST", base+"/manage", url.Values{"domain": {"acme.dev"}, "adopt": {"www|A|198.51.100.1"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("manage = %d\n%s", rec.Code, rec.Body)
	}
	rows, _ := s.Store.DNSRecords.List(ctx)
	if len(rows) != 1 || rows[0].Name != "www" || !rows[0].Adopted {
		t.Errorf("rows = %+v", rows)
	}
}

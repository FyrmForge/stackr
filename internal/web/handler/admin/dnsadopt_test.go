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
		strings.Contains(body, "old.acme.dev") || strings.Contains(body, `value="*|`) {
		t.Fatalf("preview = %d\n%s", rec.Code, body)
	}
	if ds, _ := s.Orch.DNSDomains(ctx, nil, c.Connector.ID); ds[0].State == service.DNSManaged {
		t.Error("the preview managed the domain")
	}

	// an empty zone still asks: the scope is the plan's first question
	rec = s.As(t, root, "POST", base+"/preview", url.Values{"domain": {"shop.io"}})
	if body := rec.Body.String(); rec.Code != http.StatusOK || !strings.Contains(body, "Manage shop.io?") || !strings.Contains(body, "Nothing changes in the zone now.") {
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

// The setup card offers the panel's part of a domain; its button opens the
// plan for that scope with the records it leaves alone, and managing reads
// "under" on the row. A bad scope is refused on its field.
func TestAdminSuggestsPanelScope(t *testing.T) {
	s := webtest.New(t)
	ctx := context.Background()
	s.DNS.Zones = []service.DNSZone{{Name: "acme.dev", OurDNS: true}}
	s.DNS.Put("acme.dev", libdns.RR{Name: "www", Type: "A", Data: "203.0.113.9", TTL: 300 * time.Second})
	if err := s.Orch.SetSettings(ctx, map[string]string{"panel_domain": "stackr.acme.dev"}); err != nil {
		t.Fatal(err)
	}
	root := s.Session(t, s.User(t, "root@x.test", true))
	c, err := s.Orch.AddConnector(ctx, service.ConnectorAdd{Kind: "namecheap", Values: map[string]string{"user": "jo", "key": panelKey, "client_ip": "203.0.113.7"}})
	if err != nil {
		t.Fatal(err)
	}
	base := "/-/admin/dns/" + c.Connector.ID

	body := s.As(t, root, "POST", base+"/domains", nil).Body.String()
	if !strings.Contains(body, "Your panel runs at") || !strings.Contains(body, "Manage stackr.acme.dev") || !strings.Contains(body, "Manage all of acme.dev") {
		t.Fatalf("no suggestion card:\n%s", body)
	}
	if body := s.As(t, root, "POST", base+"/domains", url.Values{"not_now": {"1"}}).Body.String(); strings.Contains(body, "Your panel runs at") {
		t.Error("Not now kept the card")
	}

	rec := s.As(t, root, "POST", base+"/preview", url.Values{"domain": {"acme.dev"}, "part": {"under"}, "under": {"bad_name"}})
	if body := rec.Body.String(); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(body, `aria-invalid`) || !strings.Contains(body, `data-has-error="true"`) {
		t.Fatalf("bad scope = %d\n%s", rec.Code, body)
	}

	body = s.As(t, root, "POST", base+"/preview", url.Values{"domain": {"acme.dev"}, "part": {"under"}, "under": {"stackr.acme.dev"}}).Body.String() // the full name pasted
	if !strings.Contains(body, "Manage stackr.acme.dev?") || !strings.Contains(body, "Left alone: 1 record outside stackr.acme.dev") || !strings.Contains(body, "www.acme.dev") {
		t.Fatalf("scoped plan:\n%s", body)
	}
	rec = s.As(t, root, "POST", base+"/manage", url.Values{"domain": {"acme.dev"}, "part": {"under"}, "under": {"stackr"}})
	if body := rec.Body.String(); rec.Code != http.StatusOK || !strings.Contains(body, "under <span class=\"font-mono\">stackr.acme.dev</span>") {
		t.Fatalf("manage = %d\n%s", rec.Code, body)
	}
}

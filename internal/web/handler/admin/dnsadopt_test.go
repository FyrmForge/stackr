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

// The setup card is the plain list's and a manage, unmanage or take over's:
// Change scope and Records answer about their own domain and leave it out.
// A domain managed through another connector is never offered.
func TestAdminSuggestionOnlyOnList(t *testing.T) {
	s := webtest.New(t)
	ctx := context.Background()
	s.DNS.Zones = []service.DNSZone{{Name: "acme.dev", OurDNS: true}, {Name: "vulpe.dev", OurDNS: true}}
	if err := s.Orch.SetSettings(ctx, map[string]string{"panel_domain": "stackr.vulpe.dev"}); err != nil {
		t.Fatal(err)
	}
	root := s.Session(t, s.User(t, "root@x.test", true))
	add := func(user string) string {
		c, err := s.Orch.AddConnector(ctx, service.ConnectorAdd{Kind: "namecheap", Values: map[string]string{"user": user, "key": panelKey, "client_ip": "203.0.113.7"}})
		if err != nil {
			t.Fatal(err)
		}
		return "/-/admin/dns/" + c.Connector.ID
	}
	base := add("jo")
	if _, err := s.Orch.ManageDNSDomainAdopting(ctx, nil, strings.TrimPrefix(base, "/-/admin/dns/"), "acme.dev", "test.acme.dev", nil, nil); err != nil {
		t.Fatal(err)
	}
	if body := s.As(t, root, "POST", base+"/domains", nil).Body.String(); !strings.Contains(body, "Your panel runs at") {
		t.Fatalf("no card on the list:\n%s", body)
	}
	rec := s.As(t, root, "POST", base+"/rescope", url.Values{"domain": {"acme.dev"}, "part": {"all"}})
	if body := rec.Body.String(); !strings.Contains(body, "Scope changed.") || strings.Contains(body, "Your panel runs at") {
		t.Fatalf("rescope answer = %d\n%s", rec.Code, body)
	}
	if body := s.As(t, root, "POST", base+"/records", url.Values{"domain": {"acme.dev"}}).Body.String(); strings.Contains(body, "Your panel runs at") {
		t.Errorf("card on the records answer:\n%s", body)
	}
	// a manage, unmanage or take over that worked brings the next one
	if body := s.As(t, root, "POST", base+"/unmanage", url.Values{"domain": {"acme.dev"}}).Body.String(); !strings.Contains(body, "No longer managed.") || !strings.Contains(body, "Your panel runs at") {
		t.Errorf("unmanage answer without the next suggestion:\n%s", body)
	}

	// vulpe.dev managed through jo's connector: a second one lists it too
	if _, err := s.Orch.ManageDNSDomain(ctx, nil, strings.TrimPrefix(base, "/-/admin/dns/"), "vulpe.dev", nil); err != nil {
		t.Fatal(err)
	}
	body := s.As(t, root, "POST", add("ann")+"/domains", nil).Body.String()
	if strings.Contains(body, "Your panel runs at") {
		t.Errorf("card for a domain another connector manages:\n%s", body)
	}
	if strings.Count(body, "Managed by another connector") != 1 || strings.Count(body, ">Manage</button>") != 1 { // acme.dev only
		t.Errorf("ann's list:\n%s", body)
	}
}

// A managed row offers Change scope on its own scope; widening plans the
// records now inside, narrowing lists what becomes the user's, and the
// change lands with the row reading its new scope.
func TestAdminChangeScope(t *testing.T) {
	s := webtest.New(t)
	ctx := context.Background()
	s.DNS.Zones = []service.DNSZone{{Name: "acme.dev", OurDNS: true}}
	ns, _ := s.Store.Nodes.List(ctx)
	ns[0].PublicIP = "198.51.100.1"
	if err := s.Store.Nodes.Update(ctx, ns[0]); err != nil {
		t.Fatal(err)
	}
	s.DNS.Put("acme.dev", libdns.RR{Name: "www", Type: "A", Data: "198.51.100.1", TTL: 300 * time.Second})
	root := s.Session(t, s.User(t, "root@x.test", true))
	c, err := s.Orch.AddConnector(ctx, service.ConnectorAdd{Kind: "namecheap", Values: map[string]string{"user": "jo", "key": panelKey, "client_ip": "203.0.113.7"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Orch.ManageDNSDomainAdopting(ctx, nil, c.Connector.ID, "acme.dev", "test.acme.dev", nil, nil); err != nil {
		t.Fatal(err)
	}
	base := "/-/admin/dns/" + c.Connector.ID

	body := s.As(t, root, "POST", base+"/domains", nil).Body.String()
	if !strings.Contains(body, ">Change scope</button>") || !strings.Contains(body, base+"/rescope-preview") {
		t.Fatalf("no Change scope on the managed row:\n%s", body)
	}
	rec := s.As(t, root, "POST", base+"/rescope-preview", url.Values{"domain": {"acme.dev"}, "part": {"all"}})
	body = rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "Change what stackr manages on acme.dev?") || !strings.Contains(body, `name="adopt" value="www|A|198.51.100.1"`) {
		t.Fatalf("widen preview = %d\n%s", rec.Code, body)
	}
	if !strings.Contains(body, `&#34;from&#34;:&#34;test.acme.dev&#34;`) {
		t.Fatalf("widen preview posts no from:\n%s", body)
	}
	confirm := url.Values{"domain": {"acme.dev"}, "part": {"all"}, "from": {"test.acme.dev"}, "adopt": {"www|A|198.51.100.1"}}
	rec = s.As(t, root, "POST", base+"/rescope", confirm)
	if body := rec.Body.String(); rec.Code != http.StatusOK || !strings.Contains(body, "Scope changed.") || strings.Contains(body, "under <span") {
		t.Fatalf("widen = %d\n%s", rec.Code, body)
	}
	// the same confirm again: its plan is stale now
	if body := s.As(t, root, "POST", base+"/rescope", confirm).Body.String(); !strings.Contains(body, "The scope of acme.dev changed since this plan. Review the plan again.") {
		t.Fatalf("stale confirm:\n%s", body)
	}

	body = s.As(t, root, "POST", base+"/rescope-preview", url.Values{"domain": {"acme.dev"}, "part": {"under"}, "under": {"test"}}).Body.String()
	if !strings.Contains(body, "These become yours.") || !strings.Contains(body, "www.acme.dev → 198.51.100.1") {
		t.Fatalf("narrow preview:\n%s", body)
	}
	rec = s.As(t, root, "POST", base+"/rescope", url.Values{"domain": {"acme.dev"}, "part": {"under"}, "under": {"test"}})
	if body := rec.Body.String(); rec.Code != http.StatusOK || !strings.Contains(body, "under <span class=\"font-mono\">test.acme.dev</span>") {
		t.Fatalf("narrow = %d\n%s", rec.Code, body)
	}
}

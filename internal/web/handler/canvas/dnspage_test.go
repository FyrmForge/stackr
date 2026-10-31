package canvas_test

import (
	"context"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// The org drawer's Managed DNS tab (owners): the domains the org may use,
// only its own tiles' records on a server domain, and Change scope and
// Records only on its own connector's domain, linking its Connectors tab.
func TestOrgManagedDNS(t *testing.T) {
	s := webtest.New(t)
	ctx := context.Background()
	s.DNS.Zones = []service.DNSZone{{Name: "acme.dev", OurDNS: true}, {Name: "shop.io", OurDNS: true}, {Name: "beta.dev", OurDNS: true}}
	ns, _ := s.Store.Nodes.List(ctx)
	ns[0].PublicIP = "198.51.100.1"
	if err := s.Store.Nodes.Update(ctx, ns[0]); err != nil {
		t.Fatal(err)
	}
	account := map[string]string{"user": "jo", "key": "k", "client_ip": "203.0.113.7"}
	srv, err := s.Orch.AddConnector(ctx, service.ConnectorAdd{Kind: "namecheap", Values: account})
	if err != nil {
		t.Fatal(err)
	}
	beta := s.Env.Org(t, "beta")
	if _, err := s.Orch.ManageDNSDomain(ctx, nil, srv.Connector.ID, "acme.dev", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Orch.ManageDNSDomain(ctx, nil, srv.Connector.ID, "beta.dev", []string{beta}); err != nil {
		t.Fatal(err)
	}
	own, err := s.Orch.AddConnector(ctx, service.ConnectorAdd{OrgID: &s.Org, Kind: "namecheap", Values: account})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Orch.ManageDNSDomainAdopting(ctx, &s.Org, own.Connector.ID, "shop.io", "test.shop.io", nil, nil); err != nil {
		t.Fatal(err)
	}
	host := func(tileID, h string) {
		t.Helper()
		if _, _, err := s.Orch.UpdateTile(ctx, tileID, func(tl *service.Tile) error { tl.ContainerPort = 80; return nil }); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Orch.AttachDomain(ctx, tileID, service.DomainSpec{Host: h}); err != nil {
			t.Fatal(err)
		}
	}
	host(s.Tile.ID, "shop.acme.dev")
	host(s.Env.Tile(t, beta).ID, "blog.acme.dev")
	s.Orch.WaitDNS()
	if d, err := s.Orch.ManagedDomainNamed(ctx, nil, "acme.dev"); err != nil || len(d.Records) != 2 {
		t.Fatalf("admin view of acme.dev, want both apps' records: %+v %v", d, err)
	}

	body := s.Do(t, "GET", "/acme/-/drawer?tab=dns", nil).Body.String()
	for _, want := range []string{">Managed DNS<", ">acme.dev<", ">shop.acme.dev<", "&amp;tab=domains", ">shop.io<", "Only under test", "via Namecheap (org)"} {
		if !strings.Contains(body, want) {
			t.Fatalf("no %q on the tab:\n%s", want, body)
		}
	}
	for _, leak := range []string{"blog.acme.dev", "beta.dev", "For ", ">Panel<", ">Node<"} {
		if strings.Contains(body, leak) {
			t.Errorf("tab shows %q:\n%s", leak, body)
		}
	}
	if strings.Count(body, ">Change scope<") != 1 ||
		!strings.Contains(body, `href="/acme?drawer=org:`+s.Org+`&amp;tab=connectors&amp;act=rescope&amp;conn=`+own.Connector.ID+`&amp;domain=shop.io&amp;under=test"`) {
		t.Errorf("Change scope, want on shop.io alone:\n%s", body)
	}

	body = s.Do(t, "GET", "/acme/-/drawer?tab=connectors&act=rescope&conn="+own.Connector.ID+"&domain=shop.io&under=test", nil).Body.String()
	if !strings.Contains(body, "<confirm-dialog open>") || !strings.Contains(body, `hx-post="/acme/-/drawer/dns/`+own.Connector.ID+`/rescope-preview"`) {
		t.Fatalf("the link does not open Change scope:\n%s", body)
	}

	m := s.User(t, "member@acme.test", false)
	s.Member(t, s.Org, m, "member")
	if body := s.As(t, s.Session(t, m), "GET", "/acme/-/drawer?tab=dns", nil).Body.String(); strings.Contains(body, "Managed DNS") || strings.Contains(body, "shop.acme.dev") {
		t.Errorf("a member sees the tab:\n%s", body)
	}
}

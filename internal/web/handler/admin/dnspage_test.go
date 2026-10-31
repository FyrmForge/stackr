package admin_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// Managed DNS lists every managed domain and the records stackr wrote,
// the app record linking its tile's Domains tab; Change scope and Records
// link the Connectors section, which opens that connector's dialog on
// them. Admins only.
func TestAdminManagedDNS(t *testing.T) {
	s := webtest.New(t)
	ctx := context.Background()
	s.DNS.Zones = []service.DNSZone{{Name: "acme.dev", OurDNS: true}}
	ns, _ := s.Store.Nodes.List(ctx)
	ns[0].PublicIP = "198.51.100.1"
	if err := s.Store.Nodes.Update(ctx, ns[0]); err != nil {
		t.Fatal(err)
	}
	root := s.Session(t, s.User(t, "root@x.test", true))
	c, err := s.Orch.AddConnector(ctx, service.ConnectorAdd{Kind: "namecheap", Values: map[string]string{"user": "jo", "key": panelKey, "client_ip": "203.0.113.7"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Orch.ManageDNSDomain(ctx, nil, c.Connector.ID, "acme.dev", nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Orch.UpdateTile(ctx, s.Tile.ID, func(tl *service.Tile) error { tl.ContainerPort = 80; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Orch.AttachDomain(ctx, s.Tile.ID, service.DomainSpec{Host: "shop.acme.dev"}); err != nil {
		t.Fatal(err)
	}
	s.Orch.WaitDNS()

	rec := s.As(t, root, "GET", "/-/admin/managed-dns", nil)
	body := rec.Body.String()
	for _, want := range []string{
		">Managed DNS<", "All of acme.dev", "via Namecheap (server)", "For every org",
		">shop.acme.dev<", "198.51.100.1", "&amp;tab=domains", "Only records stackr wrote are listed.",
		`href="/-/admin/connectors?act=rescope&amp;conn=` + c.Connector.ID + `&amp;domain=acme.dev"`,
		`href="/-/admin/connectors?act=records&amp;conn=` + c.Connector.ID + `&amp;domain=acme.dev"`,
	} {
		if rec.Code != http.StatusOK || !strings.Contains(body, want) {
			t.Fatalf("page = %d, no %q:\n%s", rec.Code, want, body)
		}
	}
	if strings.Contains(body, panelKey) {
		t.Fatal("the account key is on the page")
	}

	body = s.As(t, root, "GET", "/-/admin/connectors?act=records&conn="+c.Connector.ID+"&domain=acme.dev", nil).Body.String()
	if !strings.Contains(body, "<confirm-dialog open>") || !strings.Contains(body, `hx-post="/-/admin/dns/`+c.Connector.ID+`/records"`) {
		t.Fatalf("records link does not open the dialog:\n%s", body)
	}
	body = s.As(t, root, "GET", "/-/admin/connectors?act=rescope&conn="+c.Connector.ID+"&domain=acme.dev", nil).Body.String()
	if !strings.Contains(body, `hx-post="/-/admin/dns/`+c.Connector.ID+`/rescope-preview"`) {
		t.Fatalf("rescope link does not open Change scope:\n%s", body)
	}

	if rec := s.Do(t, "GET", "/-/admin/managed-dns", nil); rec.Code == http.StatusOK {
		t.Errorf("an org owner read the server page: %d", rec.Code)
	}
}

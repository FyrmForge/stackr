package admin_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// The settings dialog's tabs: Connection (status, rename, credentials
// with no secret ever filled), Domains, Records (the record stackr made
// for a tile domain, its tile's path linked and the node it points at)
// and Used by (that tile, linked).
func TestConnectorSettingsTabs(t *testing.T) {
	s := webtest.New(t)
	ctx := context.Background()
	s.DNS.Zones = []service.DNSZone{{Name: "acme.dev", OurDNS: true}}
	root := s.Session(t, s.User(t, "root@x.test", true))
	ns, _ := s.Store.Nodes.List(ctx)
	ns[0].PublicIP = "198.51.100.1"
	if err := s.Store.Nodes.Update(ctx, ns[0]); err != nil {
		t.Fatal(err)
	}
	a, err := s.Orch.AddConnector(ctx, service.ConnectorAdd{Kind: "namecheap", Values: map[string]string{"user": "jo", "key": panelKey, "client_ip": "203.0.113.7"}})
	if err != nil {
		t.Fatal(err)
	}
	id := a.Connector.ID
	if _, err := s.Orch.ManageDNSDomain(ctx, nil, id, "acme.dev", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Orch.AttachDomain(ctx, s.Tile.ID, service.DomainSpec{Host: "shop.acme.dev"}); err != nil {
		t.Fatal(err)
	}
	s.Orch.WaitDNS()
	base := "/-/admin/connectors/" + id

	body := s.As(t, root, "GET", base+"/settings", nil).Body.String()
	for _, w := range []string{">Connection</button>", ">Domains</button>", ">Records</button>", ">Used by</button>", `aria-current="page"`, "modal-tall"} {
		if !strings.Contains(body, w) {
			t.Errorf("dialog lacks %s", w)
		}
	}
	tab := func(name string) string {
		t.Helper()
		b := s.As(t, root, "GET", base+"/settings?tab="+name, nil).Body.String()
		if strings.Contains(b, panelKey) {
			t.Errorf("the %s tab carries the key", name)
		}
		return b
	}
	conn := tab("connection")
	for _, w := range []string{"Namecheap · jo", "Working, checked", "Check now", `name="user" type="text" value="jo"`, `name="key" type="password" value=""`, "Update credentials"} {
		if !strings.Contains(conn, w) {
			t.Errorf("Connection lacks %s\n%s", w, conn)
		}
	}
	rec := tab("records")
	path := "/acme/shop/dev?drawer=tile:" + s.Tile.ID
	for _, w := range []string{"acme.dev", ">shop<", "198.51.100.1", "on " + ns[0].Name, "shop.acme.dev", `href="` + path + `"`, "acme / shop / dev / api"} {
		if !strings.Contains(rec, w) {
			t.Errorf("Records lacks %s\n%s", w, rec)
		}
	}
	if used := tab("used"); !strings.Contains(used, `href="`+path+`"`) || !strings.Contains(used, "A record shop.acme.dev") {
		t.Errorf("Used by:\n%s", used)
	}

	// an empty key keeps the stored one; a refused one changes nothing and
	// comes back without the key
	upd := s.As(t, root, "POST", base+"/credentials", url.Values{"user": {"ann"}, "key": {""}, "client_ip": {"203.0.113.7"}})
	if upd.Code != http.StatusOK || !strings.Contains(upd.Body.String(), "Saved and checked.") || strings.Contains(upd.Body.String(), panelKey) {
		t.Fatalf("update = %d\n%s", upd.Code, upd.Body)
	}
	if o := s.DNS.Opened[len(s.DNS.Opened)-1]; o["key"] != panelKey {
		t.Error("the empty key was not kept")
	}
	upd = s.As(t, root, "POST", base+"/credentials", url.Values{"user": {"bob"}, "key": {servicetest.BadKey}, "client_ip": {"203.0.113.7"}})
	if b := upd.Body.String(); upd.Code != http.StatusUnprocessableEntity || strings.Contains(b, servicetest.BadKey) || !strings.Contains(b, `value="bob"`) {
		t.Fatalf("refused update = %d\n%s", upd.Code, b)
	}
	if c, _ := s.Orch.ConnectorAt(ctx, nil, id); c.Name != "Namecheap · ann" {
		t.Errorf("a refused update renamed it: %q", c.Name)
	}
	// stop managing while the tile uses it: refused, the tile linked
	if b := s.As(t, root, "POST", "/-/admin/dns/"+id+"/unmanage", url.Values{"domain": {"acme.dev"}}).Body.String(); !strings.Contains(b, `href="`+path+`"`) {
		t.Errorf("unmanage in use:\n%s", b)
	}
}

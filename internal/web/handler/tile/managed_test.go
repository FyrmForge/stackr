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
	c, err := s.Orch.AddConnector(ctx, service.ConnectorAdd{OrgID: nil, Kind: "namecheap", Values: map[string]string{"user": "u", "key": "k", "client_ip": "203.0.113.7"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Orch.ManageDNSDomain(ctx, nil, c.Connector.ID, "acme.dev", nil); err != nil {
		t.Fatal(err)
	}
	body := s.Do(t, "GET", drawer+"?tab=settings", nil).Body.String()
	if !strings.Contains(body, `<option value="acme.dev">acme.dev</option>`) || !strings.Contains(body, "Other (I'll set DNS myself)") {
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

// A Replace it offer keeps the add form open with what was typed and says
// why, so a Cancel loses nothing.
func TestManagedDomainReplaceKeepsForm(t *testing.T) {
	s := managedSite(t, "198.51.100.1")
	s.DNS.Put("acme.dev", libdns.RR{Name: "shop", Type: "A", Data: "203.0.113.9"})
	rec := s.Do(t, "POST", drawer+"/domains", url.Values{"domain": {"acme.dev"}, "sub": {"shop"}, "path": {"/api"}})
	body := rec.Body.String()
	for _, want := range []string{`<details class="mb-4" open`, `name="sub" value="shop"`, `name="path" value="/api"`, `<option value="acme.dev" selected>`, "shop.acme.dev already has a DNS record."} {
		if !strings.Contains(body, want) {
			t.Errorf("offer = %d, no %q", rec.Code, want)
		}
	}
}

// An add on a managed domain whose ingress node has no public IP is
// refused naming the node; a server admin gets a link to its Network tab.
func TestManagedDomainNoPublicIP(t *testing.T) {
	s := managedSite(t, "")
	ns, err := s.Store.Nodes.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	named := "Node " + ns[0].Name + " has no public IP address"
	link := `href="/-/cluster?drawer=node:` + ns[0].ID + `&amp;tab=network"`
	rec := s.Do(t, "POST", drawer+"/domains", url.Values{"domain": {"acme.dev"}, "sub": {"app"}, "https": {"1"}})
	if body := rec.Body.String(); rec.Code != 422 || !strings.Contains(body, named) || strings.Contains(body, link) {
		t.Fatalf("owner: no IP = %d, want %q and no link", rec.Code, named)
	}
	admin := s.Session(t, s.User(t, "admin@acme.test", true))
	rec = s.As(t, admin, "POST", drawer+"/domains", url.Values{"domain": {"acme.dev"}, "sub": {"web"}, "https": {"1"}})
	if body := rec.Body.String(); rec.Code != 422 || !strings.Contains(body, named) || !strings.Contains(body, link) {
		t.Fatalf("admin: no IP = %d, want %q and %s", rec.Code, named, link)
	}
	if body := s.Do(t, "GET", drawer+"?tab=settings", nil).Body.String(); strings.Contains(body, "app.acme.dev") || strings.Contains(body, "web.acme.dev") {
		t.Errorf("a refused add saved its domain")
	}
}

// managedSite has acme.dev managed through a DNS connector, the node's
// public IP set to ip ("" = none).
func managedSite(t *testing.T, ip string) *webtest.Site {
	t.Helper()
	s := webtest.New(t)
	ctx := context.Background()
	s.DNS.Zones = []service.DNSZone{{Name: "acme.dev", OurDNS: true}}
	ns, err := s.Store.Nodes.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ns[0].PublicIP = ip
	if err := s.Store.Nodes.Update(ctx, ns[0]); err != nil {
		t.Fatal(err)
	}
	c, err := s.Orch.AddConnector(ctx, service.ConnectorAdd{OrgID: nil, Kind: "namecheap", Values: map[string]string{"user": "u", "key": "k", "client_ip": "203.0.113.7"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Orch.ManageDNSDomain(ctx, nil, c.Connector.ID, "acme.dev", nil); err != nil {
		t.Fatal(err)
	}
	return s
}

// The add form is one address row with a preview, and an empty subdomain
// attaches the bare domain.
func TestManagedDomainBareHost(t *testing.T) {
	s := managedSite(t, "198.51.100.1")
	body := s.Do(t, "GET", drawer+"?tab=settings", nil).Body.String()
	for _, want := range []string{`data-preview`, ">https://acme.dev<", "Leave empty to use", "Use the generated name"} {
		if !strings.Contains(body, want) {
			t.Errorf("form lacks %q", want)
		}
	}
	rec := s.Do(t, "POST", drawer+"/domains", url.Values{"domain": {"acme.dev"}, "sub": {""}, "https": {"1"}})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "attached acme.dev") {
		t.Fatalf("bare domain = %d\n%s", rec.Code, rec.Body)
	}
}

// "Use the generated name" shows the host it would attach once a domain
// resource is visible to the tile, and no host before.
func TestGeneratedNameShown(t *testing.T) {
	s := webtest.New(t)
	if body := s.Do(t, "GET", drawer+"?tab=settings", nil).Body.String(); !strings.Contains(body, "Use the generated name") {
		t.Fatalf("no generated-name button:\n%s", body)
	}
	if _, err := s.Orch.CreateDomainResource(context.Background(), "instance", "", "example.com", false, ""); err != nil {
		t.Fatal(err)
	}
	if body := s.Do(t, "GET", drawer+"?tab=settings", nil).Body.String(); !strings.Contains(body, `ml-1">api.`) || !strings.Contains(body, ".example.com") {
		t.Errorf("generated host not shown:\n%s", body)
	}
}

// A name whose record already points at our node asks "Let stackr manage
// it?", and yes adopts it and attaches the host.
func TestManagedDomainAdoptOffer(t *testing.T) {
	s := managedSite(t, "198.51.100.1")
	s.DNS.Put("acme.dev", libdns.RR{Name: "shop", Type: "A", Data: "198.51.100.1"})
	form := url.Values{"domain": {"acme.dev"}, "sub": {"shop"}, "https": {"1"}}
	rec := s.Do(t, "POST", drawer+"/domains", form)
	body := rec.Body.String()
	if rec.Code != 422 || !strings.Contains(body, "shop.acme.dev already points at your node. Let stackr manage it?") ||
		!strings.Contains(body, `&#34;adopt&#34;:&#34;1&#34;`) || strings.Contains(body, "Replace it") {
		t.Fatalf("adopt offer = %d\n%s", rec.Code, body)
	}
	form.Set("adopt", "1")
	rec = s.Do(t, "POST", drawer+"/domains", form)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "attached shop.acme.dev") {
		t.Fatalf("adopt = %d\n%s", rec.Code, rec.Body)
	}
}

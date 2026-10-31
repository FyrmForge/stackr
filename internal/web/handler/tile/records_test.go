package tile_test

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// A host in a managed domain shows its DNS records section; an add lands
// with its note and Remove takes it off. A host outside every managed
// domain has no section.
func TestDomainRecords(t *testing.T) {
	s := webtest.New(t)
	ctx := context.Background()
	s.DNS.Zones = []service.DNSZone{{Name: "acme.dev", OurDNS: true}}
	ns, _ := s.Store.Nodes.List(ctx)
	ns[0].PublicIP = "198.51.100.1"
	if err := s.Store.Nodes.Update(ctx, ns[0]); err != nil {
		t.Fatal(err)
	}
	c, err := s.Orch.AddConnector(ctx, service.ConnectorAdd{Kind: "namecheap", Values: map[string]string{"user": "jo", "key": "k", "client_ip": "203.0.113.7"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Orch.ManageDNSDomain(ctx, nil, c.Connector.ID, "acme.dev", nil); err != nil {
		t.Fatal(err)
	}
	setPort(t, s, 80)
	d, err := s.Orch.AttachDomain(ctx, s.Tile.ID, service.DomainSpec{Host: "shop.acme.dev"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Orch.AttachDomain(ctx, s.Tile.ID, service.DomainSpec{Host: "app.example.org"}); err != nil {
		t.Fatal(err)
	}
	body := s.Do(t, "GET", drawer+"?tab=settings", nil).Body.String()
	if strings.Count(body, ">DNS records</summary>") != 1 || !strings.Contains(body, "Managed by acme.dev via Namecheap (server)") {
		t.Fatalf("records section:\n%s", body)
	}
	rec := s.Do(t, "POST", drawer+"/domains/"+d.ID+"/records?open=network", url.Values{"record_of": {d.ID}, "rec_name": {"_dmarc"}, "rec_type": {"TXT"}, "rec_value": {"v=DMARC1; p=none"}})
	if body := rec.Body.String(); rec.Code != 200 || !strings.Contains(body, "Record added.") || !strings.Contains(body, "_dmarc.shop.acme.dev") {
		t.Fatalf("add = %d\n%s", rec.Code, body)
	}
	rs, _ := s.Orch.TileDomainRecords(ctx, s.Tile.ID, d.ID)
	if len(rs.Records) != 1 {
		t.Fatalf("records = %+v", rs)
	}
	rec = s.Do(t, "POST", drawer+"/domains/"+d.ID+"/records/remove?open=network", url.Values{"record": {rs.Records[0].ID}})
	if body := rec.Body.String(); rec.Code != 200 || !strings.Contains(body, "Record removed.") || strings.Contains(body, "_dmarc.shop.acme.dev") {
		t.Fatalf("remove = %d\n%s", rec.Code, body)
	}
}

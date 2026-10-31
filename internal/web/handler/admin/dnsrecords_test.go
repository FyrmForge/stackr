package admin_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// A managed row opens its custom records; an add lands in the zone with
// its note, a bad value is refused in the section, and Remove deletes it.
func TestAdminDomainRecords(t *testing.T) {
	s := webtest.New(t)
	ctx := context.Background()
	s.DNS.Zones = []service.DNSZone{{Name: "acme.dev", OurDNS: true}}
	root := s.Session(t, s.User(t, "root@x.test", true))
	c, err := s.Orch.AddConnector(ctx, service.ConnectorAdd{Kind: "namecheap", Values: map[string]string{"user": "jo", "key": panelKey, "client_ip": "203.0.113.7"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Orch.ManageDNSDomain(ctx, nil, c.Connector.ID, "acme.dev", nil); err != nil {
		t.Fatal(err)
	}
	base := "/-/admin/dns/" + c.Connector.ID

	body := s.As(t, root, "POST", base+"/domains", nil).Body.String()
	if !strings.Contains(body, ">Records</button>") || !strings.Contains(body, base+"/records") {
		t.Fatalf("no Records on the managed row:\n%s", body)
	}
	body = s.As(t, root, "POST", base+"/records", url.Values{"domain": {"acme.dev"}}).Body.String()
	if !strings.Contains(body, "Managed by acme.dev via Namecheap (server)") || !strings.Contains(body, base+"/record-add") {
		t.Fatalf("records closed:\n%s", body)
	}

	rec := s.As(t, root, "POST", base+"/record-add", url.Values{"domain": {"acme.dev"}, "record_of": {"acme.dev"}, "rec_type": {"A"}, "rec_value": {"not-an-ip"}})
	if body := rec.Body.String(); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(body, "An A record points at an IPv4 address.") || !strings.Contains(body, `value="not-an-ip"`) {
		t.Fatalf("bad value = %d\n%s", rec.Code, body)
	}
	rec = s.As(t, root, "POST", base+"/record-add", url.Values{"domain": {"acme.dev"}, "rec_type": {"MX"}, "rec_value": {"10 mx.example.net"}})
	if body := rec.Body.String(); rec.Code != http.StatusOK || !strings.Contains(body, "Record added.") || !strings.Contains(body, "10 mx.example.net") {
		t.Fatalf("add = %d\n%s", rec.Code, body)
	}
	rs, _ := s.Orch.DNSDomainRecords(ctx, nil, c.Connector.ID, "acme.dev")
	if len(rs.Records) != 1 || !rs.Records[0].Written {
		t.Fatalf("records = %+v", rs)
	}
	rec = s.As(t, root, "POST", base+"/record-rm", url.Values{"domain": {"acme.dev"}, "record": {rs.Records[0].ID}})
	if body := rec.Body.String(); rec.Code != http.StatusOK || !strings.Contains(body, "Record removed.") || strings.Contains(body, "10 mx.example.net") {
		t.Fatalf("remove = %d\n%s", rec.Code, body)
	}
	if z := s.DNS.Zone("acme.dev"); len(z) != 0 {
		t.Errorf("zone after remove: %v", z)
	}
}

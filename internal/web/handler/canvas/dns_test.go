package canvas_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// The org drawer's Connectors tab is an owner's: a member sees why not and
// reaches no action; the owner adds a Namecheap account (key never shown)
// and manages a domain, which only this org may then use.
func TestOrgDNSConnectors(t *testing.T) {
	s := webtest.New(t)
	ctx := context.Background()
	s.DNS.Zones = []service.DNSZone{{Name: "acme.dev", OurDNS: true}}
	const key = "org-key-never-shown-12"

	m := s.User(t, "member@acme.test", false)
	s.Member(t, s.Org, m, "member")
	member := s.Session(t, m)
	if body := s.As(t, member, "GET", "/acme/-/drawer?tab=connectors", nil).Body.String(); !strings.Contains(body, "Only an owner") ||
		strings.Contains(body, "+ Add connector") {
		t.Errorf("member tab:\n%s", body)
	}
	if rec := s.As(t, member, "POST", "/acme/-/drawer/connectors/new/namecheap", url.Values{"user": {"jo"}, "key": {key}, "client_ip": {"203.0.113.7"}}); rec.Code != http.StatusForbidden {
		t.Errorf("member add = %d, want 403", rec.Code)
	}

	// the add dialog offers both kinds; Namecheap's back goes to the picker
	if body := s.Do(t, "GET", "/acme/-/drawer/connectors/new", nil).Body.String(); !strings.Contains(body, "<confirm-dialog open>") ||
		!strings.Contains(body, `hx-get="/acme/-/drawer/connectors/new/github"`) || !strings.Contains(body, `hx-get="/acme/-/drawer/connectors/new/namecheap"`) ||
		!strings.Contains(body, `hx-get="/acme/-/drawer?tab=connectors"`) {
		t.Fatalf("add dialog:\n%s", body)
	}
	if body := s.Do(t, "GET", "/acme/-/drawer/connectors/new/namecheap", nil).Body.String(); !strings.Contains(body, `hx-get="/acme/-/drawer/connectors/new?pick=1"`) {
		t.Errorf("Namecheap screens:\n%s", body)
	}
	if body := s.Do(t, "GET", "/acme/-/drawer/connectors/new/github", nil).Body.String(); !strings.Contains(body, `hx-post="/acme/-/drawer/connectors/new/github"`) {
		t.Errorf("GitHub screen:\n%s", body)
	}
	rec := s.Do(t, "POST", "/acme/-/drawer/connectors/new/github", url.Values{"owner": {"acme-inc"}})
	if body := rec.Body.String(); rec.Code != http.StatusOK || !strings.Contains(body, "Continue on GitHub") || !strings.Contains(body, `name="manifest"`) {
		t.Fatalf("GitHub begin = %d\n%s", rec.Code, body)
	}
	rec = s.Do(t, "POST", "/acme/-/drawer/connectors/new/github", url.Values{"owner": {"acme-inc"}})
	if body := rec.Body.String(); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(body, ">This org already has a github.com connector<") {
		t.Errorf("second GitHub begin = %d\n%s", rec.Code, body)
	}
	if rec := s.As(t, member, "GET", "/acme/-/drawer/connectors/new", nil); rec.Code != http.StatusForbidden {
		t.Errorf("member add dialog = %d, want 403", rec.Code)
	}

	rec = s.Do(t, "POST", "/acme/-/drawer/connectors/new/namecheap", url.Values{"user": {"jo"}, "key": {key}, "client_ip": {"203.0.113.7"}})
	if body := rec.Body.String(); rec.Code != http.StatusOK || !strings.Contains(body, "Step 5 of 5: Domains") || strings.Contains(body, key) {
		t.Fatalf("add = %d\n%s", rec.Code, body)
	}
	cs, _ := s.Orch.DNSConnectors(ctx, &s.Org)
	if len(cs) != 1 {
		t.Fatalf("connectors = %v", cs)
	}
	// the tab: the pending GitHub connector opens its drawer, the DNS one its
	// dialog; a server App the admin has not finished is not listed
	if _, err := s.Orch.AddConnector(ctx, service.ConnectorAdd{Kind: "github", Values: map[string]string{"owner": "half-done"}, User: s.User(t, "root@x.test", true), Return: "/"}); err != nil {
		t.Fatal(err)
	}
	body := s.Do(t, "GET", "/acme/-/drawer?tab=connectors", nil).Body.String()
	if strings.Count(body, `hx-push-url="?drawer=connector:`) != 1 || !strings.Contains(body, ">pending<") {
		t.Errorf("GitHub rows: want the org's own pending one only\n%s", body)
	}
	if !strings.Contains(body, "Namecheap · jo") || !strings.Contains(body, "No managed domains") ||
		!strings.Contains(body, `hx-get="/acme/-/drawer/connectors/`+cs[0].ID+`/settings"`) || !strings.Contains(body, `hx-push-url="?drawer=connector:`) {
		t.Fatalf("tab:\n%s", body)
	}
	if body := s.Do(t, "GET", "/acme/-/drawer?tab=connectors&add=1", nil).Body.String(); !strings.Contains(body, "<confirm-dialog open>") {
		t.Errorf("add=1 does not open the dialog:\n%s", body)
	}
	// the canvas's "+ Create connector" opens it for an owner
	if body := s.Do(t, "GET", "/acme", nil).Body.String(); !strings.Contains(body, `hx-get="/acme/-/drawer?tab=connectors&amp;add=1"`) {
		t.Errorf("the canvas create button does not open the add dialog")
	}
	base := "/acme/-/drawer/dns/" + cs[0].ID
	if rec := s.As(t, member, "POST", base+"/domains", nil); rec.Code != http.StatusForbidden {
		t.Errorf("member domains = %d, want 403", rec.Code)
	}
	rec = s.Do(t, "POST", base+"/manage", url.Values{"domain": {"acme.dev"}})
	if body := rec.Body.String(); rec.Code != http.StatusOK || !strings.Contains(body, "Managed") || strings.Contains(body, "Available to") {
		t.Fatalf("manage = %d\n%s", rec.Code, body)
	}
	if got, _ := s.Orch.UsableDNSDomains(ctx, s.Org); len(got) != 1 {
		t.Errorf("usable = %v", got)
	}
	rec = s.Do(t, "POST", "/acme/-/drawer/connectors/"+cs[0].ID+"/delete", nil)
	if cs, _ := s.Orch.DNSConnectors(ctx, &s.Org); rec.Code != http.StatusOK || len(cs) != 0 {
		t.Errorf("delete = %d, left %v", rec.Code, cs)
	}
}

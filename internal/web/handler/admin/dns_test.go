package admin_test

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
	"github.com/FyrmForge/stackr/internal/web/webtest"
)

const panelKey = "panel-key-never-shown-31"

// Admin > Connectors adds a Namecheap account (the key never shown back,
// a refusal keeps the user and IP), lists its domains in three states,
// manages one and says which orgs may use it. An owner reaches none of it.
func TestAdminDNSConnectors(t *testing.T) {
	s := webtest.New(t)
	ctx := context.Background()
	s.DNS.Zones = []service.DNSZone{{Name: "acme.dev", OurDNS: true}, {Name: "shop.io", OurDNS: true}, {Name: "away.com"}}
	root := s.Session(t, s.User(t, "root@x.test", true))

	rec := s.As(t, root, "GET", "/-/admin?tab=connectors", nil)
	for _, want := range []string{"Add a DNS connector: Namecheap", "Controls your whole Namecheap account.",
		"Add this IP to Namecheap&#39;s API allow list.", "Namecheap API Access", `hx-post="/-/admin/dns/namecheap"`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("tab has no %q\n%s", want, rec.Body)
		}
	}

	bad := url.Values{"user": {"jo"}, "key": {servicetest.BadKey}, "ip": {"203.0.113.7"}}
	rec = s.As(t, root, "POST", "/-/admin/dns/namecheap", bad)
	if body := rec.Body.String(); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(body, "refused the API key") ||
		!strings.Contains(body, `value="jo"`) || !strings.Contains(body, `value="203.0.113.7"`) || strings.Contains(body, servicetest.BadKey) {
		t.Fatalf("bad key = %d\n%s", rec.Code, body)
	}

	rec = s.As(t, root, "POST", "/-/admin/dns/namecheap", url.Values{"user": {"jo"}, "key": {panelKey}, "ip": {"203.0.113.7"}})
	if body := rec.Body.String(); rec.Code != http.StatusOK || !strings.Contains(body, "Namecheap · jo") || strings.Contains(body, panelKey) {
		t.Fatalf("add = %d\n%s", rec.Code, body)
	}
	cs, _ := s.Orch.DNSConnectors(ctx, nil)
	if len(cs) != 1 {
		t.Fatalf("connectors = %v", cs)
	}
	base := "/-/admin/dns/" + cs[0].ID

	rec = s.As(t, root, "POST", base+"/domains", nil)
	if body := rec.Body.String(); rec.Code != http.StatusOK || !strings.Contains(body, "Not managed") ||
		!strings.Contains(body, "DNS elsewhere") || !strings.Contains(body, "Take over DNS") || strings.Contains(body, panelKey) {
		t.Fatalf("domains = %d\n%s", rec.Code, body)
	}
	rec = s.As(t, root, "POST", base+"/manage", url.Values{"domain": {"shop.io"}})
	if body := rec.Body.String(); rec.Code != http.StatusOK || !strings.Contains(body, "Managed") || !strings.Contains(body, "Available to") {
		t.Fatalf("manage = %d\n%s", rec.Code, body)
	}
	rec = s.As(t, root, "POST", base+"/manage", url.Values{"domain": {"shop.io"}, "org": {s.Org}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `value="`+s.Org+`" checked`) {
		t.Fatalf("share = %d\n%s", rec.Code, rec.Body)
	}
	if got, _ := s.Orch.UsableDNSDomains(ctx, s.Org); !slices.Equal(got, []string{"shop.io"}) {
		t.Errorf("usable = %v", got)
	}
	rec = s.As(t, root, "POST", base+"/takeover", url.Values{"domain": {"away.com"}, "confirm": {"nope"}})
	if rec.Code != http.StatusUnprocessableEntity || len(s.DNS.TookOver) != 0 {
		t.Errorf("take over, wrong word = %d, took over %v", rec.Code, s.DNS.TookOver)
	}

	for _, p := range []string{"/-/admin/dns/namecheap", base + "/domains", base + "/manage", base + "/takeover"} {
		if rec := s.Do(t, "POST", p, url.Values{"domain": {"acme.dev"}}); rec.Code != http.StatusForbidden {
			t.Errorf("owner POST %s = %d, want 403", p, rec.Code)
		}
	}
}

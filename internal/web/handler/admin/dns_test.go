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

	// Namecheap's screens: four steps in one form, no fake IP
	rec := s.As(t, root, "GET", "/-/admin/connectors/new/namecheap", nil)
	for _, want := range []string{"Step 1 of 5: API access", "Step 2 of 5: Account", "Step 3 of 5: Allow list", "Step 4 of 5: Check",
		"Controls your whole Namecheap account.", "Add this IP to Namecheap&#39;s API allow list.", "Namecheap API Access",
		`hx-post="/-/admin/connectors/new/namecheap" hx-target="#connector-dialog-body"`, `hx-get="/-/admin/connectors/new?pick=1"`, "data-next", "data-back"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("Namecheap screens have no %q\n%s", want, rec.Body)
		}
	}
	if strings.Contains(rec.Body.String(), "203.0.113.7") {
		t.Errorf("the IP field carries a made-up address:\n%s", rec.Body)
	}

	// an empty submit names every field, and opens on the first one's step
	rec = s.As(t, root, "POST", "/-/admin/connectors/new/namecheap", url.Values{})
	if body := rec.Body.String(); rec.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(body, "Check these fields: Namecheap username, API key, Server IPv4.") ||
		!strings.Contains(body, "Enter the Namecheap username") || !strings.Contains(body, "Enter the API key") ||
		!strings.Contains(body, "Namecheap needs this server&#39;s public IPv4 address") ||
		!strings.Contains(body, `<section data-step hidden>`) || strings.Count(body, `<section data-step>`) != 1 ||
		!strings.Contains(body, `<section data-step><div class="modal-body"><p class="text-xs text-rw-faint">Step 2 of 5`) {
		t.Fatalf("empty add = %d\n%s", rec.Code, body)
	}

	bad := url.Values{"user": {"jo"}, "key": {servicetest.BadKey}, "client_ip": {"203.0.113.7"}}
	rec = s.As(t, root, "POST", "/-/admin/connectors/new/namecheap", bad)
	if body := rec.Body.String(); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(body, "refused the API key") ||
		!strings.Contains(body, `value="jo"`) || !strings.Contains(body, `value="203.0.113.7"`) || strings.Contains(body, servicetest.BadKey) {
		t.Fatalf("bad key = %d\n%s", rec.Code, body)
	}

	// connected: the dialog's last step, its domains
	rec = s.As(t, root, "POST", "/-/admin/connectors/new/namecheap", url.Values{"user": {"jo"}, "key": {panelKey}, "client_ip": {"203.0.113.7"}})
	if body := rec.Body.String(); rec.Code != http.StatusOK || !strings.Contains(body, "Step 5 of 5: Domains") ||
		!strings.Contains(body, "/domains") || strings.Contains(body, panelKey) {
		t.Fatalf("add = %d\n%s", rec.Code, body)
	}
	cs, _ := s.Orch.DNSConnectors(ctx, nil)
	if len(cs) != 1 {
		t.Fatalf("connectors = %v", cs)
	}
	base := "/-/admin/dns/" + cs[0].ID

	// the list: a row with its summary and cogwheel, no domains inline
	body := s.As(t, root, "GET", "/-/admin?tab=connectors", nil).Body.String()
	if !strings.Contains(body, "Namecheap · jo") || !strings.Contains(body, "No managed domains") ||
		!strings.Contains(body, `hx-get="/-/admin/connectors/`+cs[0].ID+`/settings"`) || strings.Contains(body, base+"/domains") {
		t.Fatalf("tab:\n%s", body)
	}
	// the cogwheel: the domain list and Remove
	body = s.As(t, root, "GET", "/-/admin/connectors/"+cs[0].ID+"/settings", nil).Body.String()
	if !strings.Contains(body, "<confirm-dialog open>") || !strings.Contains(body, `hx-post="`+base+`/domains" hx-trigger="load"`) ||
		!strings.Contains(body, `hx-post="/-/admin/connectors/`+cs[0].ID+`/delete"`) {
		t.Fatalf("settings:\n%s", body)
	}

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
	if body := s.As(t, root, "GET", "/-/admin?tab=connectors", nil).Body.String(); !strings.Contains(body, "1 managed domain") {
		t.Errorf("the summary does not count the managed domain:\n%s", body)
	}
	rec = s.As(t, root, "POST", base+"/takeover", url.Values{"domain": {"away.com"}, "confirm": {"nope"}})
	if rec.Code != http.StatusUnprocessableEntity || len(s.DNS.TookOver) != 0 || !strings.Contains(rec.Body.String(), ">Type away.com to confirm<") {
		t.Errorf("take over, wrong word = %d, took over %v", rec.Code, s.DNS.TookOver)
	}

	for _, p := range []string{"/-/admin/connectors/new/namecheap", base + "/domains", base + "/manage", base + "/takeover"} {
		if rec := s.Do(t, "POST", p, url.Values{"domain": {"acme.dev"}}); rec.Code != http.StatusForbidden {
			t.Errorf("owner POST %s = %d, want 403", p, rec.Code)
		}
	}
	for _, p := range []string{"/-/admin/connectors/new/namecheap", "/-/admin/connectors/" + cs[0].ID + "/settings"} {
		if rec := s.Do(t, "GET", p, nil); rec.Code != http.StatusForbidden {
			t.Errorf("owner GET %s = %d, want 403", p, rec.Code)
		}
	}
}

package service_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/libdns/libdns"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// scopedRig is dnsRig with the controller on 198.51.100.1, the panel at
// stackr.acme.dev, and acme.dev managed only under stackr.acme.dev.
// Someone else's @, www and MX records sit in the zone first.
func scopedRig(t *testing.T) (*servicetest.Env, string, string) {
	t.Helper()
	env, acme, _ := dnsRig(t)
	ctx := context.Background()
	setIP(t, env, "198.51.100.1")
	env.DNS.Put("acme.dev", a("@", "203.0.113.9"), a("www", "203.0.113.9"),
		libdns.RR{Name: "@", Type: "MX", Data: "10 mail.acme.dev", TTL: time.Hour})
	if err := env.Orch.SetSettings(ctx, map[string]string{"panel_domain": "stackr.acme.dev"}); err != nil {
		t.Fatal(err)
	}
	srv, err := env.Orch.AddConnector(ctx, service.ConnectorAdd{OrgID: nil, Kind: "namecheap", Values: account()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Orch.ManageDNSDomainAdopting(ctx, nil, srv.Connector.ID, "acme.dev", "stackr.acme.dev", nil, nil); err != nil {
		t.Fatal(err)
	}
	env.Orch.WaitDNS()
	return env, acme, srv.Connector.ID
}


// Under a scope, only names at or under it get records: the panel's, a
// node's and a tile's there. A tile host elsewhere on the domain is not
// managed, and the records outside the scope are never touched.
func TestScopedDomainWritesOnlyUnderScope(t *testing.T) {
	t.Parallel()
	env, acme, _ := scopedRig(t)
	ctx := context.Background()
	if got, _ := env.Orch.UsableDNSDomains(ctx, acme); !slices.Equal(got, []string{"stackr.acme.dev"}) {
		t.Errorf("usable = %v, want the scope", got)
	}
	tl := env.Tile(t, acme)
	if _, err := env.Orch.AttachDomain(ctx, tl.ID, service.DomainSpec{Host: "shop.stackr.acme.dev"}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Orch.AttachDomain(ctx, tl.ID, service.DomainSpec{Host: "blog.acme.dev"}); err != nil {
		t.Fatal(err)
	}
	env.Orch.WaitDNS()
	z := env.DNS.Zone("acme.dev")
	for _, n := range []string{"stackr", "shop.stackr"} {
		if !has(z, n, "A", "198.51.100.1") {
			t.Errorf("no record at %s: %v", n, z)
		}
	}
	if slices.ContainsFunc(z, func(r libdns.RR) bool { return r.Name == "blog" }) {
		t.Errorf("a host outside the scope got a record: %v", z)
	}
	if !has(z, "@", "A", "203.0.113.9") || !has(z, "www", "A", "203.0.113.9") || !has(z, "@", "MX", "10 mail.acme.dev") {
		t.Errorf("someone else's records changed: %v", z)
	}
}

// The safety net: a write that would delete a record outside the scope is
// refused whole, and nothing of it reaches the provider, not even the
// in-scope part.
func TestSafetyNetRefusesWriteOutsideScope(t *testing.T) {
	t.Parallel()
	env, acme, _ := scopedRig(t)
	ctx := context.Background()
	d, err := env.Store.DNSDomains.GetByName(ctx, "acme.dev")
	if err != nil {
		t.Fatal(err)
	}
	// a bug's leftover: a row claiming www, outside the scope, as stackr's
	if err := env.Store.DNSRecords.Create(ctx, store.DNSRecord{
		ID: uuid.NewString(), DNSDomainID: &d.ID, Zone: "acme.dev", Name: "www", Type: "A",
		Value: "203.0.113.9", TTL: 300, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	appended, deleted, _ := env.DNS.Log()
	tl := env.Tile(t, acme)
	if _, err := env.Orch.AttachDomain(ctx, tl.ID, service.DomainSpec{Host: "shop.stackr.acme.dev"}); err == nil {
		t.Fatal("the attach's sync wrote past the safety net")
	} else if !errors.Is(err, errs.ErrRefused) {
		t.Fatalf("err = %v, want a refusal", err)
	}
	a2, d2, _ := env.DNS.Log()
	if len(a2) != len(appended) || len(d2) != len(deleted) {
		t.Errorf("refused write still called the provider: appended %v deleted %v", a2[len(appended):], d2[len(deleted):])
	}
	if z := env.DNS.Zone("acme.dev"); !has(z, "www", "A", "203.0.113.9") || slices.ContainsFunc(z, func(r libdns.RR) bool { return r.Name == "shop.stackr" }) {
		t.Errorf("zone after a refused write: %v", z)
	}
}

// The manage plan lists what stays untouched outside the scope, writes
// the panel's record, and ticks an existing panel record pointing at us.
func TestManagePlanLeftAloneAndPanel(t *testing.T) {
	t.Parallel()
	env, _, _ := dnsRig(t)
	ctx := context.Background()
	setIP(t, env, "198.51.100.1")
	env.DNS.Put("acme.dev", a("www", "203.0.113.9"), a("stackr", "198.51.100.1"))
	if err := env.Orch.SetSettings(ctx, map[string]string{"panel_domain": "stackr.acme.dev"}); err != nil {
		t.Fatal(err)
	}
	srv, err := env.Orch.AddConnector(ctx, service.ConnectorAdd{OrgID: nil, Kind: "namecheap", Values: account()})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := env.Orch.PlanManageDNSDomain(ctx, nil, srv.Connector.ID, "acme.dev", "stackr.acme.dev", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.LeftAlone) != 1 || plan.LeftAlone[0].Host != "www.acme.dev" {
		t.Errorf("left alone = %v", plan.LeftAlone)
	}
	if len(plan.Adopt) != 1 || plan.Adopt[0].Name != "stackr" || !plan.Adopt[0].Serves {
		t.Errorf("adopt = %+v, want the panel's record ticked", plan.Adopt)
	}
	if _, err := env.Orch.PlanManageDNSDomain(ctx, nil, srv.Connector.ID, "acme.dev", "stackr.other.dev", nil); err == nil {
		t.Error("a scope off the domain was planned")
	}
	whole, err := env.Orch.PlanManageDNSDomain(ctx, nil, srv.Connector.ID, "acme.dev", "", nil)
	if err != nil || len(whole.LeftAlone) != 0 {
		t.Errorf("whole domain left alone = %v, %v", whole.LeftAlone, err)
	}
}

// A managed domain keeps its scope: re-saving who may use it does not
// widen it, and asking for another scope is refused.
func TestScopeIsSetOnce(t *testing.T) {
	t.Parallel()
	env, _, id := scopedRig(t)
	ctx := context.Background()
	if _, err := env.Orch.ManageDNSDomain(ctx, nil, id, "acme.dev", nil); err != nil {
		t.Fatal(err)
	}
	ds, _ := env.Orch.DNSDomains(ctx, nil, id)
	i := slices.IndexFunc(ds, func(d service.DNSDomain) bool { return d.Name == "acme.dev" })
	if i < 0 || ds[i].Scope != "stackr.acme.dev" {
		t.Errorf("after a share re-save: %+v", ds)
	}
	if _, err := env.Orch.ManageDNSDomainAdopting(ctx, nil, id, "acme.dev", "apps.acme.dev", nil, nil); err == nil {
		t.Error("another scope on a managed domain was taken")
	}
}

// An ACME challenge outside the scope is refused before any provider call.
func TestACMEChallengeStaysInScope(t *testing.T) {
	t.Parallel()
	env, _, _ := scopedRig(t)
	ctx := context.Background()
	txt := []libdns.RR{{Name: "_acme-challenge.www", Type: "TXT", Data: "tok"}}
	if _, err := env.Orch.ACMEChallenge(ctx, false, "acme.dev", txt); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("out of scope: err = %v", err)
	}
	txt[0].Name = "_acme-challenge.stackr"
	if _, err := env.Orch.ACMEChallenge(ctx, false, "acme.dev", txt); err != nil {
		t.Errorf("in scope: %v", err)
	}
}

// Domains suggests managing the panel's part of a domain on the account.
func TestDNSSuggestions(t *testing.T) {
	t.Parallel()
	env, _, _ := dnsRig(t)
	ctx := context.Background()
	if err := env.Orch.SetSettings(ctx, map[string]string{"panel_domain": "stackr.acme.dev"}); err != nil {
		t.Fatal(err)
	}
	ds := []service.DNSDomain{{Name: "acme.dev", State: service.DNSNotManaged}, {Name: "shop.io", State: service.DNSNotManaged}}
	got := env.Orch.DNSSuggestions(ctx, nil, ds)
	if len(got) != 1 || got[0].Domain != "acme.dev" || got[0].Sub != "stackr" || !got[0].Panel {
		t.Errorf("suggestions = %+v", got)
	}
	org := "o"
	if got := env.Orch.DNSSuggestions(ctx, &org, ds); len(got) != 0 {
		t.Errorf("an org connector got %+v", got)
	}
}

// Stop managing deletes the tile records stackr made but keeps the
// panel's: the panel stays reachable by name.
func TestUnmanageKeepsPanelRecord(t *testing.T) {
	t.Parallel()
	env, _, id := scopedRig(t)
	ctx := context.Background()
	if !has(env.DNS.Zone("acme.dev"), "stackr", "A", "198.51.100.1") {
		t.Fatalf("no panel record: %v", env.DNS.Zone("acme.dev"))
	}
	if err := env.Orch.UnmanageDNSDomain(ctx, nil, id, "acme.dev"); err != nil {
		t.Fatal(err)
	}
	if !has(env.DNS.Zone("acme.dev"), "stackr", "A", "198.51.100.1") {
		t.Errorf("stop managing took the panel's record: %v", env.DNS.Zone("acme.dev"))
	}
	if rs, _ := env.Store.DNSRecords.List(ctx); len(rs) != 0 {
		t.Errorf("rows left: %v", rs)
	}
}

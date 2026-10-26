package service_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/libdns/libdns"

	"github.com/FyrmForge/stackr/internal/authz"
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

func adminOf(t *testing.T, env *servicetest.Env) *service.Principal {
	id := env.User(t, "root@acme.dev", true)
	return &service.Principal{User: service.User{ID: id}, Access: authz.User{ID: id, Active: true, Admin: true}}
}

// Under a scope, only names at or under it get records: the panel's, a
// node's and a tile's there. A tile host elsewhere on the domain is not
// managed, and the records outside the scope are never touched.
func TestScopedDomainWritesOnlyUnderScope(t *testing.T) {
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
	ctl, _ := env.Store.Nodes.List(ctx)
	if _, err := env.Orch.SetNodeAddress(ctx, adminOf(t, env), ctl[0].ID, "n1.stackr.acme.dev"); err != nil {
		t.Fatal(err)
	}
	env.Orch.WaitDNS()
	z := env.DNS.Zone("acme.dev")
	for _, n := range []string{"stackr", "shop.stackr", "n1.stackr"} {
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

// A node's address outside every managed scope is the record to set by
// hand; inside one stackr writes it. No public address, no record.
func TestNodeAddressRecord(t *testing.T) {
	env, _, _ := scopedRig(t)
	ctx := context.Background()
	adm := adminOf(t, env)
	ns, _ := env.Store.Nodes.List(ctx)
	ctl := ns[0]
	if ctl.Address != "n1.stackr.acme.dev" { // set at the panel's save: the first node's default
		t.Errorf("controller address = %q", ctl.Address)
	}
	if _, err := env.Orch.SetNodeAddress(ctx, adm, ctl.ID, "box1.lan.example.com"); err != nil {
		t.Fatal(err)
	}
	d, err := env.Orch.NodeDNSOf(ctx, ctl.ID)
	if err != nil || d.Managed || len(d.Records) != 1 || d.Records[0].Value != "198.51.100.1" {
		t.Errorf("outside a scope: %+v %v", d, err)
	}
	if _, err := env.Orch.SetNodeAddress(ctx, adm, ctl.ID, "stackr.acme.dev"); err == nil {
		t.Error("the panel's own address was taken")
	}
	if _, err := env.Orch.SetNodeAddress(ctx, adm, ctl.ID, "n1.stackr.acme.dev"); err != nil {
		t.Fatal(err)
	}
	env.Orch.WaitDNS()
	if d, _ := env.Orch.NodeDNSOf(ctx, ctl.ID); !d.Managed {
		t.Errorf("inside the scope: %+v", d)
	}
	if !has(env.DNS.Zone("acme.dev"), "n1.stackr", "A", "198.51.100.1") {
		t.Errorf("no node record: %v", env.DNS.Zone("acme.dev"))
	}
	setIP(t, env, "")
	if d, _ := env.Orch.NodeDNSOf(ctx, ctl.ID); !d.NoPublic || len(d.Records) != 0 {
		t.Errorf("no public address: %+v", d)
	}
}

// Domains suggests managing the panel's part of a domain on the account.
func TestDNSSuggestions(t *testing.T) {
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

package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// pageRig seeds three managed domains straight into stackr's tables:
// acme.dev (server, shared with every org), only.dev (server, shared with
// other only) and own.dev (acme's own connector, scoped under test), and
// records of every kind on acme.dev and own.dev. No provider is asked.
func pageRig(t *testing.T) (env *servicetest.Env, acme, other string) {
	t.Helper()
	env, acme, other = dnsRig(t)
	ctx := context.Background()
	if err := env.Orch.SetSetting(ctx, "panel_domain", "panel.acme.dev"); err != nil {
		t.Fatal(err)
	}
	srv, err := env.Orch.AddConnector(ctx, service.ConnectorAdd{Kind: "namecheap", Values: account()})
	if err != nil {
		t.Fatal(err)
	}
	mine, err := env.Orch.AddConnector(ctx, service.ConnectorAdd{OrgID: &acme, Kind: "namecheap", Values: account()})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	zone := func(name, conn string, orgID *string, all bool, orgs []string, scope string) string {
		id := uuid.NewString()
		if err := env.Store.DNSDomains.Create(ctx, store.DNSDomain{
			ID: id, ConnectorID: conn, OrgID: orgID, Name: name, ShareAll: all, OrgIDs: orgs, Scope: scope, CreatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	acmeDev := zone("acme.dev", srv.Connector.ID, nil, true, nil, "")
	zone("only.dev", srv.Connector.ID, nil, false, []string{other}, "")
	ownDev := zone("own.dev", mine.Connector.ID, &acme, false, nil, "test.own.dev")

	shop := env.Domain(t, env.Tile(t, acme).ID, "shop.acme.dev")
	blog := env.Domain(t, env.Tile(t, other).ID, "blog.acme.dev")
	rec := func(zid, zname, name, typ, value, kind string, domainID *string, mod func(*store.DNSRecord)) {
		r := store.DNSRecord{
			ID: uuid.NewString(), DNSDomainID: &zid, Zone: zname, Name: name, Type: typ, Value: value,
			TTL: 300, DomainID: domainID, Kind: kind, CreatedAt: now,
		}
		if mod != nil {
			mod(&r)
		}
		if err := env.Store.DNSRecords.Create(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	rec(acmeDev, "acme.dev", "shop", "A", "198.51.100.1", store.RecordApp, &shop, nil)
	rec(acmeDev, "acme.dev", "shop", "TXT", "v=shop", store.RecordCustom, &shop, func(r *store.DNSRecord) { r.PutBackAt = &now })
	rec(acmeDev, "acme.dev", "blog", "A", "198.51.100.1", store.RecordApp, &blog, nil)
	rec(acmeDev, "acme.dev", "@", "MX", "10 mx.acme.dev", store.RecordCustom, nil, nil)
	rec(acmeDev, "acme.dev", "old", "A", "198.51.100.1", store.RecordApp, nil, nil)
	rec(acmeDev, "acme.dev", "panel", "A", "198.51.100.1", store.RecordName, nil, nil)
	rec(acmeDev, "acme.dev", "n1", "A", "198.51.100.1", store.RecordName, nil, nil)
	rec(acmeDev, "acme.dev", "gone", "A", "198.51.100.1", store.RecordApp, nil, func(r *store.DNSRecord) { r.HandedBackAt = &now })
	rec(ownDev, "own.dev", "@", "TXT", "v=own", store.RecordCustom, nil, nil)
	rec(ownDev, "own.dev", "stale.test", "A", "198.51.100.1", store.RecordApp, nil, nil)
	return env, acme, other
}

// whose is a domain's records as name type → whose (drift appended).
func whose(d service.ManagedDomain) map[string]string {
	out := map[string]string{}
	for _, r := range d.Records {
		out[r.Name+" "+r.Type] = r.Whose + r.Drift
	}
	return out
}

func domainOf(t *testing.T, ds []service.ManagedDomain, name string) (service.ManagedDomain, bool) {
	t.Helper()
	for _, d := range ds {
		if d.Name == name {
			return d, true
		}
	}
	return service.ManagedDomain{}, false
}

// An admin sees every domain and every record, each classified.
func TestManagedDomainsClassify(t *testing.T) {
	t.Parallel()
	env, _, _ := pageRig(t)
	ds, err := env.Orch.ManagedDomains(context.Background(), nil)
	if err != nil || len(ds) != 3 {
		t.Fatalf("domains = %+v %v", ds, err)
	}
	d, _ := domainOf(t, ds, "acme.dev")
	want := map[string]string{
		"shop.acme.dev A":   "app",
		"shop.acme.dev TXT": "appChanged outside stackr; put back",
		"blog.acme.dev A":   "app",
		"acme.dev MX":       "domain",
		"old.acme.dev A":    "unused",
		"panel.acme.dev A":  "panel",
		"n1.acme.dev A":     "node",
		"gone.acme.dev A":   "handed_back",
	}
	got := whose(d)
	if len(got) != len(want) {
		t.Fatalf("acme.dev records = %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if d.ScopeText != "All of acme.dev" || d.Via != "via Namecheap (server)" || !d.AllOrgs || !d.CanEdit {
		t.Errorf("acme.dev row = %+v", d)
	}
	for _, r := range d.Records {
		if r.Name == "shop.acme.dev" && (r.Tile == nil || r.Host != "shop.acme.dev") {
			t.Errorf("app record without its tile: %+v", r)
		}
	}
	own, _ := domainOf(t, ds, "own.dev")
	if own.ScopeText != "Only under test" || own.Via != "via Namecheap (org)" || len(own.Orgs) != 1 || own.Orgs[0] != "acme" {
		t.Errorf("own.dev row = %+v", own)
	}
}

// An org sees the domains it may use, its own tiles' records on them,
// the domain's and unused records only on its own connectors' domains,
// and never the panel's or nodes' addresses nor another org's name.
func TestManagedDomainsOrgFilter(t *testing.T) {
	t.Parallel()
	env, acme, other := pageRig(t)
	ctx := context.Background()

	ds, err := env.Orch.ManagedDomains(ctx, &acme)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := domainOf(t, ds, "only.dev"); ok || len(ds) != 2 {
		t.Fatalf("acme sees %+v", ds)
	}
	d, _ := domainOf(t, ds, "acme.dev")
	if got := whose(d); len(got) != 2 || got["shop.acme.dev A"] != "app" || got["shop.acme.dev TXT"] == "" {
		t.Errorf("acme on acme.dev = %v", got)
	}
	if d.CanEdit || d.AllOrgs || d.Orgs != nil {
		t.Errorf("acme's view of a server domain = %+v", d)
	}
	own, _ := domainOf(t, ds, "own.dev")
	if got := whose(own); len(got) != 2 || got["own.dev TXT"] != "domain" || got["stale.test.own.dev A"] != "unused" || !own.CanEdit {
		t.Errorf("acme on own.dev = %v %+v", got, own)
	}

	ds, err = env.Orch.ManagedDomains(ctx, &other)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := domainOf(t, ds, "own.dev"); ok || len(ds) != 2 {
		t.Fatalf("other sees %+v", ds)
	}
	d, _ = domainOf(t, ds, "acme.dev")
	if got := whose(d); len(got) != 1 || got["blog.acme.dev A"] != "app" {
		t.Errorf("other on acme.dev = %v", got)
	}
	if _, err := env.Orch.ManagedDomainNamed(ctx, &other, "own.dev"); err != errs.ErrNotFound {
		t.Errorf("other reads own.dev: %v", err)
	}
}

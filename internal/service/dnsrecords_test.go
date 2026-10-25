package service_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/libdns/libdns"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// recordsRig is dnsRig with acme.dev managed through a server connector
// for every org, the controller's records pointing at 198.51.100.1, and
// acme's shop/dev/api tile.
func recordsRig(t *testing.T) (*servicetest.Env, string, string, servicetest.Tile) {
	t.Helper()
	env, acme, other := dnsRig(t)
	ctx := context.Background()
	setIP(t, env, "198.51.100.1")
	srv, err := env.Orch.AddConnector(ctx, service.ConnectorAdd{OrgID: nil, Kind: "namecheap", Values: account()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Orch.ManageDNSDomain(ctx, nil, srv.Connector.ID, "acme.dev", nil); err != nil {
		t.Fatal(err)
	}
	return env, acme, other, env.Tile(t, acme)
}

func setIP(t *testing.T, env *servicetest.Env, ip string) {
	t.Helper()
	ctx := context.Background()
	ns, err := env.Store.Nodes.List(ctx)
	if err != nil || len(ns) == 0 {
		t.Fatalf("nodes %v %v", ns, err)
	}
	ns[0].PublicIP = ip
	if err := env.Store.Nodes.Update(ctx, ns[0]); err != nil {
		t.Fatal(err)
	}
}

func a(name, ip string) libdns.RR {
	return libdns.RR{Name: name, Type: "A", Data: ip, TTL: 300 * time.Second}
}

func has(rs []libdns.RR, name, typ, data string) bool {
	return slices.ContainsFunc(rs, func(r libdns.RR) bool { return r.Name == name && r.Type == typ && r.Data == data })
}

// A tile domain in a managed domain gets its A record; a repoint follows
// the node's address; removing the domain deletes it. Someone else's
// records in the zone are never touched, and a sync with nothing to change
// calls no provider.
func TestRecordLifecycle(t *testing.T) {
	env, _, _, tl := recordsRig(t)
	ctx := context.Background()
	foreign := []libdns.RR{a("@", "203.0.113.9"), a("www", "203.0.113.9"), a("*", "203.0.113.9")}
	env.DNS.Put("acme.dev", foreign...)

	d, err := env.Orch.AttachDomain(ctx, tl.ID, service.DomainSpec{Host: "shop.acme.dev"})
	if err != nil {
		t.Fatal(err)
	}
	if z := env.DNS.Zone("acme.dev"); !has(z, "shop", "A", "198.51.100.1") || len(z) != 4 {
		t.Fatalf("after add: %v", z)
	}
	env.Orch.WaitDNS() // the push's own records sync: nothing left to do
	calls := env.DNS.Calls
	if err := env.Orch.SyncProxy(ctx); err != nil {
		t.Fatal(err)
	}
	env.Orch.WaitDNS()
	if env.DNS.Calls != calls {
		t.Errorf("a sync with nothing to change made %d provider calls", env.DNS.Calls-calls)
	}

	// repoint: the controller's address moves (a floating IP, say)
	setIP(t, env, "198.51.100.2")
	if err := env.Orch.SyncProxy(ctx); err != nil {
		t.Fatal(err)
	}
	env.Orch.WaitDNS()
	if z := env.DNS.Zone("acme.dev"); !has(z, "shop", "A", "198.51.100.2") || has(z, "shop", "A", "198.51.100.1") {
		t.Fatalf("after repoint: %v", z)
	}

	// the node loses its address (override cleared, nothing detected): the
	// record stays as it was, never deleted over a missing address
	setIP(t, env, "")
	if err := env.Orch.SyncProxy(ctx); err != nil {
		t.Fatal(err)
	}
	env.Orch.WaitDNS()
	if z := env.DNS.Zone("acme.dev"); !has(z, "shop", "A", "198.51.100.2") {
		t.Fatalf("a missing address deleted the record: %v", z)
	}

	if err := env.Orch.DetachDomain(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	env.Orch.WaitDNS()
	z := env.DNS.Zone("acme.dev")
	if len(z) != 3 {
		t.Errorf("after remove: %v", z)
	}
	for _, f := range foreign {
		if !has(z, f.Name, f.Type, f.Data) {
			t.Errorf("someone else's %s record went", f.Name)
		}
	}
	if rs, _ := env.Store.DNSRecords.List(ctx); len(rs) != 0 {
		t.Errorf("records table after remove: %v", rs)
	}
}

// A name with a record stackr did not make is refused, naming the host;
// replace takes it over and tracks it, so removing the domain deletes it.
func TestRecordReplace(t *testing.T) {
	env, _, _, tl := recordsRig(t)
	ctx := context.Background()
	env.DNS.Put("acme.dev", a("shop", "203.0.113.9"), libdns.RR{Name: "shop", Type: "TXT", Data: "keep"})

	_, err := env.Orch.AttachDomain(ctx, tl.ID, service.DomainSpec{Host: "shop.acme.dev"})
	var taken *service.RecordTaken
	if !errors.As(err, &taken) || !slices.Equal(taken.Hosts, []string{"shop.acme.dev"}) {
		t.Fatalf("want *RecordTaken for shop.acme.dev, got %v", err)
	}
	if ds, _ := env.Orch.Domains(ctx, tl.ID); len(ds) != 0 {
		t.Fatalf("a refused domain was attached: %v", ds)
	}
	if z := env.DNS.Zone("acme.dev"); !has(z, "shop", "A", "203.0.113.9") {
		t.Fatalf("the refusal touched the record: %v", z)
	}

	d, err := env.Orch.AttachDomainReplace(ctx, tl.ID, service.DomainSpec{Host: "shop.acme.dev"}, true)
	if err != nil {
		t.Fatal(err)
	}
	z := env.DNS.Zone("acme.dev")
	if !has(z, "shop", "A", "198.51.100.1") || has(z, "shop", "A", "203.0.113.9") || !has(z, "shop", "TXT", "keep") {
		t.Fatalf("after replace: %v", z)
	}
	if err := env.Orch.DetachDomain(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	env.Orch.WaitDNS()
	if z := env.DNS.Zone("acme.dev"); has(z, "shop", "A", "198.51.100.1") || !has(z, "shop", "TXT", "keep") {
		t.Errorf("after remove: %v", z)
	}
}

// Org B can't put a tile under org A's own domain, nor under a server
// domain not available to it: neither by the list nor by typing the host.
func TestRecordOrgCannotUseOthersZone(t *testing.T) {
	env, acme, other := dnsRig(t)
	ctx := context.Background()
	setIP(t, env, "198.51.100.1")
	mine, err := env.Orch.AddConnector(ctx, service.ConnectorAdd{OrgID: &acme, Kind: "namecheap", Values: account()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Orch.ManageDNSDomain(ctx, &acme, mine.Connector.ID, "acme.dev", nil); err != nil {
		t.Fatal(err)
	}
	srv, err := env.Orch.AddConnector(ctx, service.ConnectorAdd{OrgID: nil, Kind: "namecheap", Values: account()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Orch.ManageDNSDomain(ctx, nil, srv.Connector.ID, "shop.io", []string{acme}); err != nil {
		t.Fatal(err)
	}
	theirs := env.Tile(t, other)
	if got, _ := env.Orch.TileDNSDomains(ctx, theirs.ID); len(got) != 0 {
		t.Errorf("other's list = %v", got)
	}
	for _, host := range []string{"x.acme.dev", "acme.dev", "x.shop.io"} {
		if _, err := env.Orch.AttachDomain(ctx, theirs.ID, service.DomainSpec{Host: host}); err == nil {
			t.Errorf("other attached %s", host)
		}
	}
	if z := env.DNS.Zone("acme.dev"); len(z) != 0 {
		t.Errorf("records made for another org: %v", z)
	}
	ours := env.Tile(t, acme)
	if got, _ := env.Orch.TileDNSDomains(ctx, ours.ID); !slices.Equal(got, []string{"acme.dev", "shop.io"}) {
		t.Errorf("acme's list = %v", got)
	}
	if _, err := env.Orch.AttachDomain(ctx, ours.ID, service.DomainSpec{Host: service.ManagedHost("shop.io", "x")}); err != nil {
		t.Errorf("acme on shop.io: %v", err)
	}
}

// Unmanaging, or deleting the connector, is refused while a tile uses the
// domain, naming it; once unused only stackr's records go, then the
// domain and the resource stackr made for it.
func TestUnmanageRefusedWhileUsed(t *testing.T) {
	env, _, _, tl := recordsRig(t)
	ctx := context.Background()
	env.DNS.Put("acme.dev", a("www", "203.0.113.9"))
	cs, _ := env.Orch.DNSConnectors(ctx, nil)
	id := cs[0].ID
	d, err := env.Orch.AttachDomain(ctx, tl.ID, service.DomainSpec{Host: "shop.acme.dev"})
	if err != nil {
		t.Fatal(err)
	}
	for name, err := range map[string]error{
		"unmanage": env.Orch.UnmanageDNSDomain(ctx, nil, id, "acme.dev"),
		"delete":   env.Orch.RemoveConnector(ctx, nil, id),
	} {
		if _, ok := errs.IsConflict(err); !ok || !strings.Contains(err.Error(), "shop.acme.dev") || !strings.Contains(err.Error(), "api") {
			t.Errorf("%s while used = %v, want a conflict naming the tile and host", name, err)
		}
	}
	rs, _ := env.Orch.AllDomainResources(ctx)
	i := slices.IndexFunc(rs, func(r service.DomainResource) bool { return r.Host == "acme.dev" })
	if i < 0 || rs[i].Declared {
		t.Fatalf("managed domain resource = %+v", rs)
	}
	if err := env.Orch.DeleteDomainResource(ctx, rs[i].ID); err == nil {
		t.Error("the managed domain's resource was deleted around its connector")
	}
	// a record that sneaked in while managed: stackr's own goes, this stays
	if err := env.Orch.DetachDomain(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	env.Orch.WaitDNS()
	if err := env.Orch.RemoveConnector(ctx, nil, id); err != nil {
		t.Fatal(err)
	}
	if z := env.DNS.Zone("acme.dev"); len(z) != 1 || !has(z, "www", "A", "203.0.113.9") {
		t.Errorf("zone after delete: %v", z)
	}
	rs, _ = env.Orch.AllDomainResources(ctx)
	if slices.ContainsFunc(rs, func(r service.DomainResource) bool { return r.Host == "acme.dev" }) {
		t.Error("the resource stackr made outlived its domain")
	}
}

// A zone that is already a domain resource elsewhere is refused.
func TestManageRefusesResourceElsewhere(t *testing.T) {
	env, acme, other := dnsRig(t)
	ctx := context.Background()
	if _, err := env.Orch.CreateDomainResource(ctx, "org", other, "shop.io", false, ""); err != nil {
		t.Fatal(err)
	}
	mine, err := env.Orch.AddConnector(ctx, service.ConnectorAdd{OrgID: &acme, Kind: "namecheap", Values: account()})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := errs.IsConflict(func() error {
		_, err := env.Orch.ManageDNSDomain(ctx, &acme, mine.Connector.ID, "shop.io", nil)
		return err
	}()); !ok {
		t.Error("managed a domain another org holds as its resource")
	}
	if us, _ := env.Orch.UsableDNSDomains(ctx, acme); len(us) != 0 {
		t.Errorf("refused, yet managed: %v", us)
	}
}

// A host unticked in a move is held: a sync leaves its record where it is,
// and repoints it again once released.
func TestHeldHostIsNotRepointed(t *testing.T) {
	env, _, _, tl := recordsRig(t)
	ctx := context.Background()
	if _, err := env.Orch.AttachDomain(ctx, tl.ID, service.DomainSpec{Host: "shop.acme.dev"}); err != nil {
		t.Fatal(err)
	}
	env.Orch.WaitDNS()
	if err := env.Store.Settings.Set(ctx, "dns_hold", "shop.acme.dev"); err != nil {
		t.Fatal(err)
	}
	setIP(t, env, "198.51.100.2")
	if err := env.Orch.SyncProxy(ctx); err != nil {
		t.Fatal(err)
	}
	env.Orch.WaitDNS()
	if z := env.DNS.Zone("acme.dev"); !has(z, "shop", "A", "198.51.100.1") || has(z, "shop", "A", "198.51.100.2") {
		t.Fatalf("a held host was repointed: %v", z)
	}
	if err := env.Orch.ReleaseDNS(ctx, []string{"shop.acme.dev"}); err != nil {
		t.Fatal(err)
	}
	if err := env.Orch.SyncProxy(ctx); err != nil {
		t.Fatal(err)
	}
	env.Orch.WaitDNS()
	if z := env.DNS.Zone("acme.dev"); !has(z, "shop", "A", "198.51.100.2") {
		t.Fatalf("a released host was not repointed: %v", z)
	}
}

// A node with a public IPv6 gets an AAAA record only while the IPv6
// setting is on.
func TestRecordAAAAFollowsIPv6Setting(t *testing.T) {
	env, _, _, tl := recordsRig(t)
	ctx := context.Background()
	setIP(t, env, "2001:db8::1")
	if _, err := env.Orch.AttachDomain(ctx, tl.ID, service.DomainSpec{Host: "shop.acme.dev"}); err == nil {
		t.Fatal("ipv6 off: a node with only an IPv6 address took a domain")
	}
	env.Orch.WaitDNS()
	if z := env.DNS.Zone("acme.dev"); has(z, "shop", "AAAA", "2001:db8::1") {
		t.Fatalf("ipv6 off wrote an AAAA: %v", z)
	}
	if err := env.Orch.SetSettings(ctx, map[string]string{"ipv6": "true"}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Orch.AttachDomain(ctx, tl.ID, service.DomainSpec{Host: "shop.acme.dev"}); err != nil {
		t.Fatal(err)
	}
	env.Orch.WaitDNS()
	if z := env.DNS.Zone("acme.dev"); !has(z, "shop", "AAAA", "2001:db8::1") {
		t.Fatalf("ipv6 on wrote no AAAA: %v", z)
	}
}

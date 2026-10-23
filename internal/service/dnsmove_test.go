package service

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/libdns/libdns"

	"github.com/FyrmForge/stackr/internal/authz"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/dnscap"
)

// zoneFake is one account holding acme.dev's records.
type zoneFake struct {
	mu sync.Mutex
	rs []libdns.RR
}

func (f *zoneFake) Zones(context.Context) ([]DNSZone, error) {
	return []DNSZone{{Name: "acme.dev", OurDNS: true}}, nil
}

func (f *zoneFake) Get(context.Context, string) ([]libdns.Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []libdns.Record
	for _, r := range f.rs {
		out = append(out, r)
	}
	return out, nil
}

func (f *zoneFake) Append(_ context.Context, _ string, rs []libdns.Record) ([]libdns.Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range rs {
		f.rs = append(f.rs, r.RR())
	}
	return rs, nil
}

func (f *zoneFake) Set(context.Context, string, []libdns.Record) ([]libdns.Record, error) {
	panic("stackr never sets a whole name: it would drop someone else's records")
}

func (f *zoneFake) Delete(_ context.Context, _ string, rs []libdns.Record) ([]libdns.Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rs = slices.DeleteFunc(f.rs, func(x libdns.RR) bool {
		return slices.ContainsFunc(rs, func(r libdns.Record) bool { return r.RR().Data == x.Data && r.RR().Name == x.Name })
	})
	return rs, nil
}

func (f *zoneFake) TakeOver(context.Context, string) error { return nil }

func (f *zoneFake) shop() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.rs {
		if r.Name == "shop" {
			out = append(out, r.Type+" "+r.Data)
		}
	}
	return out
}

// Moving a scope's ingress to another node repoints its domains' records.
func TestRecordFollowsIngressMove(t *testing.T) {
	f := &zoneFake{}
	n := newTwoNodes(t, WithDNS(func(string, DNSCreds) (DNSProvider, error) { return f, nil }))
	ctx := context.Background()
	admin := &Principal{User: User{ID: "u-admin"}, Access: authz.User{ID: "u-admin", Active: true, Admin: true}}
	for id, ip := range map[string]string{n.ctl.ID: "198.51.100.1", "w1": "198.51.100.2"} {
		nd, err := n.st.Nodes.Get(ctx, id)
		must(t, err)
		nd.PublicIP = ip
		must(t, n.st.Nodes.Update(ctx, nd))
	}
	c, err := n.orch.AddDNSConnector(ctx, nil, dnscap.Namecheap, DNSAccount{User: "u", Key: "k", ClientIP: "203.0.113.7"})
	must(t, err)
	_, err = n.orch.ManageDNSDomain(ctx, nil, c.Connector.ID, "acme.dev", nil)
	must(t, err)
	_, err = n.orch.AttachDomain(ctx, n.edgeT.ID, DomainSpec{Host: "shop.acme.dev"})
	must(t, err)
	if got := f.shop(); !slices.Equal(got, []string{"A 198.51.100.1"}) {
		t.Fatalf("served by the default ingress: %v", got)
	}

	addPlacement(t, n, "w1", "w1", false)
	must(t, n.orch.SetPlacementOpen(ctx, admin, "w1", false, []string{"acme"}))
	w, err := n.st.Nodes.Get(ctx, "w1")
	must(t, err)
	w.Tags = `["ingress","egress"]`
	must(t, n.st.Nodes.Update(ctx, w))
	must(t, n.orch.SetIngress(ctx, admin, ScopeEnv, n.edge.ID, "w1"))
	if got := f.shop(); !slices.Equal(got, []string{"A 198.51.100.2"}) {
		t.Errorf("after the move: %v", got)
	}
}

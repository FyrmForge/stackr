package service

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/libdns/libdns"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/dnscap"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Kicks that land while a run is going fold into exactly one more run, and
// Kick never waits for a run.
func TestCoalesceKicks(t *testing.T) {
	var runs atomic.Int32
	started, release := make(chan struct{}, 4), make(chan struct{})
	k := &coalesce{fn: func() {
		runs.Add(1)
		started <- struct{}{}
		<-release
	}}
	k.Kick()
	<-started
	for range 10 {
		k.Kick() // returns at once: the run is still blocked
	}
	close(release)
	k.Wait()
	if n := runs.Load(); n != 2 {
		t.Fatalf("runs = %d, want 2 (the first and one for the ten kicks)", n)
	}
	k.Kick()
	k.Wait()
	if n := runs.Load(); n != 3 {
		t.Errorf("a kick after the runs ended = %d runs, want 3", n)
	}
}

// slowZone is a provider whose writes hang until gate closes, then fail.
type slowZone struct {
	*zoneFake
	gate chan struct{}
}

func (s slowZone) Append(context.Context, string, []libdns.Record) ([]libdns.Record, error) {
	<-s.gate
	return nil, errors.New("namecheap: timed out")
}

// A provider that hangs never holds the proxy push; its failure is kept
// for the DNS connectors section, and a later clean sync clears it.
func TestSlowDNSNeverHoldsTheProxyPush(t *testing.T) {
	ctx := context.Background()
	slow := slowZone{&zoneFake{}, make(chan struct{})}
	var p DNSProvider = slow
	n := newTwoNodes(t, WithDNS(func(string, DNSCreds) (DNSProvider, error) { return p, nil }))
	nd, err := n.st.Nodes.Get(ctx, n.ctl.ID)
	must(t, err)
	nd.PublicIP = "198.51.100.1"
	must(t, n.st.Nodes.Update(ctx, nd))
	c, err := n.orch.AddDNSConnector(ctx, nil, dnscap.Namecheap, DNSAccount{User: "u", Key: "k", ClientIP: "203.0.113.7"})
	must(t, err)
	_, err = n.orch.ManageDNSDomain(ctx, nil, c.Connector.ID, "acme.dev", nil)
	must(t, err)
	must(t, n.st.Domains.Create(ctx, store.Domain{ID: "d1", TileID: n.edgeT.ID, Host: "shop.acme.dev", Path: "/", CreatedAt: time.Now()}))

	done := make(chan error, 1)
	go func() { done <- n.orch.SyncProxy(ctx) }()
	select {
	case err := <-done:
		must(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the proxy push waited for the DNS provider")
	}
	close(slow.gate)
	n.orch.WaitDNS()
	if got := n.orch.DNSSyncError(); !strings.Contains(got, "timed out") {
		t.Fatalf("sync error = %q, want the provider's failure", got)
	}

	p = slow.zoneFake // the provider answers again
	must(t, n.orch.SyncProxy(ctx))
	n.orch.WaitDNS()
	if got := n.orch.DNSSyncError(); got != "" {
		t.Errorf("after a clean sync: %q", got)
	}
}

package service

import (
	"context"
	"slices"
	"testing"
	"time"
)

func noBannerCache(t *testing.T) {
	dnsBannerTTL = 0
	t.Cleanup(func() { dnsBannerTTL = 2 * time.Minute })
}

// A hand-managed host that resolves elsewhere is in the banner with the record
// to set, and drops out once it resolves to the ingress node.
func TestEnvDNSBannerShowsStaleAndClears(t *testing.T) {
	noBannerCache(t)
	ctx := context.Background()
	n := newTwoNodes(t)
	publicIPs(t, n, "198.51.100.1", "198.51.100.2")
	_, err := n.orch.AttachDomain(ctx, n.devT.ID, DomainSpec{Host: "shop.example.com"})
	must(t, err)

	fakeDNS(t, map[string][]string{"shop.example.com": {"192.0.2.9"}})
	m, err := n.orch.EnvDNSBanner(ctx, n.dev.ID)
	must(t, err)
	if len(m.Hand) != 1 || m.Hand[0].Host != "shop.example.com" || recordsOf(m.Hand[0])[0] != "A shop.example.com 198.51.100.1" {
		t.Fatalf("banner = %+v", m)
	}

	fakeDNS(t, map[string][]string{"shop.example.com": {"198.51.100.1"}})
	if m, err = n.orch.EnvDNSBanner(ctx, n.dev.ID); err != nil || !m.Empty() {
		t.Fatalf("banner after match = %+v, %v", m, err)
	}
}

// A held managed host stays in the banner until it resolves to its node, then
// stackr releases it.
func TestEnvDNSBannerReleasesHeldHost(t *testing.T) {
	noBannerCache(t)
	ctx := context.Background()
	f := &zoneFake{}
	n := newTwoNodes(t, WithDNS(func(string, DNSCreds) (DNSProvider, error) { return f, nil }))
	publicIPs(t, n, "198.51.100.1", "198.51.100.2")
	c, err := n.orch.AddConnector(ctx, ConnectorAdd{Kind: "namecheap", Values: map[string]string{"user": "u", "key": "k", "client_ip": "203.0.113.7"}})
	must(t, err)
	_, err = n.orch.ManageDNSDomain(ctx, nil, c.Connector.ID, "acme.dev", nil)
	must(t, err)
	_, err = n.orch.AttachDomain(ctx, n.devT.ID, DomainSpec{Host: "app.acme.dev"})
	must(t, err)
	must(t, n.orch.holdDNS(ctx, []string{"app.acme.dev"}))

	fakeDNS(t, map[string][]string{"app.acme.dev": {"192.0.2.9"}})
	m, err := n.orch.EnvDNSBanner(ctx, n.dev.ID)
	must(t, err)
	if len(m.Managed) != 1 || !slices.Equal(n.orch.DNSHeld(ctx), []string{"app.acme.dev"}) {
		t.Fatalf("banner = %+v held = %v", m, n.orch.DNSHeld(ctx))
	}

	fakeDNS(t, map[string][]string{"app.acme.dev": {"198.51.100.1"}})
	if m, err = n.orch.EnvDNSBanner(ctx, n.dev.ID); err != nil || !m.Empty() {
		t.Fatalf("banner after match = %+v, %v", m, err)
	}
	if got := n.orch.DNSHeld(ctx); len(got) != 0 {
		t.Errorf("held after match = %v", got)
	}
}

// The records sync releases a held host once it resolves to its node, with
// nobody looking at a banner; until then the hold stays.
func TestDNSSyncReleasesHeldHost(t *testing.T) {
	ctx := context.Background()
	f := &zoneFake{}
	n := newTwoNodes(t, WithDNS(func(string, DNSCreds) (DNSProvider, error) { return f, nil }))
	publicIPs(t, n, "198.51.100.1", "198.51.100.2")
	c, err := n.orch.AddConnector(ctx, ConnectorAdd{Kind: "namecheap", Values: map[string]string{"user": "u", "key": "k", "client_ip": "203.0.113.7"}})
	must(t, err)
	_, err = n.orch.ManageDNSDomain(ctx, nil, c.Connector.ID, "acme.dev", nil)
	must(t, err)
	_, err = n.orch.AttachDomain(ctx, n.devT.ID, DomainSpec{Host: "app.acme.dev"})
	must(t, err)
	must(t, n.orch.holdDNS(ctx, []string{"app.acme.dev"}))

	fakeDNS(t, map[string][]string{"app.acme.dev": {"192.0.2.9"}})
	must(t, n.orch.reconcileDNS(ctx, nil))
	if !slices.Equal(n.orch.DNSHeld(ctx), []string{"app.acme.dev"}) {
		t.Fatalf("held after a sync that does not match = %v", n.orch.DNSHeld(ctx))
	}

	fakeDNS(t, map[string][]string{"app.acme.dev": {"198.51.100.1"}})
	must(t, n.orch.reconcileDNS(ctx, nil))
	if got := n.orch.DNSHeld(ctx); len(got) != 0 {
		t.Errorf("held after a matching sync = %v", got)
	}
}

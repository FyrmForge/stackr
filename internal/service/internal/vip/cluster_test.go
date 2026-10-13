package vip

import (
	"net/netip"
	"strings"
	"testing"
)

// The supernet and the transit net sit above every grant: a local tile with a
// lan:10.0.0.0/8 grant cannot reach another node's range or a WireGuard link,
// in FORWARD or at the host. A source outside this node's range returns at
// the top of FWD, before any of it (the mesh polices those, not this chain).
func TestFilterKeepsClusterOutOfGrants(t *testing.T) {
	got := Render(nil, map[string]Entry{
		"10.213.1.2": {Replicas: []string{"10.213.1.3"}, Lan: []string{"lan:10.0.0.0/8"}},
	}, testBase)
	at := func(sub string) int {
		t.Helper()
		i := strings.Index(got, sub)
		if i < 0 {
			t.Fatalf("missing %q:\n%s", sub, got)
		}
		return i
	}
	grant := "-s 10.213.1.3/32 -d 10.0.0.0/8 -j RETURN"
	for chain, dropPrefix := range map[string]string{
		"STACKR-FWD-OWN": "-A STACKR-FWD-OWN -d ",
		"STACKR-IN-OWN":  "-A STACKR-IN-OWN -d ",
	} {
		lan := at("-A " + chain + " " + grant)
		for _, net := range []string{"10.208.0.0/12", "10.207.255.0/24"} {
			if drop := at(dropPrefix + net + " -j DROP"); drop > lan {
				t.Errorf("%s: %s dropped after the lan grant", chain, net)
			}
		}
	}
	remote := at("-A STACKR-FWD -j RETURN")
	local := at("-A STACKR-FWD-OWN -d 10.213.1.0/24 -j RETURN")
	xn := at("-A STACKR-FWD-OWN -j STACKR-XN")
	drop := at("-A STACKR-FWD-OWN -d 10.208.0.0/12 -j DROP")
	if remote >= local || local >= xn || xn >= drop {
		t.Errorf("FWD order: remote-source return %d, local return %d, XN %d, supernet drop %d", remote, local, xn, drop)
	}

	// No cluster in the base, none of it rendered.
	b := testBase
	b.Cluster = netip.Prefix{}
	plain := Render(nil, nil, b)
	for _, s := range []string{"10.208.0.0/12", "10.207.255.0/24", "-j STACKR-XN"} {
		if strings.Contains(plain, s) {
			t.Errorf("base without a cluster rendered %q", s)
		}
	}
}

// The VIP chains render the same on one node whatever the cluster fields say:
// only the (empty) STACKR-NAT chain and the raw section are added around them.
func TestRenderVIPsUnchangedByCluster(t *testing.T) {
	next := map[string]Entry{
		"10.213.1.2": {Replicas: []string{"10.213.1.3", "10.213.1.4"}},
		"10.213.2.2": {Replicas: []string{"10.213.2.3"}},
	}
	prev := map[string]Entry{"10.213.9.2": {Replicas: []string{"10.213.9.3"}}}
	nat := func(s string) string {
		head, _, _ := strings.Cut(s, "COMMIT\n")
		return head
	}
	old := nat(Render(prev, next, Base{}))
	got := nat(Render(prev, next, testBase))
	if want := strings.Replace(old, ":STACKR-VIP - [0:0]\n", ":STACKR-VIP - [0:0]\n:STACKR-NAT - [0:0]\n", 1); got != want {
		t.Fatalf("nat section:\n%s\nwant:\n%s", got, want)
	}
	full := Render(prev, next, testBase)
	if !strings.Contains(full, "COMMIT\n*raw\n:STACKR-RAW - [0:0]\nCOMMIT\n*filter\n") {
		t.Fatalf("raw section missing or misplaced:\n%s", full)
	}
	if strings.Contains(Render(prev, next, Base{}), "STACKR-RAW") {
		t.Fatal("raw chain rendered without a base")
	}
}

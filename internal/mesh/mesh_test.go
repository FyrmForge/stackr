package mesh

import (
	"maps"
	"net/netip"
	"slices"
	"testing"
)

var (
	pf = netip.MustParsePrefix
	ad = netip.MustParseAddr
	ap = netip.MustParseAddrPort
)

func TestAllocTransit(t *testing.T) {
	a, err := AllocTransit(nil)
	if err != nil || a != ad("10.207.255.1") || Slot(a) != 1 {
		t.Fatal(a, err)
	}
	// taken ones and the controller's are skipped
	taken := []netip.Addr{ad("10.207.255.1"), ad("10.207.255.2"), ad("10.207.255.3"), ad("10.207.255.4"), ad("10.207.255.5")}
	if a, _ = AllocTransit(taken); a != ad("10.207.255.7") {
		t.Fatal(a)
	}
	var all []netip.Addr
	for i := 1; i < 255; i++ {
		all = append(all, netip.AddrFrom4([4]byte{10, 207, 255, byte(i)}))
	}
	if _, err := AllocTransit(all); err != ErrNoTransit {
		t.Fatal(err)
	}
}

func TestFilterAddrs(t *testing.T) {
	in := []netip.Addr{ad("127.0.0.1"), ad("169.254.169.254"), ad("169.254.1.1"), ad("224.0.0.1"),
		ad("0.0.0.0"), ad("::1"), ad("fe80::1"), ad("10.209.0.5"), ad("10.207.255.2"),
		ad("203.0.113.5"), ad("::ffff:203.0.113.5"), ad("192.168.1.5")}
	got := FilterAddrs(in)
	if len(got) != 2 || got[0] != ad("203.0.113.5") || got[1] != ad("192.168.1.5") {
		t.Fatal(got)
	}
}

func TestCandidatesOrder(t *testing.T) {
	self := Node{ID: "a", Ifaces: []netip.Prefix{pf("192.168.1.110/24"), pf("127.0.0.1/8")}}
	other := Node{ID: "b", Port: 51820,
		Ifaces: []netip.Prefix{pf("192.168.1.111/24"), pf("172.16.0.5/16"), pf("203.0.113.9/24")},
		Public: []netip.Addr{ad("203.0.113.9"), ad("169.254.169.254")}}
	got := Candidates(self, other)
	want := []netip.AddrPort{ap("192.168.1.111:51820"), ap("203.0.113.9:51820")}
	if len(got) != 2 {
		t.Fatal(got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatal(got)
		}
	}
	// no shared subnet: private dropped
	self.Ifaces = []netip.Prefix{pf("10.0.0.2/24")}
	got = Candidates(self, other)
	if len(got) != 1 || got[0] != ap("203.0.113.9:51820") {
		t.Fatal(got)
	}
	// other's IPv6 only when self has IPv6 (109 behind a v4-only NAT)
	other.Public = []netip.Addr{ad("2001:db8::9"), ad("203.0.113.9")}
	got = Candidates(self, other)
	if len(got) != 1 || got[0] != ap("203.0.113.9:51820") {
		t.Fatal(got)
	}
	self.Public = []netip.Addr{ad("2001:db8::2")}
	got = Candidates(self, other)
	if len(got) != 2 || got[0] != netip.AddrPortFrom(ad("2001:db8::9"), 51820) {
		t.Fatal(got)
	}
}

func TestPeers(t *testing.T) {
	a := Node{ID: "a", PublicKey: "ka", Transit: ad("10.207.255.1"), Subnets: []netip.Prefix{pf("10.208.0.0/24")}}
	b := Node{ID: "b", PublicKey: "kb", Transit: ad("10.207.255.2"), Subnets: []netip.Prefix{pf("10.208.7.0/24"), pf("10.208.3.0/24")},
		Port: 1, Public: []netip.Addr{ad("203.0.113.9")}}
	c := Node{ID: "c"} // not joined yet
	ps, err := Peers(a, []Node{a, b, c}, nil)
	if err != nil || len(ps) != 1 {
		t.Fatal(ps, err)
	}
	p := ps[0]
	want := []netip.Prefix{pf("10.208.7.0/24"), pf("10.208.3.0/24"), pf("10.207.255.2/32")}
	if !slices.Equal(p.AllowedIPs, want) || len(p.Candidates) != 1 {
		t.Fatal(p)
	}
}

// A pair routed through v: on a, b's prefixes ride v's peer entry and b's
// own entry keeps its endpoints but carries nothing.
func TestPeersVia(t *testing.T) {
	a := Node{ID: "a", PublicKey: "ka", Transit: ad("10.207.255.1"), Subnets: []netip.Prefix{pf("10.208.0.0/24")}}
	b := Node{ID: "b", PublicKey: "kb", Transit: ad("10.207.255.2"), Subnets: []netip.Prefix{pf("10.208.1.0/24")},
		Port: 1, Public: []netip.Addr{ad("203.0.113.9")}}
	v := Node{ID: "v", PublicKey: "kv", Transit: ad("10.207.255.3"), Subnets: []netip.Prefix{pf("10.208.2.0/24")}}
	via := func(x, y string) string {
		if PairKey(x, y) == PairKey("a", "b") {
			return "v"
		}
		return ""
	}
	ps, err := Peers(a, []Node{a, b, v}, via)
	if err != nil || len(ps) != 2 {
		t.Fatal(ps, err)
	}
	if ps[0].NodeID != "b" || len(ps[0].AllowedIPs) != 0 || len(ps[0].Candidates) != 1 {
		t.Fatal(ps[0])
	}
	want := []netip.Prefix{pf("10.208.2.0/24"), pf("10.207.255.3/32"), pf("10.208.1.0/24"), pf("10.207.255.2/32")}
	if !slices.Equal(ps[1].AllowedIPs, want) {
		t.Fatal(ps[1].AllowedIPs)
	}
}

type graph map[[2]string]bool

func (g graph) up(a, b string) bool { return g[PairKey(a, b)] }

func links(pairs ...string) graph {
	g := graph{}
	for i := 0; i < len(pairs); i += 2 {
		g[PairKey(pairs[i], pairs[i+1])] = true
	}
	return g
}

// h2 lost its public address: only h1 reaches it, everyone else goes there
// through h1 (2 hops), and h1 is public so it wins over a lower id.
func TestRoutesTwoHops(t *testing.T) {
	g := links("ctl", "h1", "ctl", "m", "h1", "m", "h1", "h2", "a", "ctl", "a", "h2")
	pub := func(id string) bool { return id == "h1" }
	got := Routes([]string{"a", "ctl", "h1", "h2", "m"}, g.up, pub)
	want := map[[2]string]string{
		{"ctl", "h2"}: "h1", {"h2", "ctl"}: "h1",
		{"m", "h2"}: "h1", {"h2", "m"}: "h1",
		{"a", "h1"}: "ctl", {"h1", "a"}: "ctl",
		{"a", "m"}: "ctl", {"m", "a"}: "ctl",
	}
	if !maps.Equal(got, want) {
		t.Fatal(got)
	}
}

// A chain a-b-c-d: a reaches d in 3 hops, every next hop one step closer.
func TestRoutesThreeHops(t *testing.T) {
	g := links("a", "b", "b", "c", "c", "d")
	got := Routes([]string{"a", "b", "c", "d"}, g.up, func(string) bool { return false })
	want := map[[2]string]string{
		{"a", "c"}: "b", {"a", "d"}: "b", {"b", "d"}: "c",
		{"c", "a"}: "b", {"d", "a"}: "c", {"d", "b"}: "c",
	}
	if !maps.Equal(got, want) {
		t.Fatal(got)
	}
}

// The direct tunnel comes back: the route goes; a node nobody reaches gets
// none.
func TestRoutesRecoveryAndUnreachable(t *testing.T) {
	g := links("a", "b", "b", "c")
	if got := Routes([]string{"a", "b", "c"}, g.up, func(string) bool { return false }); got[[2]string{"a", "c"}] != "b" {
		t.Fatal(got)
	}
	g[PairKey("a", "c")] = true
	if got := Routes([]string{"a", "b", "c"}, g.up, func(string) bool { return false }); len(got) != 0 {
		t.Fatal(got)
	}
	got := Routes([]string{"a", "b", "c", "x"}, g.up, func(string) bool { return false })
	for k := range got {
		if k[0] == "x" || k[1] == "x" {
			t.Fatal(got)
		}
	}
}

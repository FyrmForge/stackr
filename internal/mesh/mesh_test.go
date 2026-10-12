package mesh

import (
	"net/netip"
	"testing"
)

var (
	pf = netip.MustParsePrefix
	ad = netip.MustParseAddr
	ap = netip.MustParseAddrPort
)

func TestAllocateRange(t *testing.T) {
	r, err := AllocateRange(nil, nil)
	if err != nil || r != pf("10.208.0.0/16") {
		t.Fatal(r, err)
	}
	r, _ = AllocateRange([]netip.Prefix{pf("10.208.0.0/16")}, []netip.Prefix{pf("10.209.4.7/24")})
	if r != pf("10.210.0.0/16") {
		t.Fatal(r)
	}
	// a forgotten range stays taken
	all := make([]netip.Prefix, 0, 16)
	for i := 208; i < 224; i++ {
		all = append(all, netip.PrefixFrom(netip.AddrFrom4([4]byte{10, byte(i), 0, 0}), 16))
	}
	if _, err := AllocateRange(all, nil); err != ErrNoRange {
		t.Fatal(err)
	}
	if _, err := AllocateRange(nil, []netip.Prefix{pf("10.0.0.0/8")}); err != ErrNoRange {
		t.Fatal(err)
	}
}

func TestTransitAddr(t *testing.T) {
	a, err := TransitAddr(pf("10.211.0.0/16"))
	if err != nil || a != ad("10.207.255.4") || !Transit.Contains(a) || Supernet.Contains(a) {
		t.Fatal(a, err)
	}
	if _, err := TransitAddr(pf("192.168.0.0/16")); err == nil {
		t.Fatal("want error")
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
	got := Candidates(self, other, ap("198.51.100.1:40000"))
	want := []netip.AddrPort{ap("192.168.1.111:51820"), ap("203.0.113.9:51820"), ap("198.51.100.1:40000")}
	if len(got) != 3 {
		t.Fatal(got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatal(got)
		}
	}
	// no shared subnet: private dropped
	self.Ifaces = []netip.Prefix{pf("10.0.0.2/24")}
	got = Candidates(self, other, netip.AddrPort{})
	if len(got) != 1 || got[0] != ap("203.0.113.9:51820") {
		t.Fatal(got)
	}
	// other's IPv6 only when self has IPv6 (109 behind a v4-only NAT)
	other.Public = []netip.Addr{ad("2001:db8::9"), ad("203.0.113.9")}
	got = Candidates(self, other, netip.AddrPort{})
	if len(got) != 1 || got[0] != ap("203.0.113.9:51820") {
		t.Fatal(got)
	}
	self.Public = []netip.Addr{ad("2001:db8::2")}
	got = Candidates(self, other, netip.AddrPort{})
	if len(got) != 2 || got[0] != netip.AddrPortFrom(ad("2001:db8::9"), 51820) {
		t.Fatal(got)
	}
}

func TestPeers(t *testing.T) {
	a := Node{ID: "a", PublicKey: "ka", Range: pf("10.208.0.0/16")}
	b := Node{ID: "b", PublicKey: "kb", Range: pf("10.209.0.0/16"), Port: 1, Public: []netip.Addr{ad("203.0.113.9")}}
	c := Node{ID: "c"} // not joined yet
	ps, err := Peers(a, []Node{a, b, c}, func(x, y string) netip.AddrPort { return ap("198.51.100.1:9") })
	if err != nil || len(ps) != 1 {
		t.Fatal(ps, err)
	}
	p := ps[0]
	if p.AllowedIPs[0] != pf("10.209.0.0/16") || p.AllowedIPs[1] != pf("10.207.255.2/32") || len(p.Candidates) != 2 {
		t.Fatal(p)
	}
}

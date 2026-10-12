package relay

import (
	"net"
	"net/netip"
	"testing"
	"time"
)

func hs(typ byte, n int) []byte {
	b := make([]byte, n)
	b[0] = typ
	return b
}

func recv(c *net.UDPConn) []byte {
	c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	b := make([]byte, 2000)
	n, _, err := c.ReadFromUDP(b)
	if err != nil {
		return nil
	}
	return b[:n]
}

// Loopback gives both sides the same IP, so the tests use a pair of
// 127.0.0.1 and 127.0.0.2 and a dialer bound to each.
func bound(t *testing.T, ip string) *net.UDPConn {
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP(ip)})
	if err != nil {
		t.Skip("cannot bind", ip)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestRelayLearnsOnlyFromHandshake(t *testing.T) {
	a, b, evil := bound(t, "127.0.0.1"), bound(t, "127.0.0.2"), bound(t, "127.0.0.3")
	r, err := Start(Pair{A: netip.MustParseAddr("127.0.0.1"), B: netip.MustParseAddr("127.0.0.2")})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	dst := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: r.Addr().Port}

	// data before any handshake goes nowhere
	a.WriteToUDP([]byte("early"), dst)
	if recv(b) != nil {
		t.Fatal("forwarded before learning")
	}
	// A initiates: B not learned yet, dropped
	a.WriteToUDP(hs(1, 148), dst)
	if recv(b) != nil {
		t.Fatal("forwarded to unlearned side")
	}
	// B responds: learned, forwarded to A
	b.WriteToUDP(hs(2, 92), dst)
	if got := recv(a); len(got) != 92 || got[0] != 2 {
		t.Fatal("response not forwarded", got)
	}
	// data both ways between learned endpoints
	a.WriteToUDP([]byte{4, 0, 0, 0, 9}, dst)
	if got := recv(b); len(got) != 5 {
		t.Fatal("data a->b", got)
	}
	b.WriteToUDP([]byte{4, 0, 0, 0, 8, 8}, dst)
	if got := recv(a); len(got) != 6 {
		t.Fatal("data b->a", got)
	}
	// junk from an unknown IP is dropped
	evil.WriteToUDP(hs(1, 148), dst)
	if recv(a) != nil || recv(b) != nil {
		t.Fatal("foreign ip forwarded")
	}
	// same IP, other port, non-handshake: cannot steal the mapping
	a2 := bound(t, "127.0.0.1")
	a2.WriteToUDP([]byte{4, 0, 0, 0, 1}, dst)
	if recv(b) != nil {
		t.Fatal("unlearned port forwarded")
	}
	b.WriteToUDP([]byte{4, 0, 0, 0, 7}, dst)
	if got := recv(a); len(got) != 5 {
		t.Fatal("mapping hijacked by junk", got)
	}
	// a handshake from the new port does move the mapping (NAT rebind)
	a2.WriteToUDP(hs(1, 148), dst)
	if got := recv(b); len(got) != 148 {
		t.Fatal("rebind handshake", got)
	}
	b.WriteToUDP([]byte{4, 0, 0, 0, 6}, dst)
	if got := recv(a2); len(got) != 5 {
		t.Fatal("not relearned", got)
	}
	// wrong-length "handshake" does not count
	a.WriteToUDP(hs(1, 100), dst)
	b.WriteToUDP([]byte{4, 0, 0, 0, 5}, dst)
	if recv(a) != nil {
		t.Fatal("bad handshake relearned")
	}
}

func TestManagerSync(t *testing.T) {
	var m Manager
	defer m.Close()
	p := Pair{A: netip.MustParseAddr("127.0.0.1"), B: netip.MustParseAddr("127.0.0.2")}
	if err := m.Sync([]Pair{p}); err != nil {
		t.Fatal(err)
	}
	if err := m.Sync([]Pair{p}); err != nil || len(m.relays) != 1 {
		t.Fatal(err)
	}
	m.Sync(nil)
	if len(m.relays) != 0 {
		t.Fatal("not stopped")
	}
	if err := m.Sync([]Pair{{A: p.A, B: p.A}}); err == nil {
		t.Fatal("want error for same address")
	}
}

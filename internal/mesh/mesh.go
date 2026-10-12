// Package mesh is the controller-side pure logic of the node WireGuard mesh:
// address plan, range allocation, untrusted report filtering and peer lists.
package mesh

import (
	"errors"
	"fmt"
	"net/netip"
)

// Address plan. Node ranges all sit inside Supernet so one firewall rule
// covers "cluster"; Transit is outside it and holds the wg interface addresses.
var (
	Supernet = netip.MustParsePrefix("10.208.0.0/12")
	Transit  = netip.MustParsePrefix("10.207.255.0/24")
	// ExitNet holds the exit links: a /30 per (client, exit) pair of the 16
	// node slots.
	ExitNet = netip.MustParsePrefix("10.251.0.0/22")
)

// Iface is the mesh's WireGuard interface on every node. Not wg0, and the
// port not 51820: those are WireGuard's defaults, likely already taken by a
// VPN the box runs.
const Iface = "stackr0"

// DefaultPort is the mesh's UDP listen port; a node may pick another at
// install or join (--wg-port) and reports it in its facts.
const DefaultPort = 51871

// LegacyIface is the interface name before stackr0; a node removes it on
// upgrade when it is the one stackr made (its private key is the node's).
const LegacyIface = "wg0"

// NodePrefixLen is the size of one node's container range.
const NodePrefixLen = 16

// ErrNoRange means the supernet has no free /16 left.
var ErrNoRange = errors.New("mesh: no free node range in the cluster supernet")

func overlaps(a, b netip.Prefix) bool { return a.Overlaps(b) }

// AllocateRange picks the lowest /16 of the supernet that overlaps none of
// taken (every range ever handed out, forgotten nodes included, so a range is
// never reused) and none of ifaces (every node's reported interface prefixes).
func AllocateRange(taken, ifaces []netip.Prefix) (netip.Prefix, error) {
	base := Supernet.Addr().As4()
	n := 1 << (NodePrefixLen - Supernet.Bits())
	for i := range n {
		a := base
		a[1] += byte(i)
		cand := netip.PrefixFrom(netip.AddrFrom4(a), NodePrefixLen)
		if !free(cand, taken) || !free(cand, ifaces) {
			continue
		}
		return cand, nil
	}
	return netip.Prefix{}, ErrNoRange
}

func free(c netip.Prefix, used []netip.Prefix) bool {
	for _, u := range used {
		if overlaps(c, u.Masked()) {
			return false
		}
	}
	return true
}

// TransitAddr is the node's address on the transit net, derived from its
// range so it needs no allocator of its own: slot 0 is .1, slot 1 is .2, ...
func TransitAddr(rng netip.Prefix) (netip.Addr, error) {
	if rng.Bits() != NodePrefixLen || !Supernet.Contains(rng.Addr()) {
		return netip.Addr{}, fmt.Errorf("mesh: %s is not a node range", rng)
	}
	slot := rng.Addr().As4()[1] - Supernet.Addr().As4()[1]
	a := Transit.Addr().As4()
	a[3] = slot + 1
	return netip.AddrFrom4(a), nil
}

var metadata = netip.MustParseAddr("169.254.169.254")

// UsableEndpoint reports whether a reported address may become a WireGuard
// endpoint. Reports come from nodes and are untrusted.
func UsableEndpoint(a netip.Addr) bool {
	a = a.Unmap()
	switch {
	case !a.IsValid(), a.IsUnspecified(), a.IsLoopback(), a.IsMulticast(),
		a.IsLinkLocalUnicast(), a.IsLinkLocalMulticast(), a == metadata:
		return false
	}
	return !Supernet.Contains(a) && !Transit.Contains(a)
}

// FilterAddrs keeps the usable, de-duplicated addresses in order.
func FilterAddrs(in []netip.Addr) []netip.Addr {
	var out []netip.Addr
	seen := map[netip.Addr]bool{}
	for _, a := range in {
		a = a.Unmap()
		if UsableEndpoint(a) && !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	return out
}

// Node is what the controller knows about one node.
type Node struct {
	ID        string
	PublicKey string
	Range     netip.Prefix
	Port      uint16 // wg listen port
	// Ifaces are the node's reported interface prefixes (address with mask).
	Ifaces []netip.Prefix
	// Public are addresses reachable from outside, reported by the node.
	Public []netip.Addr
}

// Peer is one remote node as the local wg interface should see it.
type Peer struct {
	NodeID     string
	PublicKey  string
	AllowedIPs []netip.Prefix // remote range and transit /32
	// Candidates are endpoints in the order to try. The first is current.
	Candidates []netip.AddrPort
}

// lanAddrs are other's private addresses on a subnet self also has.
func lanAddrs(self, other Node) []netip.Addr {
	var out []netip.Addr
	for _, p := range other.Ifaces {
		a := p.Addr().Unmap()
		if !UsableEndpoint(a) || !a.IsPrivate() {
			continue
		}
		for _, mine := range self.Ifaces {
			if mine.Masked().Contains(a) && mine.Addr().Unmap() != a {
				out = append(out, a)
				break
			}
		}
	}
	return out
}

// SameLAN reports whether a and b share a private subnet.
func SameLAN(a, b Node) bool { return len(lanAddrs(a, b)) > 0 }

// HasV6 reports whether n has a public IPv6 address of its own: without one
// it cannot reach anybody's IPv6.
func HasV6(n Node) bool {
	for _, a := range FilterAddrs(n.Public) {
		if a.Is6() {
			return true
		}
	}
	return false
}

// Candidates orders how self can reach other: other's private addresses only
// when self has an interface on the same subnet, then other's public
// addresses (IPv6 ones only when self has IPv6), then the relay (zero
// AddrPort means none).
func Candidates(self, other Node, relay netip.AddrPort) []netip.AddrPort {
	var out []netip.AddrPort
	seen := map[netip.AddrPort]bool{}
	add := func(a netip.Addr) {
		ap := netip.AddrPortFrom(a, other.Port)
		if !seen[ap] {
			seen[ap] = true
			out = append(out, ap)
		}
	}
	for _, a := range lanAddrs(self, other) {
		add(a)
	}
	v6 := HasV6(self)
	for _, a := range FilterAddrs(other.Public) {
		if a.Is4() || v6 {
			add(a)
		}
	}
	if relay.IsValid() && !seen[relay] {
		out = append(out, relay)
	}
	return out
}

// RelayFor returns the relay endpoint for a pair, if the controller has one.
type RelayFor func(a, b string) netip.AddrPort

// Peers builds self's peer list: every other node with a range and key.
func Peers(self Node, all []Node, relay RelayFor) ([]Peer, error) {
	var out []Peer
	for _, o := range all {
		if o.ID == self.ID || o.PublicKey == "" || !o.Range.IsValid() {
			continue
		}
		t, err := TransitAddr(o.Range)
		if err != nil {
			return nil, err
		}
		var r netip.AddrPort
		if relay != nil {
			r = relay(self.ID, o.ID)
		}
		out = append(out, Peer{
			NodeID:     o.ID,
			PublicKey:  o.PublicKey,
			AllowedIPs: []netip.Prefix{o.Range, netip.PrefixFrom(t, 32)},
			Candidates: Candidates(self, o, r),
		})
	}
	return out, nil
}

// Package mesh is the controller-side pure logic of the node WireGuard mesh:
// address plan, transit allocation, untrusted report filtering and peer lists.
package mesh

import (
	"errors"
	"net/netip"
	"slices"
	"strings"
)

// Address plan. Every stackr network of every node is a /24 of Supernet,
// handed out from one cluster pool, so one firewall rule covers "cluster" and
// an env keeps its network when it moves; Transit is outside it and holds the
// wg interface addresses, one per node.
var (
	Supernet = netip.MustParsePrefix("10.208.0.0/12")
	Transit  = netip.MustParsePrefix("10.207.255.0/24")
	// ExitNet holds the exit links: a /30 per (client, exit) pair in use.
	ExitNet = netip.MustParsePrefix("10.251.0.0/22")
	// ControllerTransit is the controller's transit address. It is the one
	// the /16-per-node plan gave the controller, kept so an upgraded cluster
	// needs no re-handshake with it.
	ControllerTransit = netip.MustParseAddr("10.207.255.6")
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

// ErrNoTransit means the transit net has no free address left: the cluster
// holds as many nodes as it can.
var ErrNoTransit = errors.New("mesh: no free transit address left")

// AllocTransit is the lowest transit address not in taken (every node's,
// the controller's included). .0 and .255 are never handed out.
func AllocTransit(taken []netip.Addr) (netip.Addr, error) {
	a := Transit.Addr().As4()
	for i := 1; i < 255; i++ {
		a[3] = byte(i)
		c := netip.AddrFrom4(a)
		if c != ControllerTransit && !slices.Contains(taken, c) {
			return c, nil
		}
	}
	return netip.Addr{}, ErrNoTransit
}

// Slot is a node's small number, from its transit address (1..254): what
// exit links number their tables and marks by.
func Slot(transit netip.Addr) int {
	if !Transit.Contains(transit) {
		return 0
	}
	return int(transit.As4()[3])
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
	// Exit links ride the mesh: a mesh endpoint on one would tunnel the mesh
	// through itself.
	return !Supernet.Contains(a) && !Transit.Contains(a) && !ExitNet.Contains(a)
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
	Transit   netip.Addr     // the node's address on the transit net
	Subnets   []netip.Prefix // the networks of the envs on the node, from the cluster pool
	Port      uint16         // wg listen port
	// Ifaces are the node's reported interface prefixes (address with mask).
	Ifaces []netip.Prefix
	// Public are addresses reachable from outside, reported by the node.
	Public []netip.Addr
}

// Peer is one remote node as the local wg interface should see it.
type Peer struct {
	NodeID     string
	PublicKey  string
	AllowedIPs []netip.Prefix // remote env networks and transit /32
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
// addresses (IPv6 ones only when self has IPv6).
func Candidates(self, other Node) []netip.AddrPort {
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
	return out
}

// NextHop names the peer that carries self's traffic for dest when the two
// have no direct tunnel, "" when they connect directly (or nothing reaches
// dest).
type NextHop func(self, dest string) string

// Peers builds self's peer list: every other node with a transit address and
// key. A node reached through others keeps its own peer entry, so its
// endpoints are still probed, but its networks and transit address move to the next
// hop's entry: WireGuard routes by AllowedIPs, which must not overlap on one
// interface.
func Peers(self Node, all []Node, next NextHop) ([]Peer, error) {
	var out []Peer
	idx := map[string]int{}
	var moves []struct {
		hop string
		ps  []netip.Prefix
	}
	for _, o := range all {
		if o.ID == self.ID || o.PublicKey == "" || !Transit.Contains(o.Transit) {
			continue
		}
		own := append(slices.Clone(o.Subnets), netip.PrefixFrom(o.Transit, 32))
		p := Peer{NodeID: o.ID, PublicKey: o.PublicKey, Candidates: Candidates(self, o)}
		hop := ""
		if next != nil {
			hop = next(self.ID, o.ID)
		}
		if hop != "" && hop != o.ID {
			moves = append(moves, struct {
				hop string
				ps  []netip.Prefix
			}{hop, own})
		} else {
			p.AllowedIPs = own
		}
		idx[o.ID] = len(out)
		out = append(out, p)
	}
	for _, m := range moves {
		if i, ok := idx[m.hop]; ok {
			out[i].AllowedIPs = append(out[i].AllowedIPs, m.ps...)
		}
	}
	return out, nil
}

// Routes is every node's next hop towards every node it has no working
// tunnel to, keyed {src, dst}: hop-count shortest paths over the graph of
// working tunnels (up). Each node picks, among its neighbours one hop
// closer to dst, a public one (pub) first, then the lowest id. All nodes
// read one graph, so a next hop is always strictly closer to dst and the
// routes are loop-free. A pair that is up, or that nothing connects, gets
// no entry.
func Routes(ids []string, up func(a, b string) bool, pub func(id string) bool) map[[2]string]string {
	order := slices.Clone(ids)
	slices.SortFunc(order, func(a, b string) int {
		if pa, pb := pub(a), pub(b); pa != pb {
			if pa {
				return -1
			}
			return 1
		}
		return strings.Compare(a, b)
	})
	out := map[[2]string]string{}
	for _, dst := range ids {
		// BFS from dst: dist is each node's hop count to it.
		dist := map[string]int{dst: 0}
		queue := []string{dst}
		for len(queue) > 0 {
			x := queue[0]
			queue = queue[1:]
			for _, y := range order {
				if _, seen := dist[y]; !seen && y != x && up(x, y) {
					dist[y] = dist[x] + 1
					queue = append(queue, y)
				}
			}
		}
		for _, src := range ids {
			d, ok := dist[src]
			if !ok || d < 2 { // unreachable, dst itself, or direct
				continue
			}
			for _, n := range order {
				if dn, ok := dist[n]; ok && dn == d-1 && up(src, n) {
					out[[2]string{src, dst}] = n
					break
				}
			}
		}
	}
	return out
}

// PairKey is a and b in a fixed order, for maps keyed by pair.
func PairKey(a, b string) [2]string {
	if a > b {
		a, b = b, a
	}
	return [2]string{a, b}
}

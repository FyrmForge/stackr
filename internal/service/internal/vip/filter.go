package vip

import (
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/internal/cluster"
)

// Filter chains: FWD sees what a stackr container sends through the host,
// IN what it sends to the host itself. FWD is jumped to from DOCKER-USER
// (Docker keeps that chain across restarts, FORWARD it rewrites), IN from
// INPUT, each at position 1.
const (
	FwdChain = "STACKR-FWD"
	InChain  = "STACKR-IN"
	// XNChain is jumped to from inside FwdChain, after the local range's own
	// returns and before the supernet drops: where cross-node accepts go
	// (step 5). Empty, it falls through. It is not a second jump from
	// DOCKER-USER: FwdChain keeps the only rule-1 claim there.
	XNChain = "STACKR-XN"
	// RawChain hangs off raw PREROUTING and NATChain off nat POSTROUTING,
	// each at rule 1. Empty for now: the mesh (step 3) fills them (the wg0
	// accept for the supernet, the no-SNAT accept into wg0).
	RawChain = "STACKR-RAW"
	NATChain = "STACKR-NAT"
)

// Ports the mesh interface accepts on the host: the exit links, one UDP port
// per (client, exit) pair, and the worker API.
const (
	ExitPortLo    = 52000
	ExitPortHi    = 52255
	WorkerAPIPort = 7443
)

// Entry is one VIP's state: the live replica IPs and the lan grants of its
// tile (a grant line, "lan:192.168.1.0/24", "lan:192.168.1.10:8123"; the
// "lan:" prefix is optional).
type Entry struct {
	Replicas []string
	Lan      []string
}

// Base is what the filter rules need beyond the VIPs. The zero value (no
// Range) renders no filter table at all.
type Base struct {
	Range     netip.Prefix // this node's range, the pool its stackr networks come from
	Cluster   netip.Prefix // the cluster supernet; with the transit net it is kept out of every grant. Zero = no cluster drops
	Resolvers []string     // DNS servers a container may reach on :53
	ProxyIP   string       // Caddy on docker0; "" = unknown, the hairpin rule is left out
	Front     []string     // IPs or CIDRs of the front proxy, reachable on 80/443
	PanelBind string       // the panel's listen address; "" = not locked
	PanelPort int          // the panel's real listen port
	// WGIface is the mesh interface; set, STACKR-IN lets only the exit links
	// (UDP ExitPortLo-ExitPortHi) and the worker API (WorkerAPIPort, from
	// WGController alone when set) in from it and drops the rest.
	WGIface      string
	WGController netip.Addr
	// WGPanelPort, set on the controller, is the panel's tcp port a worker
	// reaches over the mesh: registry pulls go through the registry front on
	// the panel listener.
	WGPanelPort int

	early map[string][]string // set by Table: replica IP -> lan lines, before it is routed
}

// private are the ranges a tile cannot start connections to unless granted.
var private = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16", "100.64.0.0/10"}

// lanSpec is a parsed lan grant.
type lanSpec struct {
	dst  netip.Prefix
	port int // 0 = any
}

// parseLan reads ip | cidr, each with an optional :port.
func parseLan(line string) (lanSpec, error) {
	s := strings.TrimPrefix(strings.TrimSpace(line), "lan:")
	var l lanSpec
	if i := strings.LastIndexByte(s, ':'); i >= 0 {
		p, err := strconv.Atoi(s[i+1:])
		if err != nil || p < 1 || p > 65535 {
			return l, fmt.Errorf("vip: lan %q has a bad port", line)
		}
		l.port, s = p, s[:i]
	}
	if p, err := netip.ParsePrefix(s); err == nil && p.Addr().Is4() {
		l.dst = p.Masked()
		return l, nil
	}
	if a, err := netip.ParseAddr(s); err == nil && a.Is4() {
		l.dst = netip.PrefixFrom(a, 32)
		return l, nil
	}
	return l, fmt.Errorf("vip: lan %q is not an IPv4 address or CIDR", line)
}

// target is an IPv4 address or CIDR, as the script prints it.
func target(s string) (string, error) {
	if p, err := netip.ParsePrefix(s); err == nil && p.Addr().Is4() {
		return p.Masked().String(), nil
	}
	if a, err := netip.ParseAddr(s); err == nil && a.Is4() {
		return a.String() + "/32", nil
	}
	return "", fmt.Errorf("vip: %q is not an IPv4 address or CIDR", s)
}

// check validates everything that reaches the filter script as text.
func (b Base) check() error {
	if !b.Range.IsValid() {
		return nil
	}
	if !b.Range.Addr().Is4() {
		return fmt.Errorf("vip: range %s is not IPv4", b.Range)
	}
	if b.Cluster.IsValid() && !b.Cluster.Addr().Is4() {
		return fmt.Errorf("vip: cluster range %s is not IPv4", b.Cluster)
	}
	for _, r := range b.Resolvers {
		if _, err := target(r); err != nil {
			return err
		}
	}
	for _, f := range b.Front {
		if _, err := target(f); err != nil {
			return err
		}
	}
	if b.WGIface != "" && (len(b.WGIface) > 15 || strings.ContainsAny(b.WGIface, " \t\n/")) {
		return fmt.Errorf("vip: %q is not an interface name", b.WGIface)
	}
	if b.WGController.IsValid() && !b.WGController.Is4() {
		return fmt.Errorf("vip: %s is not an IPv4 address", b.WGController)
	}
	if b.WGPanelPort < 0 || b.WGPanelPort > 65535 {
		return fmt.Errorf("vip: panel port %d out of range", b.WGPanelPort)
	}
	for _, a := range []string{b.ProxyIP, b.PanelBind} {
		if a == "" {
			continue
		}
		if p, err := netip.ParseAddr(a); err != nil || !p.Is4() {
			return fmt.Errorf("vip: %q is not an IPv4 address", a)
		}
	}
	if b.PanelPort < 0 || b.PanelPort > 65535 {
		return fmt.Errorf("vip: panel port %d out of range", b.PanelPort)
	}
	return nil
}

// clusterNets are the prefixes kept out of every grant: the supernet and the
// transit net. None when the base carries no cluster.
func (b Base) clusterNets() []string {
	if !b.Cluster.IsValid() {
		return nil
	}
	return []string{b.Cluster.Masked().String(), cluster.Transit}
}

// lanRules are the match specs of every replica's lan grants, shared by
// STACKR-FWD and STACKR-IN.
func lanRules(next map[string]Entry, early map[string][]string) []string {
	var out []string
	// A replica still at its health gate has no VIP entry yet.
	pending := map[string]Entry{}
	for ip, lan := range early {
		pending[ip] = Entry{Replicas: []string{ip}, Lan: lan}
	}
	next = merged(next, pending)
	for _, v := range slices.Sorted(maps.Keys(next)) {
		e := next[v]
		for _, rep := range e.Replicas {
			for _, line := range e.Lan {
				l, _ := parseLan(line)
				if l.port == 0 {
					out = append(out, fmt.Sprintf("-s %s/32 -d %s -j RETURN", rep, l.dst))
					continue
				}
				for _, proto := range []string{"tcp", "udp"} {
					out = append(out, fmt.Sprintf("-s %s/32 -d %s -p %s --dport %d -j RETURN", rep, l.dst, proto, l.port))
				}
			}
		}
	}
	return out
}

// renderFilter is the *filter section: both chains are declared (so
// flushed) and rewritten whole. Addresses are already checked.
func renderFilter(next map[string]Entry, b Base) string {
	rng := b.Range.Masked().String()
	var w strings.Builder
	w.WriteString("*filter\n:" + FwdChain + " - [0:0]\n:" + InChain + " - [0:0]\n:" + XNChain + " - [0:0]\n")
	f := func(format string, a ...any) { fmt.Fprintf(&w, "-A "+FwdChain+" "+format+"\n", a...) }
	f("-m conntrack ! --ctstate NEW -j RETURN")
	// A source outside this node's range is not a container of this node;
	// what it may reach is the mesh's business (STACKR-XN, wg0), not ours.
	f("! -s %s -j RETURN", rng)
	f("-d %s -j RETURN", rng)
	// The supernet and the transit net sit above every grant: a lan:10.0.0.0/8
	// must not open the other nodes' ranges or the WireGuard links. Cross-node
	// accepts (STACKR-XN) go first, so they are the only way in.
	clusterDrops := b.clusterNets()
	if len(clusterDrops) > 0 {
		f("-j %s", XNChain)
	}
	for _, c := range clusterDrops {
		f("-d %s -j DROP", c)
	}
	for _, r := range b.Resolvers {
		d, _ := target(r)
		f("-d %s -p udp --dport 53 -j RETURN", d)
		f("-d %s -p tcp --dport 53 -j RETURN", d)
	}
	if b.ProxyIP != "" {
		f("-d %s -p tcp -m multiport --dports 80,443 -j ACCEPT", b.ProxyIP+"/32")
	}
	for _, fr := range b.Front {
		d, _ := target(fr)
		f("-d %s -p tcp -m multiport --dports 80,443 -j RETURN", d)
	}
	lans := lanRules(next, b.early)
	for _, r := range lans {
		f("%s", r)
	}
	for _, p := range private {
		f("-d %s -j DROP", p)
	}

	in := func(format string, a ...any) { fmt.Fprintf(&w, "-A "+InChain+" "+format+"\n", a...) }
	in("-m conntrack ! --ctstate NEW -j RETURN")
	in("-i lo -j RETURN")
	if b.WGIface != "" {
		in("-i %s -p udp --dport %d:%d -j RETURN", b.WGIface, ExitPortLo, ExitPortHi)
		if b.WGController.IsValid() {
			in("-i %s -s %s/32 -p tcp --dport %d -j RETURN", b.WGIface, b.WGController, WorkerAPIPort)
		} else {
			in("-i %s -p tcp --dport %d -j RETURN", b.WGIface, WorkerAPIPort)
		}
		if b.WGPanelPort > 0 {
			in("-i %s -p tcp --dport %d -j RETURN", b.WGIface, b.WGPanelPort)
		}
		in("-i %s -j DROP", b.WGIface)
	}
	for _, r := range b.Resolvers {
		d, _ := target(r)
		in("-s %s -d %s -p udp --dport 53 -j RETURN", rng, d)
		in("-s %s -d %s -p tcp --dport 53 -j RETURN", rng, d)
	}
	// Same as in FWD: the host's own addresses on the WireGuard links, and
	// every other node's range, are not reachable by a lan grant.
	for _, c := range clusterDrops {
		in("-s %s -d %s -j DROP", rng, c)
	}
	for _, r := range lans { // a lan grant to the server's own address opens INPUT too
		in("%s", r)
	}
	// After the lan returns: an admin-approved range that covers the panel
	// opens it. Always on, whatever Caddy's address is.
	if b.PanelBind != "" && b.PanelPort > 0 {
		in("-s %s -d %s/32 -p tcp --dport %d -j DROP", rng, b.PanelBind, b.PanelPort)
	}
	in("-s %s -j DROP", rng)
	w.WriteString("COMMIT\n")
	return w.String()
}

// merged is a and b under one map; keys of b that clash with a's get a
// suffix, only the rules rendered from them matter.
func merged(a, b map[string]Entry) map[string]Entry {
	out := maps.Clone(a)
	if out == nil {
		out = map[string]Entry{}
	}
	for k, v := range b {
		out["early-"+k] = v
	}
	return out
}

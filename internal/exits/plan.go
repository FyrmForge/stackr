package exits

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"strconv"
)

// Step is one command as argv, never a shell line. Check, when set, runs
// first and a nil error means the step is already done (iptables -C).
// IgnoreErr tolerates a failing step (deleting something that may not exist).
type Step struct {
	Argv      []string
	Check     []string
	IgnoreErr bool
}

// Runner runs one argv and returns an error for a non-zero exit.
type Runner interface {
	Run(ctx context.Context, argv []string) error
}

// RunnerFunc adapts a function to Runner.
type RunnerFunc func(ctx context.Context, argv []string) error

func (f RunnerFunc) Run(ctx context.Context, argv []string) error { return f(ctx, argv) }

// Apply runs the steps in order and stops at the first real failure.
func Apply(ctx context.Context, r Runner, steps []Step) error {
	for _, s := range steps {
		if s.Check != nil && r.Run(ctx, s.Check) == nil {
			continue
		}
		if err := r.Run(ctx, s.Argv); err != nil && !s.IgnoreErr {
			return fmt.Errorf("%v: %w", s.Argv, err)
		}
	}
	return nil
}

// Refused are the destinations an exit never forwards to: private ranges, the
// shared and link-local ranges (cloud metadata lives in 169.254/16).
var Refused = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "169.254.0.0/16"}

// Iface is the wg interface name for the other end of an exit link. It is
// stable and fits the 15 character limit whatever the node id looks like.
func Iface(peerNodeID string) string {
	h := sha1.Sum([]byte(peerNodeID))
	return "wgx-" + hex.EncodeToString(h[:])[:10]
}

func cmd(a ...string) Step   { return Step{Argv: a} }
func quiet(a ...string) Step { return Step{Argv: a, IgnoreErr: true} }
func ipt(table string, rule ...string) []string {
	return append([]string{"iptables", "-t", table}, rule...)
}

// once is "add rule if -C says it is missing". pos "" appends, "1" inserts first.
func once(table, chain, pos string, rule ...string) Step {
	check := ipt(table, append([]string{"-C", chain}, rule...)...)
	add := ipt(table, "-A", chain)
	if pos != "" {
		add = ipt(table, "-I", chain, pos)
	}
	return Step{Argv: append(add, rule...), Check: check}
}

func prefixes(in []string) error {
	if len(in) == 0 {
		return fmt.Errorf("exits: no source ranges")
	}
	for _, s := range in {
		if _, err := netip.ParsePrefix(s); err != nil {
			return fmt.Errorf("exits: bad range %q: %w", s, err)
		}
	}
	return nil
}

func ifaceName(n string) error {
	if n == "" || len(n) > 15 {
		return fmt.Errorf("exits: bad interface name %q", n)
	}
	return nil
}

func hostPort(s string) error {
	if _, p, err := net.SplitHostPort(s); err != nil {
		return fmt.Errorf("exits: bad endpoint %q: %w", s, err)
	} else if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("exits: bad endpoint port %q", s)
	}
	return nil
}

// Link is the node side of one exit: the wg interface towards an exit node and
// the policy routing into it. One Link serves every subject routed over it.
type Link struct {
	Iface   string // see Iface
	KeyFile string // private key file of the exit keypair
	Port    int    // local listen port
	PeerKey string // exit node's public key for this link
	// Endpoint is host:port of the exit node (its public or mesh address).
	Endpoint string
	// Addr is this side's transit address with prefix (10.251.0.1/30).
	Addr string
	// Table is the routing table holding "default dev Iface"; one per exit.
	Table int
	// Sources are the env subnets (or tile addresses) routed over this exit.
	// They feed the raw reply accepts only; rules come from Subject.
	Sources []string
	MTU     int // 0 = 1420
}

func (l Link) validate() error {
	if err := ifaceName(l.Iface); err != nil {
		return err
	}
	if l.KeyFile == "" || l.PeerKey == "" || l.Port < 1 || l.Table < 1 {
		return fmt.Errorf("exits: link %s needs key file, peer key, port and table", l.Iface)
	}
	if _, err := netip.ParsePrefix(l.Addr); err != nil {
		return fmt.Errorf("exits: bad transit address %q: %w", l.Addr, err)
	}
	if err := hostPort(l.Endpoint); err != nil {
		return err
	}
	return prefixes(l.Sources)
}

// Up builds the interface, its default route in the exit's table and the
// firewall accepts. Every step is safe to repeat. Env traffic keeps its
// source address to the exit (STACKR-NAT accepts it instead of masquerading),
// replies skip Docker's per container raw drop, and nothing new may come in
// from an exit.
func (l Link) Up() ([]Step, error) {
	if err := l.validate(); err != nil {
		return nil, err
	}
	mtu := l.MTU
	if mtu == 0 {
		mtu = 1420
	}
	tbl := strconv.Itoa(l.Table)
	s := []Step{
		quiet("ip", "link", "del", l.Iface),
		cmd("ip", "link", "add", l.Iface, "type", "wireguard"),
		cmd("wg", "set", l.Iface, "listen-port", strconv.Itoa(l.Port), "private-key", l.KeyFile,
			"peer", l.PeerKey, "endpoint", l.Endpoint, "allowed-ips", "0.0.0.0/0", "persistent-keepalive", "25"),
		cmd("ip", "addr", "add", l.Addr, "dev", l.Iface),
		cmd("ip", "link", "set", l.Iface, "mtu", strconv.Itoa(mtu), "up"),
		cmd("ip", "route", "replace", "default", "dev", l.Iface, "table", tbl),
		once("nat", "STACKR-NAT", "1", "-o", l.Iface, "-j", "ACCEPT"),
		once("filter", "STACKR-XN", "1", "-i", l.Iface, "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"),
		once("filter", "STACKR-XN", "", "-i", l.Iface, "-j", "DROP"),
	}
	for _, src := range l.Sources {
		s = append(s, once("raw", "STACKR-RAW", "1", "-i", l.Iface, "-d", src, "-j", "ACCEPT"))
	}
	return s, nil
}

// Down removes what Up and the Subject rules made. Failures are ignored: it
// runs on cleanup, where half of it may already be gone.
func (l Link) Down() []Step {
	s := []Step{
		quiet("ip", "route", "flush", "table", strconv.Itoa(l.Table)),
		quiet("ip", "link", "del", l.Iface),
		quiet(ipt("nat", "-D", "STACKR-NAT", "-o", l.Iface, "-j", "ACCEPT")...),
		quiet(ipt("filter", "-D", "STACKR-XN", "-i", l.Iface, "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT")...),
		quiet(ipt("filter", "-D", "STACKR-XN", "-i", l.Iface, "-j", "DROP")...),
	}
	for _, src := range l.Sources {
		s = append(s, quiet(ipt("raw", "-D", "STACKR-RAW", "-i", l.Iface, "-d", src, "-j", "ACCEPT")...))
	}
	return s
}

// Subject is what one ladder routes: source ranges that share a ladder, and
// the ip rule priorities they own. Pref and Pref+1 are used per source, two
// per source, so give subjects non overlapping blocks.
type Subject struct {
	Sources  []string
	PrefBase int
}

func (s Subject) pref(i, off int) string { return strconv.Itoa(s.PrefBase + 2*i + off) }

func (s Subject) validate() error {
	if s.PrefBase < 1 {
		return fmt.Errorf("exits: subject needs a rule priority base")
	}
	return prefixes(s.Sources)
}

// Clear removes the subject's rules; the traffic goes out the own uplink.
func (s Subject) Clear() []Step {
	var st []Step
	for i := range s.Sources {
		st = append(st, quiet("ip", "rule", "del", "pref", s.pref(i, 0)), quiet("ip", "rule", "del", "pref", s.pref(i, 1)))
	}
	return st
}

// Route sends the subject out through the exit table. The suppress rule first
// keeps bridges and the mesh routes of the main table winning; only what
// would fall to the default route goes to the exit.
func (s Subject) Route(table int) ([]Step, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	if table < 1 {
		return nil, fmt.Errorf("exits: bad table %d", table)
	}
	st := s.Clear()
	for i, src := range s.Sources {
		st = append(st,
			cmd("ip", "rule", "add", "pref", s.pref(i, 0), "from", src, "lookup", "main", "suppress_prefixlength", "0"),
			cmd("ip", "rule", "add", "pref", s.pref(i, 1), "from", src, "lookup", strconv.Itoa(table)))
	}
	return st, nil
}

// Blackhole is the exhausted ladder: local routes still work, everything else
// is dropped instead of leaking out the own uplink.
func (s Subject) Blackhole() ([]Step, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	st := s.Clear()
	for i, src := range s.Sources {
		st = append(st,
			cmd("ip", "rule", "add", "pref", s.pref(i, 0), "from", src, "lookup", "main", "suppress_prefixlength", "0"),
			cmd("ip", "rule", "add", "pref", s.pref(i, 1), "from", src, "blackhole"))
	}
	return st, nil
}

// Serve is the exit side of one link: the wg interface for one client node,
// scoped to the env subnets assigned to that node.
type Serve struct {
	Iface   string
	KeyFile string
	Port    int
	// PeerKey and Endpoint of the client node; Endpoint may be empty when the
	// client always dials in.
	PeerKey  string
	Endpoint string
	// Addr is this side's transit address with prefix (10.251.0.2/30), PeerAddr
	// the client's host address (10.251.0.1/32), kept in AllowedIPs.
	Addr     string
	PeerAddr string
	// Subnets are the client's assigned env ranges: the only sources accepted.
	Subnets []string
	Uplink  string // interface to the internet
	Table   int    // routing table for replies
	Mark    int    // connmark and fwmark for this link, unique per link
	Pref    int    // priority of the fwmark ip rule
	MTU     int
}

func (v Serve) validate() error {
	if err := ifaceName(v.Iface); err != nil {
		return err
	}
	if err := ifaceName(v.Uplink); err != nil {
		return err
	}
	if v.KeyFile == "" || v.PeerKey == "" || v.Port < 1 || v.Table < 1 || v.Mark < 1 || v.Pref < 1 {
		return fmt.Errorf("exits: serve %s needs key file, peer key, port, table, mark and pref", v.Iface)
	}
	if v.Endpoint != "" {
		if err := hostPort(v.Endpoint); err != nil {
			return err
		}
	}
	if _, err := netip.ParsePrefix(v.Addr); err != nil {
		return fmt.Errorf("exits: bad transit address %q: %w", v.Addr, err)
	}
	if _, err := netip.ParsePrefix(v.PeerAddr); err != nil {
		return fmt.Errorf("exits: bad peer address %q: %w", v.PeerAddr, err)
	}
	return prefixes(v.Subnets)
}

// AllowedIPs is what the interface accepts from the client: its assigned env
// subnets and its transit address, nothing else, so no other env or node can
// use this exit through the link.
func (v Serve) AllowedIPs() string {
	a := append(append([]string{}, v.Subnets...), v.PeerAddr)
	out := a[0]
	for _, x := range a[1:] {
		out += "," + x
	}
	return out
}

func (v Serve) chain(prefix string) string { return prefix + v.Iface }

// Up builds the exit side. Per link chains (flushed and refilled, so a
// changed subnet list converges) hold: CONNMARK on new flows from the link and
// restore on replies; MASQUERADE of the assigned subnets to the uplink; the
// forward filter that refuses private and metadata destinations. The fwmark
// rule sends replies back into the link instead of the more specific mesh
// route for the same range.
func (v Serve) Up() ([]Step, error) {
	if err := v.validate(); err != nil {
		return nil, err
	}
	mtu := v.MTU
	if mtu == 0 {
		mtu = 1420
	}
	mark := fmt.Sprintf("0x%x", v.Mark)
	tbl, pref := strconv.Itoa(v.Table), strconv.Itoa(v.Pref)
	mg, nt, fl := v.chain("STACKR-XM-"), v.chain("STACKR-XT-"), v.chain("STACKR-XF-")

	wg := []string{"wg", "set", v.Iface, "listen-port", strconv.Itoa(v.Port), "private-key", v.KeyFile, "peer", v.PeerKey}
	if v.Endpoint != "" {
		wg = append(wg, "endpoint", v.Endpoint)
	}
	wg = append(wg, "allowed-ips", v.AllowedIPs())

	s := []Step{
		quiet("ip", "link", "del", v.Iface),
		cmd("ip", "link", "add", v.Iface, "type", "wireguard"),
		cmd(wg...),
		cmd("ip", "addr", "add", v.Addr, "dev", v.Iface),
		cmd("ip", "link", "set", v.Iface, "mtu", strconv.Itoa(mtu), "up"),
	}
	for _, c := range [][2]string{{"mangle", mg}, {"nat", nt}, {"filter", fl}} {
		s = append(s, quiet(ipt(c[0], "-N", c[1])...), cmd(ipt(c[0], "-F", c[1])...))
	}
	s = append(s,
		cmd(ipt("mangle", "-A", mg, "-i", v.Iface, "-m", "conntrack", "--ctstate", "NEW", "-j", "CONNMARK", "--set-mark", mark)...),
		cmd(ipt("mangle", "-A", mg, "-m", "connmark", "--mark", mark, "-j", "CONNMARK", "--restore-mark")...))
	for _, sub := range v.Subnets {
		s = append(s, cmd(ipt("nat", "-A", nt, "-s", sub, "-o", v.Uplink, "-j", "MASQUERADE")...))
	}
	s = append(s, cmd(ipt("filter", "-A", fl, "-o", v.Iface, "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT")...))
	for _, d := range Refused {
		s = append(s, cmd(ipt("filter", "-A", fl, "-i", v.Iface, "-d", d, "-j", "DROP")...))
	}
	s = append(s,
		cmd(ipt("filter", "-A", fl, "-i", v.Iface, "-o", v.Uplink, "-j", "ACCEPT")...),
		cmd(ipt("filter", "-A", fl, "-i", v.Iface, "-j", "DROP")...),
		once("mangle", "PREROUTING", "1", "-j", mg),
		once("nat", "POSTROUTING", "1", "-j", nt),
		once("filter", "DOCKER-USER", "1", "-j", fl),
		// The client may ping the transit address for health; nothing else
		// from the link reaches this host.
		once("filter", "INPUT", "", "-i", v.Iface, "-j", "DROP"),
		once("filter", "INPUT", "1", "-i", v.Iface, "-p", "icmp", "--icmp-type", "echo-request", "-j", "ACCEPT"),
	)
	for _, sub := range v.Subnets {
		s = append(s, cmd("ip", "route", "replace", sub, "dev", v.Iface, "table", tbl))
	}
	s = append(s,
		quiet("ip", "rule", "del", "pref", pref),
		cmd("ip", "rule", "add", "pref", pref, "fwmark", mark, "lookup", tbl))
	return s, nil
}

// Down removes the exit side of the link.
func (v Serve) Down() []Step {
	mg, nt, fl := v.chain("STACKR-XM-"), v.chain("STACKR-XT-"), v.chain("STACKR-XF-")
	s := []Step{
		quiet("ip", "rule", "del", "pref", strconv.Itoa(v.Pref)),
		quiet("ip", "route", "flush", "table", strconv.Itoa(v.Table)),
		quiet("ip", "link", "del", v.Iface),
		quiet(ipt("mangle", "-D", "PREROUTING", "-j", mg)...),
		quiet(ipt("nat", "-D", "POSTROUTING", "-j", nt)...),
		quiet(ipt("filter", "-D", "DOCKER-USER", "-j", fl)...),
		quiet(ipt("filter", "-D", "INPUT", "-i", v.Iface, "-j", "DROP")...),
		quiet(ipt("filter", "-D", "INPUT", "-i", v.Iface, "-p", "icmp", "--icmp-type", "echo-request", "-j", "ACCEPT")...),
	}
	for _, c := range [][2]string{{"mangle", mg}, {"nat", nt}, {"filter", fl}} {
		s = append(s, quiet(ipt(c[0], "-F", c[1])...), quiet(ipt(c[0], "-X", c[1])...))
	}
	return s
}

// Fence keeps the env ranges of a node without egress off its own uplink: the
// ladder blackholes their internet, and this drops whatever still tries to
// leave by the uplink. Private and link-local destinations return from the
// chain first, so LAN and private traffic keeps flowing, and so do replies of
// connections that came in through a published port (DNAT). Exit links and
// stackr0 are other interfaces and never meet the DROP.
// ponytail: IPv4 only; env networks have no IPv6 today.
type Fence struct {
	Sources []string // env subnets
	Uplink  string   // interface of the default route
}

// FenceChain is the filter chain DOCKER-USER jumps into.
const FenceChain = "STACKR-FENCE"

// Up flushes and refills the chain and adds the jump once.
func (f Fence) Up() ([]Step, error) {
	if err := prefixes(f.Sources); err != nil {
		return nil, err
	}
	if err := ifaceName(f.Uplink); err != nil {
		return nil, err
	}
	s := []Step{quiet(ipt("filter", "-N", FenceChain)...), cmd(ipt("filter", "-F", FenceChain)...),
		cmd(ipt("filter", "-A", FenceChain, "-m", "conntrack", "--ctstate", "DNAT", "-j", "RETURN")...)}
	for _, src := range f.Sources {
		for _, d := range Refused {
			s = append(s, cmd(ipt("filter", "-A", FenceChain, "-s", src, "-d", d, "-j", "RETURN")...))
		}
		s = append(s, cmd(ipt("filter", "-A", FenceChain, "-s", src, "-o", f.Uplink, "-j", "DROP")...))
	}
	return append(s, once("filter", "DOCKER-USER", "1", "-j", FenceChain)), nil
}

// Down removes the jump and the chain.
func (f Fence) Down() []Step {
	return []Step{
		quiet(ipt("filter", "-D", "DOCKER-USER", "-j", FenceChain)...),
		quiet(ipt("filter", "-F", FenceChain)...),
		quiet(ipt("filter", "-X", FenceChain)...),
	}
}

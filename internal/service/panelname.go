package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/dnscap"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/settings"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// panelname.go is the panel's own DNS name. The panel_address setting says
// what its record points at (auto, ip:<addr>, self); checkPanelName tells
// the admin, with problem banners, when the name does not resolve there or
// cannot be reached from outside. Nothing here repairs anything.

const (
	panelAddrAlert    = "alert.panel.address"
	panelResolveAlert = "alert.panel.resolve"
	panelReachAlert   = "alert.panel.reach"
)

// outsideView is what the mesh sees of the panel's node from outside:
// IPv4 is the public address nodes in other networks see its network come
// from, Vantage the id of one of them ("" = no node outside to ask).
type outsideView struct{ IPv4, Vantage string }

// panelSeams are the outside world's three questions: the mesh, an HTTPS
// probe from a node, and a public DNS lookup. nil = the real one; tests
// set them per orchestrator.
type panelSeams struct {
	outside func(ctx context.Context, nodeID string) outsideView
	probe   func(ctx context.Context, vantage, url string) (who string, ok bool)
	resolve func(ctx context.Context, host string) ([]string, error)
}

// WithPanelSeams replaces what the panel name check asks of the world in
// tests: the mesh (the address nodes outside see for a node's network, and
// one of them), an HTTPS probe from it, and a public DNS lookup. nil keeps
// the real one.
func WithPanelSeams(outside func(ctx context.Context, nodeID string) (ipv4, vantage string), probe func(ctx context.Context, vantage, url string) (string, bool), resolve func(ctx context.Context, host string) ([]string, error)) Option {
	return func(o *options) {
		o.panelSeam = panelSeams{probe: probe, resolve: resolve}
		if outside != nil {
			o.panelSeam.outside = func(ctx context.Context, id string) outsideView {
				a, v := outside(ctx, id)
				return outsideView{IPv4: a, Vantage: v}
			}
		}
	}
}

// panelFacts is what the outside world said, asked once per sync before
// the records lock (withPanelFacts) and reused by the wants and the check:
// the mesh's view (probes can take seconds), and the panel name's public
// addresses (looked up only when auto may write the seen address). One
// goroutine uses it at a time.
type panelFacts struct {
	out            outsideView
	haveOut        bool
	ips            []string
	ipsOK, haveIPs bool
}

type panelFactsKey struct{}

// factsOf is the sync's panelFacts in ctx, or a fresh one (asked lazily).
func factsOf(ctx context.Context) *panelFacts {
	if f, ok := ctx.Value(panelFactsKey{}).(*panelFacts); ok {
		return f
	}
	return &panelFacts{}
}

// withPanelFacts puts a panelFacts in ctx unless one is there, and asks
// what the wants will need: the mesh's view and the name's lookup, but only
// when auto could write the seen address. all asks the mesh regardless (the
// check needs the vantage).
func (o *Orchestrator) withPanelFacts(ctx context.Context, all bool) context.Context {
	if _, ok := ctx.Value(panelFactsKey{}).(*panelFacts); ok {
		return ctx
	}
	ctx = context.WithValue(ctx, panelFactsKey{}, &panelFacts{})
	p := o.panelDomain(ctx)
	def, err := o.DefaultIngress(ctx)
	if p == "" || err != nil || def == "" {
		return ctx
	}
	if all {
		o.outsideOf(ctx, def)
	}
	_, _ = o.panelTarget(ctx, def)
	return ctx
}

// outsideOf is the outsideView of node id, asked once per panelFacts.
func (o *Orchestrator) outsideOf(ctx context.Context, id string) outsideView {
	f := factsOf(ctx)
	if f.haveOut {
		return f.out
	}
	f.out, f.haveOut = o.askOutside(ctx, id), true
	return f.out
}

func (o *Orchestrator) askOutside(ctx context.Context, id string) outsideView {
	if f := o.panelSeam.outside; f != nil {
		return f(ctx, id)
	}
	if o.mesh == nil {
		return outsideView{}
	}
	m, err := o.ClusterMap(ctx)
	if err != nil {
		return outsideView{}
	}
	return outsideView{IPv4: natIPv4(m, id), Vantage: outsideNode(m, id)}
}

// outsideNode is an online worker with a public address in another network
// than node id's: the place to ask "can you reach it" from. "" = none.
func outsideNode(m ClusterMap, id string) string {
	i := slices.IndexFunc(m.Nodes, func(n ClusterNode) bool { return n.ID == id })
	if i < 0 {
		return ""
	}
	for _, n := range m.Nodes {
		if n.Role == node.Worker && n.Status == NodeOnline && len(n.Public) > 0 && n.Net != m.Nodes[i].Net {
			return n.ID
		}
	}
	return ""
}

// panelTarget is where the panel name points: Mode is the panel_address
// mode, V4 and V6 the addresses (both unset: nothing to write).
type panelTarget struct {
	Mode   string
	V4, V6 netip.Addr
	// Seen: V4 came from outside the box (the router's), not from the
	// node's own interfaces.
	Seen bool
	// Saw is the outside address stackr sees but does not write: the name
	// already resolves to HeldAt (or could not be looked up: HeldAt nil).
	Saw    netip.Addr
	HeldAt []string
}

func (t panelTarget) none() bool { return !t.V4.IsValid() && !t.V6.IsValid() }

// publicUnicast is an address a node on the internet can own: no private,
// loopback, link-local or carrier-grade NAT (100.64.0.0/10) address.
func publicUnicast(a netip.Addr) bool {
	return a.IsGlobalUnicast() && !a.IsPrivate() && !a.IsLoopback() && !a.IsLinkLocalUnicast() && !cgnat.Contains(a)
}

// panelTarget is the target for the default ingress node def, read fresh:
// the records sync and the daily check both ask, so a changed home IP
// moves the record. In auto, the node's own address wins; without an own
// IPv4 the address the mesh sees from outside fills it in, but only when
// the name resolves nowhere, to that address, or to what stackr wrote: a
// name already pointing elsewhere (a wildcard, a route) is held.
func (o *Orchestrator) panelTarget(ctx context.Context, def string) (panelTarget, error) {
	raw, _ := o.settings.Get(ctx, "panel_address")
	mode, ip, _, _ := settings.ParsePanelAddress(raw)
	t := panelTarget{Mode: mode}
	switch mode {
	case settings.PanelSelf:
		return t, nil
	case settings.PanelIP:
		if ip.Is4() {
			t.V4 = ip
		} else {
			t.V6 = ip
		}
		return t, nil
	}
	var err error
	if t.V4, t.V6, _, err = o.nodeAddrs(ctx, def); err != nil {
		return t, err
	}
	if t.V4.IsValid() {
		return t, nil
	}
	a, err := netip.ParseAddr(o.outsideOf(ctx, def).IPv4)
	if err != nil || !a.Is4() || !publicUnicast(a) {
		return t, nil
	}
	ips, ok := o.panelIPs(ctx)
	// a private or loopback answer (hosts file, split horizon) says nothing
	// of what the world sees
	ips = slices.DeleteFunc(slices.Clone(ips), func(s string) bool {
		a, err := netip.ParseAddr(s)
		return err != nil || !publicUnicast(a.Unmap())
	})
	switch {
	case !ok: // cannot tell what the name does: write nothing
		t.Saw = a
	case o.mayWriteSeen(ctx, ips, a):
		t.V4, t.Seen = a, true
	default:
		t.Saw, t.HeldAt = a, ips
	}
	return t, nil
}

// mayWriteSeen: stackr already has a record at the panel name (it wrote
// it; a hand edit is protected where records are applied), or the name
// resolves nowhere or to the seen address.
func (o *Orchestrator) mayWriteSeen(ctx context.Context, ips []string, seen netip.Addr) bool {
	own := o.panelRecordValues(ctx)
	if len(own) > 0 {
		return true
	}
	for _, s := range ips {
		if s != seen.String() && !slices.Contains(own, s) {
			return false
		}
	}
	return true
}

// panelHasA is whether stackr has an A record at the panel name.
func (o *Orchestrator) panelHasA(ctx context.Context) bool {
	return slices.ContainsFunc(o.panelRecordValues(ctx), func(v string) bool {
		a, err := netip.ParseAddr(v)
		return err == nil && a.Is4()
	})
}

// panelRecordValues is the values of the panel name's records stackr made.
func (o *Orchestrator) panelRecordValues(ctx context.Context) []string {
	p := o.panelDomain(ctx)
	rows, err := o.store.DNSRecords.List(ctx)
	if err != nil {
		return nil
	}
	var out []string
	for _, r := range rows {
		host := r.Zone
		if r.Name != "@" {
			host = r.Name + "." + r.Zone
		}
		if r.Kind == store.RecordName && strings.EqualFold(host, p) {
			out = append(out, r.Value)
		}
	}
	return out
}

// panelIPs is the panel name's addresses, asked once per panelFacts. ok
// false: the lookup failed (timeout, SERVFAIL), so nothing is known; a name
// with no address at all is nil, true.
func (o *Orchestrator) panelIPs(ctx context.Context) (ips []string, ok bool) {
	f := factsOf(ctx)
	if !f.haveIPs {
		f.ips, f.ipsOK = o.lookupOK(ctx, o.panelDomain(ctx))
		f.haveIPs = true
	}
	return f.ips, f.ipsOK
}

// lookupOK is lookup with its own short time limit (never the sync's) and
// the failures told apart: no such name or no address is an answer (nil,
// true), anything else is not (nil, false).
func (o *Orchestrator) lookupOK(ctx context.Context, host string) ([]string, bool) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	ips, err := o.lookup(ctx, host)
	if err == nil {
		return ips, true
	}
	if de := (*net.DNSError)(nil); errors.As(err, &de) && de.IsNotFound {
		return nil, true
	}
	return nil, false
}

// PanelAddressInfo is the panel_address setting as the Domains screens
// show it. Value is the stored text ("auto" when unset), Seen what stackr
// would point the panel at in auto mode while the node has no public
// address of its own, "" when it cannot tell. Behind: the node has no
// public address of its own, so the admin may need to choose.
type PanelAddressInfo struct {
	Value, Seen string
	Behind      bool
	Wildcard    bool // the panel name resolves only through a wildcard record
}

// PanelAddress is the PanelAddressInfo for the panel name set now.
func (o *Orchestrator) PanelAddress(ctx context.Context) (PanelAddressInfo, error) {
	var out PanelAddressInfo
	raw, _ := o.settings.Get(ctx, "panel_address")
	_, _, out.Value, _ = settings.ParsePanelAddress(raw)
	if out.Value == "" {
		out.Value = settings.PanelAuto
	}
	p := o.panelDomain(ctx)
	def, err := o.DefaultIngress(ctx)
	if p == "" || err != nil || def == "" {
		return out, nil
	}
	if v4, _, _, err := o.nodeAddrs(ctx, def); err == nil && !v4.IsValid() {
		out.Behind = true
		if a, err := netip.ParseAddr(o.outsideOf(ctx, def).IPv4); err == nil && publicUnicast(a) {
			out.Seen = a.String()
		}
	}
	// once stackr has a record for the name, a wildcard no longer hides it
	out.Wildcard = len(o.panelRecordValues(ctx)) == 0 && o.onlyWildcardCached(ctx, p)
	return out, nil
}

// lookup is a public DNS lookup (a test's fake, or resolveHost).
func (o *Orchestrator) lookup(ctx context.Context, host string) ([]string, error) {
	if f := o.panelSeam.resolve; f != nil {
		return f(ctx, host)
	}
	return resolveHost(ctx, host)
}

type wildHit struct {
	at  time.Time
	yes bool
}

// onlyWildcardCached is onlyWildcard, reused for a few minutes: the DNS step
// polls and each answer costs two lookups.
func (o *Orchestrator) onlyWildcardCached(ctx context.Context, host string) bool {
	if v, ok := o.wildcard.Load(host); ok && time.Since(v.(wildHit).at) < 2*time.Minute {
		return v.(wildHit).yes
	}
	yes := o.onlyWildcard(ctx, host)
	o.wildcard.Store(host, wildHit{time.Now(), yes})
	return yes
}

// onlyWildcard says whether host resolves, and a name that cannot have a
// record of its own resolves to the same addresses: a wildcard answers it.
// ponytail: a record of its own with the wildcard's value reads the same;
// the page only warns, nothing acts on it.
func (o *Orchestrator) onlyWildcard(ctx context.Context, host string) bool {
	_, parent, ok := strings.Cut(host, ".")
	if !ok || !strings.Contains(parent, ".") {
		return false
	}
	ips, err := o.lookup(ctx, host)
	if err != nil || len(ips) == 0 {
		return false
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	rnd, err := o.lookup(ctx, "stackr-"+hex.EncodeToString(b)+"."+parent)
	if err != nil || len(rnd) == 0 {
		return false
	}
	slices.Sort(ips)
	slices.Sort(rnd)
	return slices.Equal(ips, rnd)
}

// panelResolves is whether the resolved addresses are the target's: one at
// least, and none that is not one of its addresses (a stale AAAA elsewhere
// sends IPv6 clients to the wrong server).
func panelResolves(ips []string, t panelTarget) bool {
	hit := false
	for _, s := range ips {
		a, err := netip.ParseAddr(s)
		if err != nil {
			continue
		}
		if a.Unmap() == t.V4 || a == t.V6 {
			hit = true
		} else {
			return false
		}
	}
	return hit
}

// outsiders keeps the addresses of ips a name can point at from outside:
// private and loopback answers (/etc/hosts, split horizon) say nothing of
// what the world sees, unless they are the target's own.
func outsiders(ips []string, t panelTarget) []string {
	return slices.DeleteFunc(slices.Clone(ips), func(s string) bool {
		a, err := netip.ParseAddr(s)
		return err != nil || (a != t.V4 && a != t.V6 && !publicUnicast(a.Unmap()))
	})
}

// checkPanelName opens or clears the panel name's problems: no address to
// point at (auto found none), the name not resolving to the target, the
// name not reachable from outside. It runs after every records sync and
// daily (dnssync.go), and from the tick while one is open.
func (o *Orchestrator) checkPanelName(ctx context.Context) {
	// checks run one at a time (background only), so verdicts land in the
	// order they were seen
	o.panelCheckMu.Lock()
	defer o.panelCheckMu.Unlock()
	// a panel record write that lands mid-check makes what it sees stale:
	// it judges nothing, the write's own check does. The slow part (lookups,
	// probes) runs outside panelMu; the verdict is applied under panelMu,
	// which a record write (reconcileDNS) holds while it writes and bumps
	// panelGen. gen is read before due: a writer stores due before gen.
	gen := o.panelGen.Load()
	due := o.panelDueAt.Load()
	var verdict []func()
	open := func(ctx context.Context, k, sev, msg string) {
		verdict = append(verdict, func() { o.openProblem(ctx, k, sev, msg) })
	}
	clear := func(ctx context.Context, k string) {
		verdict = append(verdict, func() { o.clearProblem(ctx, k) })
	}
	defer func() {
		o.panelMu.Lock()
		defer o.panelMu.Unlock()
		if o.panelGen.Load() != gen {
			return
		}
		for _, f := range verdict {
			f()
		}
	}()
	p := o.panelDomain(ctx)
	zones, err := o.dns.All(ctx)
	if err != nil {
		return
	}
	def, derr := o.DefaultIngress(ctx)
	if _, managed := dnscap.ZoneOf(p, zones); p == "" || !managed || derr != nil || def == "" {
		for _, k := range []string{panelAddrAlert, panelResolveAlert, panelReachAlert} {
			clear(ctx, k)
		}
		o.panelDueAt.CompareAndSwap(due, 0) // nothing to judge
		return
	}
	ctx = o.withPanelFacts(ctx, true)
	t, err := o.panelTarget(ctx, def)
	if err != nil {
		return
	}
	switch {
	case t.Saw.IsValid() && len(t.HeldAt) > 0:
		open(ctx, panelAddrAlert, SevWarning, fmt.Sprintf("%s points at %s; stackr sees %s. Type the address to use, or say you handle that record yourself.", p, strings.Join(t.HeldAt, ", "), t.Saw))
	case t.Saw.IsValid() && len(t.HeldAt) == 0: // the lookup failed: the alert stays as it is
	case t.Mode == settings.PanelAuto && !t.V4.IsValid() && !t.Saw.IsValid() && (t.none() || o.panelHasA(ctx)):
		open(ctx, panelAddrAlert, SevWarning, "stackr cannot tell which address "+p+" should point at. Type an address, or say you handle that record yourself.")
	case o.hasProblem(ctx, panelAddrAlert):
		clear(ctx, panelAddrAlert)
		o.dnsKick.Kick() // an address is known now: write the record
	}
	// A lookup that fails (timeout, SERVFAIL) keeps the alert as it is; an
	// answer with no address (even in self mode: the original outage), or
	// with the wrong ones, opens it. Private answers say nothing: clear.
	resolved := !o.hasProblem(ctx, panelResolveAlert)
	// right after a write the resolver may still answer the old value: the
	// next tick's recheck judges it (panelDue)
	// (any check inside that TTL is as fresh as the writing one)
	fresh := due != 0 && time.Now().UnixNano() < due
	if ips, ok := o.lookupOK(ctx, p); ok && !fresh {
		o.panelDueAt.CompareAndSwap(due, 0) // judged on a real answer; a failed lookup stays due
		switch pub := outsiders(ips, t); {
		case len(ips) == 0:
			open(ctx, panelResolveAlert, SevCritical, p+" does not resolve to this server.")
			resolved = false
		case len(pub) == 0 || t.Mode == settings.PanelSelf || t.none():
			clear(ctx, panelResolveAlert)
			resolved = true
		case !panelResolves(pub, t):
			open(ctx, panelResolveAlert, SevCritical, p+" does not resolve to this server.")
			resolved = false
		default:
			clear(ctx, panelResolveAlert)
			resolved = true
		}
	}
	if fresh { // the record is minutes old: the next check judges it, probe and all
		return
	}
	v := o.outsideOf(ctx, def)
	if !resolved || v.Vantage == "" || o.cfg.TLSOff {
		clear(ctx, panelReachAlert) // a name that does not resolve is the problem, not the router
		return
	}
	ctl, err := o.controllerID(ctx)
	if err != nil {
		return
	}
	probe := o.probe
	if f := o.panelSeam.probe; f != nil {
		probe = f
	}
	who, ok := probe(ctx, v.Vantage, "https://"+p+probePath(ctl))
	switch {
	case !ok:
	case who == def:
		clear(ctx, panelReachAlert)
	default:
		name := def
		if n, err := o.store.Nodes.Get(ctx, def); err == nil {
			name = n.Name
		}
		open(ctx, panelReachAlert, SevCritical, fmt.Sprintf("%s is not reachable from outside: check the router forwards 443 to %s.", p, name))
	}
}

// recheckPanelName runs the check again from the tick while one of its
// problems is open, so it clears when the record or the router is fixed
// without waiting for the daily one. Off the tick, one at a time.
func (o *Orchestrator) recheckPanelName(ctx context.Context) {
	rows, err := o.store.Settings.ListPrefix(ctx, problemPrefix+"panel.")
	due := o.panelDueAt.Load()
	if err != nil || (due != 0 && time.Now().UnixNano() < due) || (len(rows) == 0 && due == 0) || !o.panelRecheck.CompareAndSwap(false, true) {
		return
	}
	o.detach(ctx, func(ctx context.Context) {
		defer o.panelRecheck.Store(false)
		o.checkPanelName(ctx)
	})
}

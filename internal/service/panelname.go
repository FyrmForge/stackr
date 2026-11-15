package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/dnscap"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/settings"
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

// outsideOf is the outsideView of node id.
func (o *Orchestrator) outsideOf(ctx context.Context, id string) outsideView {
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
	// Seen: the address came from outside the box (the router's), not from
	// the node's own interfaces.
	Seen bool
}

func (t panelTarget) none() bool { return !t.V4.IsValid() && !t.V6.IsValid() }

// panelTarget is the target for the default ingress node def, read fresh:
// the records sync and the daily check both ask, so a changed home IP
// moves the record.
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
	if t.none() {
		if a, err := netip.ParseAddr(o.outsideOf(ctx, def).IPv4); err == nil {
			t.V4, t.Seen = a, true
		}
	}
	return t, nil
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
	if v4, v6, _, err := o.nodeAddrs(ctx, def); err == nil && !v4.IsValid() && !v6.IsValid() {
		out.Behind = true
		out.Seen = o.outsideOf(ctx, def).IPv4
	}
	out.Wildcard = o.onlyWildcardCached(ctx, p)
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
// least, and none of a family the target has that it does not list.
func panelResolves(ips []string, t panelTarget) bool {
	hit := false
	for _, s := range ips {
		a, err := netip.ParseAddr(s)
		if err != nil {
			continue
		}
		switch {
		case a == t.V4 || a == t.V6:
			hit = true
		case a.Is4() && t.V4.IsValid(), a.Is6() && t.V6.IsValid():
			return false
		}
	}
	return hit
}

// checkPanelName opens or clears the panel name's problems: no address to
// point at (auto found none), the name not resolving to the target, the
// name not reachable from outside. It runs after every records sync and
// daily (dnssync.go), and from the tick while one is open.
func (o *Orchestrator) checkPanelName(ctx context.Context) {
	p := o.panelDomain(ctx)
	zones, err := o.dns.All(ctx)
	if err != nil {
		return
	}
	def, derr := o.DefaultIngress(ctx)
	if _, managed := dnscap.ZoneOf(p, zones); p == "" || !managed || derr != nil || def == "" {
		for _, k := range []string{panelAddrAlert, panelResolveAlert, panelReachAlert} {
			o.clearProblem(ctx, k)
		}
		return
	}
	t, err := o.panelTarget(ctx, def)
	if err != nil {
		return
	}
	switch {
	case t.Mode == settings.PanelAuto && t.none():
		o.openProblem(ctx, panelAddrAlert, SevWarning, "stackr cannot tell which address "+p+" should point at. Type an address, or say you handle that record yourself.")
	case o.hasProblem(ctx, panelAddrAlert):
		o.clearProblem(ctx, panelAddrAlert)
		o.dnsKick.Kick() // an address is known now: write the record
	}
	resolved := true
	if t.Mode == settings.PanelSelf || t.none() {
		o.clearProblem(ctx, panelResolveAlert)
	} else if ips, err := o.lookup(ctx, p); err != nil || !panelResolves(ips, t) {
		o.openProblem(ctx, panelResolveAlert, SevCritical, p+" does not resolve to this server.")
		resolved = false
	} else {
		o.clearProblem(ctx, panelResolveAlert)
	}
	v := o.outsideOf(ctx, def)
	if !resolved || v.Vantage == "" || o.cfg.TLSOff {
		o.clearProblem(ctx, panelReachAlert) // a name that does not resolve is the problem, not the router
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
		o.clearProblem(ctx, panelReachAlert)
	default:
		name := def
		if n, err := o.store.Nodes.Get(ctx, def); err == nil {
			name = n.Name
		}
		o.openProblem(ctx, panelReachAlert, SevCritical, fmt.Sprintf("%s is not reachable from outside: check the router forwards 443 to %s.", p, name))
	}
}

// recheckPanelName runs the check again from the tick while one of its
// problems is open, so it clears when the record or the router is fixed
// without waiting for the daily one. Off the tick, one at a time.
func (o *Orchestrator) recheckPanelName(ctx context.Context) {
	rows, err := o.store.Settings.ListPrefix(ctx, problemPrefix+"panel.")
	if err != nil || len(rows) == 0 || !o.panelRecheck.CompareAndSwap(false, true) {
		return
	}
	o.detach(ctx, func(ctx context.Context) {
		defer o.panelRecheck.Store(false)
		o.checkPanelName(ctx)
	})
}

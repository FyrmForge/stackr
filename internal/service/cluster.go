package service

import (
	"context"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/FyrmForge/stackr/internal/exits"
	"github.com/FyrmForge/stackr/internal/mesh"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/worker"
)

// The cluster map (lane U2): the facts the Cluster canvas draws. The verb
// gathers (node facts, env health, ingress domains, exit ladders, backup
// destinations); buildClusterMap decides (networks, links, markers) and is
// pure, so it is tested with plain values. Geometry is the UI's.

// Env health on a node's card, a dot each.
const (
	HealthHealthy  = "healthy"
	HealthDegraded = "degraded"
	HealthDown     = "down"
	HealthStopped  = "stopped"
	HealthMoving   = "moving"
)

// Node status words on the map.
const (
	NodeOnline      = "online"
	NodeUnreachable = "unreachable" // a worker that did not answer just now
)

// Tunnel kinds: how the two ends reach each other.
const (
	LinkLAN      = "LAN"      // same network, no public node in it
	LinkPrivate  = "private"  // same network that has a public node (a cloud private net)
	LinkInternet = "internet" // different networks
)

// ClusterMap is the whole picture.
type ClusterMap struct {
	Nodes    []ClusterNode
	Networks []ClusterNet
	Links    []ClusterLink
	Backups  []ClusterBackup
}

type ClusterNode struct {
	ID, Name, Role string
	Tags           []string
	Status         string
	Docker         string   // the docker server version the node reported; "" = none
	Net            string   // ClusterNet.ID
	Public         []string // public addresses on its interfaces
	Envs           []ClusterEnv
	// Domains are the hosts this node's ingress answers on: the tiles whose
	// ingress pick is this node, the rest on the controller.
	Domains []string
	// ExitEnvs are the env paths whose traffic leaves the internet here.
	ExitEnvs []string
}

type ClusterEnv struct {
	ID, Path, Href string // Path is org/stack/env
	Health         string
}

// ClusterNet is a box on the map: the nodes that share a private subnet.
type ClusterNet struct {
	ID    string
	CIDR  string // "" = nothing reported
	Nodes []string
	// Router: nothing in the network has a public address, so it is
	// reached through a NAT router.
	Router bool
}

// ClusterLink is one tunnel between a connected pair. MS < 0 = not measured:
// only the controller measures, and only to its own workers.
type ClusterLink struct {
	A, B, Kind string
	MS         float64
}

// ClusterBackup is an external S3 destination and the nodes that back up to it.
type ClusterBackup struct {
	ID, Name string
	Nodes    []string
}

// clusterNodeIn is one node as gather saw it.
type clusterNodeIn struct {
	Node     store.Node
	Facts    *worker.Facts // nil = never reported
	Up       bool          // answered just now
	RTT      time.Duration // 0 = not measured
	Envs     []ClusterEnv
	Domains  []string
	ExitEnvs []string
}

// excluded are the interfaces that are not a LAN the node sits on.
func excludedIface(name string) bool {
	for _, p := range []string{"lo", "wg", "docker", "br-", "veth", "stackr", "tailscale", "virbr", "cni", "flannel"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// netPrefix is the node's network: the masked prefix of the first IPv4
// private address that is not the mesh's own.
func netPrefix(f worker.Facts) (netip.Prefix, bool) {
	for _, i := range f.Interfaces {
		if excludedIface(i.Name) || i.Public {
			continue
		}
		for _, s := range i.Prefixes {
			p, err := netip.ParsePrefix(s)
			a := p.Addr().Unmap()
			if err != nil || !a.Is4() || !a.IsPrivate() || mesh.Supernet.Contains(a) || mesh.Transit.Contains(a) {
				continue
			}
			return p.Masked(), true
		}
	}
	return netip.Prefix{}, false
}

func buildClusterMap(in []clusterNodeIn) ClusterMap {
	var m ClusterMap
	nets := map[string]*ClusterNet{}
	var order []string
	for _, n := range in {
		cn := ClusterNode{
			ID: n.Node.ID, Name: n.Node.Name, Role: n.Node.Role, Tags: node.Tags(n.Node),
			Envs: n.Envs, Domains: n.Domains, ExitEnvs: n.ExitEnvs, Status: n.Node.Status,
		}
		switch {
		case n.Node.Role == node.Controller, n.Up && n.Node.Status == node.StatusOnline:
			cn.Status = NodeOnline
		case n.Node.Status == node.StatusOnline:
			cn.Status = NodeUnreachable
		}
		key := "unknown"
		if f := n.Facts; f != nil {
			cn.Docker = f.Docker
			for _, a := range publicAddrs(*f) {
				cn.Public = append(cn.Public, a.String())
			}
			if p, ok := netPrefix(*f); ok {
				key = p.String()
			} else if len(cn.Public) > 0 {
				key = "public:" + n.Node.ID
			}
		}
		cn.Net = key
		if nets[key] == nil {
			c := &ClusterNet{ID: key}
			if p, err := netip.ParsePrefix(key); err == nil {
				c.CIDR = p.String()
			}
			nets[key] = c
			order = append(order, key)
		}
		nets[key].Nodes = append(nets[key].Nodes, cn.ID)
		m.Nodes = append(m.Nodes, cn)
	}
	// The controller's network first, then by CIDR; "unknown" last.
	ctl := ""
	for _, n := range m.Nodes {
		if n.Role == node.Controller {
			ctl = n.Net
		}
	}
	slices.SortStableFunc(order, func(a, b string) int {
		rank := func(k string) int {
			switch k {
			case ctl:
				return 0
			case "unknown":
				return 2
			}
			return 1
		}
		if ra, rb := rank(a), rank(b); ra != rb {
			return ra - rb
		}
		return strings.Compare(a, b)
	})
	byID := map[string]ClusterNode{}
	for _, n := range m.Nodes {
		byID[n.ID] = n
	}
	for _, k := range order {
		c := nets[k]
		c.Router = k != "unknown" && !slices.ContainsFunc(c.Nodes, func(id string) bool { return len(byID[id].Public) > 0 })
		m.Networks = append(m.Networks, *c)
	}
	// One tunnel per connected pair; a node that does not answer has none up.
	rtt := map[string]time.Duration{}
	for _, n := range in {
		rtt[n.Node.ID] = n.RTT
	}
	for i, a := range m.Nodes {
		for _, b := range m.Nodes[i+1:] {
			if a.Status != NodeOnline || b.Status != NodeOnline {
				continue
			}
			l := ClusterLink{A: a.ID, B: b.ID, Kind: LinkInternet, MS: -1}
			if a.Net == b.Net && a.Net != "unknown" {
				l.Kind = LinkLAN
				if !nets[a.Net].Router {
					l.Kind = LinkPrivate
				}
			}
			if d := rtt[b.ID]; a.Role == node.Controller && d > 0 {
				l.MS = float64(d) / float64(time.Millisecond)
			} else if d := rtt[a.ID]; b.Role == node.Controller && d > 0 {
				l.MS = float64(d) / float64(time.Millisecond)
			}
			m.Links = append(m.Links, l)
		}
	}
	return m
}

// ClusterMap is the Cluster canvas's facts. Admin only (the caller's gate).
func (o *Orchestrator) ClusterMap(ctx context.Context) (ClusterMap, error) {
	ctl, err := o.controllerID(ctx)
	if err != nil {
		return ClusterMap{}, err
	}
	ns, err := o.nodeLeaf().List(ctx)
	if err != nil {
		return ClusterMap{}, err
	}
	live := slices.DeleteFunc(ns, func(n store.Node) bool { return n.Status == node.StatusPending })
	in := make([]clusterNodeIn, len(live))
	var wg sync.WaitGroup
	for i, n := range live {
		in[i].Node = n
		wg.Add(1)
		go func() {
			defer wg.Done()
			in[i].Facts, in[i].Up, in[i].RTT = o.clusterFacts(ctx, n)
		}()
	}
	wg.Wait()

	hosts, err := o.hostsByTile(ctx)
	if err != nil {
		return ClusterMap{}, err
	}
	at := map[string]int{}
	for i, n := range in {
		at[n.Node.ID] = i
	}
	moving, err := o.movingEnvs(ctx)
	if err != nil {
		return ClusterMap{}, err
	}
	names := &pathNames{o: o, stacks: map[string]store.Stack{}, orgs: map[string]store.Org{}}
	for i, n := range in {
		envs, err := o.store.Environments.ListByNode(ctx, n.Node.ID)
		if err != nil {
			return ClusterMap{}, err
		}
		for _, e := range envs {
			org, st, err := names.of(ctx, e.StackID)
			if err != nil {
				return ClusterMap{}, err
			}
			ce := ClusterEnv{ID: e.ID, Path: org.Slug + "/" + st.Slug + "/" + e.Slug, Health: o.envHealth(ctx, e, n.Up || n.Node.ID == ctl, moving[e.ID])}
			ce.Href = "/" + ce.Path
			in[i].Envs = append(in[i].Envs, ce)
			ts, err := o.store.Tiles.ListByEnv(ctx, e.ID)
			if err != nil {
				return ClusterMap{}, err
			}
			for _, t := range ts {
				if len(hosts[t.ID]) == 0 {
					continue
				}
				// The tile's ingress pick, else the default (the controller).
				ing, err := o.IngressFor(ctx, t.ID)
				if err != nil {
					return ClusterMap{}, err
				}
				j, ok := at[ing]
				if !ok {
					j = at[ctl]
				}
				in[j].Domains = append(in[j].Domains, hosts[t.ID]...)
			}
			// The exit: the first rung of the env's resolved ladder, else its own node.
			exit, err := o.exitNode(ctx, e, st.OrgID)
			if err != nil {
				return ClusterMap{}, err
			}
			if j, ok := at[exit]; ok {
				in[j].ExitEnvs = append(in[j].ExitEnvs, ce.Path)
			}
		}
		slices.SortStableFunc(in[i].Envs, func(a, b ClusterEnv) int { return healthRank(b.Health) - healthRank(a.Health) })
	}
	for i := range in {
		slices.Sort(in[i].Domains)
		in[i].Domains = slices.Compact(in[i].Domains)
		slices.Sort(in[i].ExitEnvs)
	}
	m := buildClusterMap(in)
	if m.Backups, err = o.clusterBackups(ctx, ctl); err != nil {
		return m, err
	}
	return m, nil
}

// healthRank orders the dots: what needs a look first.
func healthRank(h string) int {
	return map[string]int{HealthDown: 4, HealthDegraded: 3, HealthMoving: 2, HealthHealthy: 1, HealthStopped: 0}[h]
}

// pathNames reads each stack and org once.
type pathNames struct {
	o      *Orchestrator
	stacks map[string]store.Stack
	orgs   map[string]store.Org
}

func (p *pathNames) of(ctx context.Context, stackID string) (store.Org, store.Stack, error) {
	st, ok := p.stacks[stackID]
	if !ok {
		var err error
		if st, err = p.o.store.Stacks.Get(ctx, stackID); err != nil {
			return store.Org{}, st, err
		}
		p.stacks[stackID] = st
	}
	org, ok := p.orgs[st.OrgID]
	if !ok {
		var err error
		if org, err = p.o.store.Orgs.Get(ctx, st.OrgID); err != nil {
			return org, st, err
		}
		p.orgs[st.OrgID] = org
	}
	return org, st, nil
}

// hostsByTile is every domain host, by tile.
func (o *Orchestrator) hostsByTile(ctx context.Context) (map[string][]string, error) {
	ds, err := o.store.Domains.List(ctx)
	out := map[string][]string{}
	for _, d := range ds {
		out[d.TileID] = append(out[d.TileID], d.Host)
	}
	return out, err
}

// movingEnvs are the envs with a move in flight.
func (o *Orchestrator) movingEnvs(ctx context.Context) (map[string]bool, error) {
	ms, err := o.store.Moves.ListUnfinished(ctx)
	out := map[string]bool{}
	for _, m := range ms {
		out[m.EnvID] = true
	}
	return out, err
}

// envHealth is the env's dot. A node that does not answer has its envs down;
// a tile word the graph knows maps as the stack canvas's roll-up does.
func (o *Orchestrator) envHealth(ctx context.Context, e store.Environment, nodeUp, moving bool) string {
	if moving {
		return HealthMoving
	}
	if !nodeUp {
		return HealthDown
	}
	w, err := o.graph.EnvWord(ctx, e.ID)
	if err != nil {
		return HealthDown
	}
	switch w {
	case "error":
		return HealthDown
	case "unhealthy", "degraded", "waiting":
		return HealthDegraded
	case "running", "done", "building", "queued", "removing":
		return HealthHealthy
	}
	return HealthStopped
}

// exitNode is where the env's traffic leaves the internet.
func (o *Orchestrator) exitNode(ctx context.Context, e store.Environment, orgID string) (string, error) {
	ladder := func(kind, id string) ([]string, error) {
		rs, err := o.store.ExitLadders.List(ctx, kind, id)
		var out []string
		for _, r := range rs {
			out = append(out, r.NodeID)
		}
		return out, err
	}
	var lv exits.Levels
	var err error
	if lv.Env, err = ladder("env", e.ID); err != nil {
		return "", err
	}
	if lv.Stack, err = ladder("stack", e.StackID); err != nil {
		return "", err
	}
	if lv.Org, err = ladder("org", orgID); err != nil {
		return "", err
	}
	if r := exits.Resolve(lv, nil); len(r.Nodes) > 0 {
		return r.Nodes[0], nil
	}
	return e.NodeID, nil
}

// clusterBackups are the external destinations some schedule writes to, and
// the nodes the scheduled volumes live on (an env volume's node, else the
// controller's).
func (o *Orchestrator) clusterBackups(ctx context.Context, ctl string) ([]ClusterBackup, error) {
	ss, err := o.store.BackupSchedules.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []ClusterBackup
	at := map[string]int{}
	for _, s := range ss {
		if s.DestID == nil {
			continue
		}
		d, err := o.store.BackupDests.Get(ctx, *s.DestID)
		if err != nil {
			continue // a destination removed under the schedule
		}
		nodeID := ctl
		if v, err := o.store.Volumes.Get(ctx, s.VolumeID); err == nil && v.ScopeKind == "env" {
			if nodeID, err = o.envNode(ctx, v.ScopeID); err != nil {
				nodeID = ctl
			}
		}
		i, ok := at[d.ID]
		if !ok {
			i = len(out)
			at[d.ID] = i
			out = append(out, ClusterBackup{ID: d.ID, Name: d.Name})
		}
		if !slices.Contains(out[i].Nodes, nodeID) {
			out[i].Nodes = append(out[i].Nodes, nodeID)
		}
	}
	return out, nil
}

// factsFor is what a node reported last and when; a node that is slow keeps
// its last answer for a while, so one map load is not a round trip per node.
type cachedFacts struct {
	f   worker.Facts
	rtt time.Duration
	at  time.Time
}

// ponytail: process-wide, keyed by node id (uuids); per-orchestrator when
// two panels share a process.
var clusterFactsCache sync.Map

const clusterFactsTTL = 15 * time.Second

// clusterFacts reads a node's facts: the controller's own interfaces, a
// worker's over its extras client (timed: that is the link latency).
func (o *Orchestrator) clusterFacts(ctx context.Context, n store.Node) (*worker.Facts, bool, time.Duration) {
	if n.Role == node.Controller {
		return &worker.Facts{Interfaces: localIfaces()}, true, 0
	}
	if v, ok := clusterFactsCache.Load(n.ID); ok {
		if c := v.(cachedFacts); time.Since(c.at) < clusterFactsTTL {
			return &c.f, true, c.rtt
		}
	}
	stale := func() (*worker.Facts, bool, time.Duration) {
		if v, ok := clusterFactsCache.Load(n.ID); ok {
			c := v.(cachedFacts)
			return &c.f, false, 0
		}
		if o.mesh != nil { // what the mesh service last read from the node
			if f, ok := o.mesh.factsOf(n.ID); ok {
				return &f, false, 0
			}
		}
		return nil, false, 0
	}
	cl, err := o.nodeExtras(n.ID)
	if err != nil {
		return stale()
	}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	t0 := time.Now()
	f, err := cl.Facts(cctx)
	if err != nil {
		return stale()
	}
	c := cachedFacts{f: f, rtt: time.Since(t0), at: time.Now()}
	clusterFactsCache.Store(n.ID, c)
	return &c.f, true, c.rtt
}

// localIfaces is the controller's interfaces the way a worker reports them.
func localIfaces() []worker.Iface {
	var out []worker.Iface
	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 {
			continue
		}
		wi := worker.Iface{Name: i.Name}
		as, _ := i.Addrs()
		for _, a := range as {
			p, err := netip.ParsePrefix(a.String())
			if err != nil {
				continue
			}
			wi.Prefixes = append(wi.Prefixes, p.String())
			if ad := p.Addr().Unmap(); ad.IsGlobalUnicast() && !ad.IsPrivate() && !cgnat.Contains(ad) {
				wi.Public = true
			}
		}
		out = append(out, wi)
	}
	return out
}

// ClusterScope is where the Cluster canvas keeps its notes and boxes: the
// home scope under an id no user id can be, so no schema change. The cards
// are drawn from the facts and never move; only notes do.
var ClusterScope = CanvasScope{Kind: CanvasHome, ID: "cluster"}

// MoveClusterNote saves where a note on the Cluster canvas was dropped.
func (o *Orchestrator) MoveClusterNote(ctx context.Context, noteID string, p Point) error {
	return o.canvas.Move(ctx, ClusterScope, noteID, p)
}

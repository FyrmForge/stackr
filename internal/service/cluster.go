package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/FyrmForge/stackr/internal/exits"
	"github.com/FyrmForge/stackr/internal/mesh"
	"github.com/FyrmForge/stackr/internal/service/errs"
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
	// DefaultIngress and DefaultExit mark the cluster-wide defaults.
	DefaultIngress, DefaultExit bool
	Docker                      string   // the docker server version the node reported; "" = none
	Net                         string   // ClusterNet.ID
	Public                      []string // public addresses on its interfaces
	Envs                        []ClusterEnv
	// Domains are the hosts this node's ingress answers on: the tiles whose
	// ingress pick is this node, the rest on the controller.
	Domains []string
	// ExitEnvs are the env paths whose traffic leaves the internet here.
	ExitEnvs []string
	// NoForward: an ingress node behind a NAT that the probe did not reach.
	NoForward bool
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
	// Forward is the node the router forwards to: the one that answered the
	// port-forward probe from outside. Unsure: the controller probed its own
	// network (no public worker to ask), which a router may not loop back.
	Forward string
	Unsure  bool
}

// ClusterLink is one tunnel between a connected pair. MS < 0 = not measured:
// only the controller measures, and only to its own workers.
type ClusterLink struct {
	A, B, Kind string
	MS         float64
	Via        string // the relay node's name, "" = direct
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
// private address that is not the mesh's own (a /32 widened, Iface.LANs).
func netPrefix(f worker.Facts) (netip.Prefix, bool) {
	for _, i := range f.Interfaces {
		if excludedIface(i.Name) || i.Public {
			continue
		}
		for _, p := range i.LANs() {
			a := p.Addr().Unmap()
			if !a.Is4() || !a.IsPrivate() || mesh.Supernet.Contains(a) || mesh.Transit.Contains(a) {
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
	defIng, err := o.DefaultIngress(ctx)
	if err != nil {
		return ClusterMap{}, err
	}
	defExit := o.DefaultExit(ctx)
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
					j = at[defIng]
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
	for i := range m.Nodes {
		m.Nodes[i].DefaultIngress = m.Nodes[i].ID == defIng
		m.Nodes[i].DefaultExit = m.Nodes[i].ID == defExit
	}
	if o.mesh != nil {
		names := map[string]string{}
		for _, n := range m.Nodes {
			names[n.ID] = n.Name
		}
		for i, l := range m.Links {
			if h := o.mesh.RelayVia(l.A, l.B); h != "" {
				m.Links[i].Via = names[h]
			}
		}
	}
	o.probeForwards(ctx, ctl, &m)
	if m.Backups, err = o.clusterBackups(ctx, ctl); err != nil {
		return m, err
	}
	return m, nil
}

// probePath is the port-forward probe's path: secret to outsiders, the
// same on every node of the cluster.
func probePath(ctl string) string {
	sum := sha256.Sum256([]byte("stackr-probe:" + ctl))
	return worker.ProbePrefix + hex.EncodeToString(sum[:12])
}

// ponytail: process-wide like clusterFactsCache; probes run inline in the
// map's gather, so a slow router costs one map load per TTL.
var probeCache sync.Map

const probeTTL = time.Minute

type cachedProbe struct {
	who string // the node id that answered, "" = nobody
	at  time.Time
}

// probeForwards asks, for each ingress node behind a NAT, who answers its
// host from outside: a public worker in another network fetches
// https://<host><probe path> and the answering Caddy says its node id. With
// no public worker the controller fetches it itself and the answer is unsure.
func (o *Orchestrator) probeForwards(ctx context.Context, ctl string, m *ClusterMap) {
	panelHost, _ := o.settings.Get(ctx, "panel_domain")
	answers := map[string]string{}
	unsure := map[string]bool{}
	for _, c := range m.Networks {
		if !c.Router {
			continue
		}
		var vantage string
		for _, n := range m.Nodes {
			if n.Role == node.Worker && n.Status == NodeOnline && len(n.Public) > 0 && n.Net != c.ID {
				vantage = n.ID
				break
			}
		}
		unsure[c.ID] = vantage == ""
		for _, n := range m.Nodes {
			if n.Net != c.ID || !slices.Contains(n.Tags, "ingress") {
				continue
			}
			hosts := n.Domains
			if n.DefaultIngress && panelHost != "" {
				hosts = append([]string{panelHost}, hosts...)
			}
			i := slices.IndexFunc(hosts, func(h string) bool { return !strings.HasPrefix(h, "*.") })
			if i < 0 {
				continue // nothing to ask for: no claim either way
			}
			if who, ok := o.probe(ctx, vantage, "https://"+hosts[i]+probePath(ctl)); ok {
				answers[n.ID] = who
			}
		}
	}
	applyProbes(m, answers, unsure)
}

// probe fetches url from the vantage worker, or from here when it is "".
// ok is false when the vantage could not be asked: no answer either way.
func (o *Orchestrator) probe(ctx context.Context, vantage, url string) (string, bool) {
	key := vantage + " " + url
	if v, ok := probeCache.Load(key); ok && time.Since(v.(cachedProbe).at) < probeTTL {
		return v.(cachedProbe).who, true
	}
	cctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	var who string
	if vantage == "" {
		if req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil); err == nil {
			if resp, err := http.DefaultClient.Do(req); err == nil {
				b, _ := io.ReadAll(io.LimitReader(resp.Body, 128))
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					who = strings.TrimSpace(string(b))
				}
			}
		}
	} else {
		cl, err := o.nodeExtras(vantage)
		if err != nil {
			return "", false
		}
		r, err := cl.Probe(cctx, url)
		if err != nil {
			return "", false
		}
		if r.Status == http.StatusOK {
			who = r.Body
		}
	}
	probeCache.Store(key, cachedProbe{who, time.Now()})
	return who, true
}

// applyProbes is the decision half, pure: answers maps each probed ingress
// node to the node id that answered ("" = nobody). The network's Forward is
// a node of its own that answered; a probed node that is not the one
// answering is NoForward, unless the probe was unsure and nobody answered.
func applyProbes(m *ClusterMap, answers map[string]string, unsure map[string]bool) {
	net := map[string]int{}
	for i, c := range m.Networks {
		net[c.ID] = i
	}
	for i := range m.Nodes {
		n := &m.Nodes[i]
		who, probed := answers[n.ID]
		ci, ok := net[n.Net]
		if !probed || !ok {
			continue
		}
		c := &m.Networks[ci]
		if who != "" && slices.ContainsFunc(m.Nodes, func(x ClusterNode) bool { return x.ID == who && x.Net == c.ID }) {
			c.Forward, c.Unsure = who, unsure[c.ID]
		}
		n.NoForward = who != n.ID && (who != "" || !unsure[c.ID])
	}
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
	if r := exits.Resolve(lv, defaultExitIDs(ctx, o.store)); len(r.Nodes) > 0 {
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
		return &worker.Facts{Interfaces: worker.Interfaces()}, true, 0
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

// ClusterScope is where the Cluster canvas keeps its notes, boxes and
// dropped positions: the home scope under an id no user id can be, so no
// schema change.
var ClusterScope = CanvasScope{Kind: CanvasHome, ID: "cluster"}

// ClusterPositions is where admins dropped cards, by canvas id ("node:<id>",
// "nat:<net>", "s3:<id>", "internet"); the map's geometry reads them.
func (o *Orchestrator) ClusterPositions(ctx context.Context) (map[string]Point, error) {
	return o.canvas.Positions(ctx, ClusterScope)
}

// PlaceCluster saves where a note, node, NAT, backup or the internet card
// on the Cluster canvas was dropped. A network box is where its cards are.
func (o *Orchestrator) PlaceCluster(ctx context.Context, id string, p Point) error {
	if note, ok := strings.CutPrefix(id, "note:"); ok {
		return o.canvas.Move(ctx, ClusterScope, note, p)
	}
	movable := id == "internet"
	for _, p := range []string{"node:", "nat:", "s3:"} {
		movable = movable || strings.HasPrefix(id, p)
	}
	if !movable {
		return errs.Invalidf("node_id", "this card does not move")
	}
	return o.canvas.Save(ctx, ClusterScope, id, p)
}

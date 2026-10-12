package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/FyrmForge/stackr/internal/exits"
	"github.com/FyrmForge/stackr/internal/mesh"
	"github.com/FyrmForge/stackr/internal/relay"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/worker"
)

// The mesh service is the controller's half of the node mesh and the exits:
// it works out, for every node, the wg peers, the relay pairs and the exit
// links and ladders from the nodes and exit_ladders tables, and pushes each
// node its whole desired state (POST /v1/mesh) on every change and on a timer.
// The node applies only what differs from what it already runs.
//
// Wiring (orchestrator.go): a field `mesh *meshSvc`, then
//
//	o.mesh = newMeshSvc(o.store, nodeClients)
//	go o.mesh.Run(ctx)
//
// and o.mesh.Kick() after a node is added, removed or changes tags, an env is
// created, moved or deleted, or an exit ladder is set.

// MeshNode is how the mesh service reaches one node's worker:
// *workerclient.Client, or an in-process adapter for the controller's own node.
type MeshNode interface {
	Facts(ctx context.Context) (worker.Facts, error)
	MeshInfo(ctx context.Context) (worker.MeshInfo, error)
	ApplyMesh(ctx context.Context, spec worker.MeshSpec) error
}

// MeshNodes finds a node's worker; false when it has no client (yet).
type MeshNodes func(nodeID string) (MeshNode, bool)

// MeshRelay asks Host to relay the wg traffic of the pair A and B (node ids)
// on Port. Nothing sets these by default: a relay is for a pair that cannot
// reach each other directly.
type MeshRelay struct {
	Host, A, B string
	Port       int
}

const (
	// MeshWGPort is the wg listen port of every node's mesh interface.
	MeshWGPort = 51820

	exitPortBase  = 52000  // + pair index
	exitPrefBase  = 10000  // subject blocks, two prefs per source
	exitPrefServe = 20000  // + client slot, the exit side's fwmark rule
	exitTableLink = 1000   // + exit slot, client side
	exitTableServ = 1100   // + client slot, exit side
	exitMarkBase  = 0x5000 // + client slot
	maxSlots      = 16     // /16s in the supernet

	meshEvery      = time.Minute
	forgottenRange = "mesh.forgotten_ranges"
)

var (
	exitNet    = netip.MustParseAddr("10.251.0.0") // /30 per (client, exit) pair
	pingTarget = "1.1.1.1"                         // the own uplink's probe
)

type meshSvc struct {
	st    *store.Store
	nodes MeshNodes
	// Relays is read at each sync.
	Relays []MeshRelay
	// Registry names the controller's registry for a worker's spec; nil
	// (or a nil answer) = none. Never set on the controller's own spec.
	Registry func(ctx context.Context) *worker.RegistrySpec
	ctlID    string

	kick chan struct{}

	mu     sync.Mutex
	facts  map[string]worker.Facts
	exitPK map[string]string // exit public key per node
	wgPK   map[string]string // wg public key per node, for a row that holds none (the controller's)
	pushed map[string][32]byte
}

func newMeshSvc(st *store.Store, nodes MeshNodes) *meshSvc {
	return &meshSvc{st: st, nodes: nodes, kick: make(chan struct{}, 1),
		facts: map[string]worker.Facts{}, exitPK: map[string]string{}, wgPK: map[string]string{}, pushed: map[string][32]byte{}}
}

// Kick asks Run for a sync soon; it never blocks.
func (m *meshSvc) Kick() {
	select {
	case m.kick <- struct{}{}:
	default:
	}
}

// Run syncs at once, on every Kick and on a timer, until ctx ends. The timer
// pass pushes every node again, which repairs a node that lost its state.
func (m *meshSvc) Run(ctx context.Context) {
	t := time.NewTicker(meshEvery)
	defer t.Stop()
	force := true
	for {
		if err := m.Sync(ctx, "", force); err != nil && ctx.Err() == nil {
			slog.Warn("mesh: sync", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-m.kick:
			force = false
			time.Sleep(time.Second) // a burst of changes is one sync
		case <-t.C:
			force = true
		}
	}
}

// Ensure pushes one node now and returns when its exit ladders have run their
// first tick. Call it before env containers start on the node, so their
// traffic never leaves by the wrong uplink.
func (m *meshSvc) Ensure(ctx context.Context, nodeID string) error {
	return m.Sync(ctx, nodeID, false)
}

// Sync computes every node's spec and pushes it: all nodes when only is "",
// else that one. Unchanged specs are skipped unless force.
func (m *meshSvc) Sync(ctx context.Context, only string, force bool) error {
	in, err := m.gather(ctx)
	if err != nil {
		return err
	}
	specs := buildMesh(in)
	// Exit sides first, so a client's first probe finds its link answering.
	ids := make([]string, 0, len(specs))
	for id := range specs {
		ids = append(ids, id)
	}
	sort.SliceStable(ids, func(i, j int) bool {
		return len(specs[ids[i]].Serves) > 0 && len(specs[ids[j]].Serves) == 0
	})
	var errs []error
	for _, id := range ids {
		spec := specs[id]
		if only != "" && id != only {
			continue
		}
		cl, ok := m.nodes(id)
		if !ok {
			continue
		}
		if m.Registry != nil && id != m.ctlID {
			spec.Registry = m.Registry(ctx)
		}
		b, _ := json.Marshal(spec)
		sum := sha256.Sum256(b)
		m.mu.Lock()
		same := m.pushed[id] == sum
		m.mu.Unlock()
		if same && !force {
			continue
		}
		if err := cl.ApplyMesh(ctx, spec); err != nil {
			m.mu.Lock()
			delete(m.pushed, id)
			m.mu.Unlock()
			errs = append(errs, fmt.Errorf("node %s: %w", in.nameOf(id), err))
			continue
		}
		m.mu.Lock()
		m.pushed[id] = sum
		m.mu.Unlock()
	}
	return errors.Join(errs...)
}

// RemoveNode takes the node out of every exit ladder and tells it to drop its
// mesh and exits (best effort: it may be gone), then re-syncs the others.
// Call it before deleting the row. The node's range stays taken (ForgetRange).
func (m *meshSvc) RemoveNode(ctx context.Context, nodeID string) error {
	rungs, err := m.st.ExitLadders.ListByNode(ctx, nodeID)
	if err != nil {
		return err
	}
	scopes := map[[2]string]bool{}
	for _, r := range rungs {
		scopes[[2]string{r.ScopeKind, r.ScopeID}] = true
	}
	for sc := range scopes {
		cur, err := m.st.ExitLadders.List(ctx, sc[0], sc[1])
		if err != nil {
			return err
		}
		var keep []string
		for _, r := range cur {
			if r.NodeID != nodeID {
				keep = append(keep, r.NodeID)
			}
		}
		if err := m.st.ExitLadders.Set(ctx, sc[0], sc[1], keep); err != nil {
			return err
		}
	}
	if cl, ok := m.nodes(nodeID); ok {
		if err := cl.ApplyMesh(ctx, worker.MeshSpec{}); err != nil {
			slog.Warn("mesh: tear down removed node", "node", nodeID, "err", err)
		}
	}
	m.mu.Lock()
	delete(m.pushed, nodeID)
	delete(m.facts, nodeID)
	delete(m.exitPK, nodeID)
	delete(m.wgPK, nodeID)
	m.mu.Unlock()
	return m.Sync(ctx, "", false)
}

// AllocateNodeRange is the next free /16 of the supernet: not any node's
// range, not a forgotten one, not on any node's interfaces.
func (m *meshSvc) AllocateNodeRange(ctx context.Context) (netip.Prefix, error) {
	ns, err := m.st.Nodes.List(ctx)
	if err != nil {
		return netip.Prefix{}, err
	}
	taken, err := m.forgotten(ctx)
	if err != nil {
		return netip.Prefix{}, err
	}
	var ifaces []netip.Prefix
	m.mu.Lock()
	for _, f := range m.facts {
		ifaces = append(ifaces, ifacePrefixes(f)...)
	}
	m.mu.Unlock()
	for _, n := range ns {
		if p, err := netip.ParsePrefix(n.RangeCIDR); err == nil {
			taken = append(taken, p)
		}
	}
	return mesh.AllocateRange(taken, ifaces)
}

// reserved is every prefix a new node range must avoid besides the node rows:
// the forgotten ranges and the interfaces the nodes reported.
func (m *meshSvc) reserved(ctx context.Context) ([]netip.Prefix, error) {
	out, err := m.forgotten(ctx)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, f := range m.facts {
		out = append(out, ifacePrefixes(f)...)
	}
	return out, nil
}

// ForgetRange keeps a removed node's range taken for good.
func (m *meshSvc) ForgetRange(ctx context.Context, cidr string) error {
	cur, _, err := m.st.Settings.Get(ctx, forgottenRange)
	if err != nil {
		return err
	}
	if slices.Contains(strings.Split(cur, ","), cidr) {
		return nil
	}
	if cur != "" {
		cur += ","
	}
	return m.st.Settings.Set(ctx, forgottenRange, cur+cidr)
}

func (m *meshSvc) forgotten(ctx context.Context) ([]netip.Prefix, error) {
	cur, _, err := m.st.Settings.Get(ctx, forgottenRange)
	if err != nil {
		return nil, err
	}
	var out []netip.Prefix
	for _, s := range strings.Split(cur, ",") {
		if p, err := netip.ParsePrefix(s); err == nil {
			out = append(out, p)
		}
	}
	return out, nil
}

// meshIn is everything buildMesh needs; gather reads it from the database and
// the nodes.
type meshIn struct {
	Nodes  []meshNodeIn
	Relays []MeshRelay
}

type meshNodeIn struct {
	mesh.Node
	Name    string
	ExitKey string
	// Envs are the exit ladders of the envs on this node, one per env with a
	// subnet; Ladder is the resolved ordered node ids (empty = own uplink).
	Envs []meshEnvIn
}

type meshEnvIn struct {
	Subnet string
	Ladder []string
}

func (in meshIn) nameOf(id string) string {
	for _, n := range in.Nodes {
		if n.ID == id {
			return n.Name
		}
	}
	return id
}

func (m *meshSvc) gather(ctx context.Context) (meshIn, error) {
	ns, err := m.st.Nodes.List(ctx)
	if err != nil {
		return meshIn{}, err
	}
	in := meshIn{Relays: m.Relays}
	stackOrg := map[string]string{}
	ladder := func(kind, id string) ([]string, error) {
		rs, err := m.st.ExitLadders.List(ctx, kind, id)
		var out []string
		for _, r := range rs {
			out = append(out, r.NodeID)
		}
		return out, err
	}
	for _, n := range ns {
		if n.Status == node.StatusPending { // no range yet ("pending:<id>")
			continue
		}
		mn := meshNodeIn{Name: n.Name, Node: mesh.Node{ID: n.ID, PublicKey: n.WGPublicKey, Port: MeshWGPort}}
		mn.Range, _ = netip.ParsePrefix(n.RangeCIDR)
		if cl, ok := m.nodes(n.ID); ok {
			m.refresh(ctx, n.ID, cl)
		}
		m.mu.Lock()
		f, haveF := m.facts[n.ID]
		mn.ExitKey = m.exitPK[n.ID]
		if mn.PublicKey == "" {
			mn.PublicKey = m.wgPK[n.ID]
		}
		m.mu.Unlock()
		if haveF {
			mn.Ifaces = ifacePrefixes(f)
			mn.Public = publicAddrs(f)
		}
		envs, err := m.st.Environments.ListByNode(ctx, n.ID)
		if err != nil {
			return meshIn{}, err
		}
		for _, e := range envs {
			if e.Subnet == "" {
				continue
			}
			var lv exits.Levels
			if lv.Env, err = ladder("env", e.ID); err != nil {
				return meshIn{}, err
			}
			if lv.Stack, err = ladder("stack", e.StackID); err != nil {
				return meshIn{}, err
			}
			org, ok := stackOrg[e.StackID]
			if !ok {
				st, err := m.st.Stacks.Get(ctx, e.StackID)
				if err != nil {
					return meshIn{}, err
				}
				org = st.OrgID
				stackOrg[e.StackID] = org
			}
			if lv.Org, err = ladder("org", org); err != nil {
				return meshIn{}, err
			}
			// No default exit set = the own uplink: no ladder.
			res := exits.Resolve(lv, defaultExitIDs(ctx, m.st))
			// OwnRung ("own node" in the placement UI) is this env's node.
			own := make([]string, 0, len(res.Nodes))
			for _, id := range res.Nodes {
				if id == OwnRung {
					id = n.ID
				}
				if !slices.Contains(own, id) {
					own = append(own, id)
				}
			}
			res.Nodes = own
			if res.Routed(n.ID) {
				mn.Envs = append(mn.Envs, meshEnvIn{Subnet: e.Subnet, Ladder: res.Nodes})
			}
		}
		in.Nodes = append(in.Nodes, mn)
	}
	return in, nil
}

// refresh reads a node's facts and exit public key; a node that does not
// answer keeps what was known.
func (m *meshSvc) refresh(ctx context.Context, id string, cl MeshNode) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if f, err := cl.Facts(cctx); err == nil {
		m.mu.Lock()
		m.facts[id] = f
		m.mu.Unlock()
	}
	m.mu.Lock()
	have := m.exitPK[id] != ""
	m.mu.Unlock()
	if have {
		return
	}
	if info, err := cl.MeshInfo(cctx); err == nil {
		m.mu.Lock()
		m.wgPK[id] = info.WGPublic
		m.exitPK[id] = info.ExitPublic
		m.mu.Unlock()
	}
}

func ifacePrefixes(f worker.Facts) []netip.Prefix {
	var out []netip.Prefix
	for _, i := range f.Interfaces {
		out = append(out, i.LANs()...)
	}
	return out
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

func publicAddrs(f worker.Facts) []netip.Addr {
	var out []netip.Addr
	for _, i := range f.Interfaces {
		if !i.Public {
			continue
		}
		for _, s := range i.Prefixes {
			if p, err := netip.ParsePrefix(s); err == nil {
				a := p.Addr().Unmap()
				if a.IsGlobalUnicast() && !a.IsPrivate() && !cgnat.Contains(a) {
					out = append(out, a)
				}
			}
		}
	}
	return out
}

// rangeSlot is the node's index in the supernet: 0 for 10.208/16 ... 15.
func rangeSlot(r netip.Prefix) int {
	return int(r.Addr().As4()[1]) - int(mesh.Supernet.Addr().As4()[1])
}

// pairBlock is the /30 of the exit link between client and exit slots; the
// client is .1 and the exit .2. Slots never change (ranges are never reused),
// so neither do the addresses, ports, tables and marks derived from them.
func pairBlock(client, exit int) (idx int, cAddr, sAddr netip.Addr) {
	idx = client*maxSlots + exit
	b := exitNet.As4()
	b[2], b[3] = byte(idx*4/256), byte(idx*4%256)
	b[3]++
	cAddr = netip.AddrFrom4(b)
	b[3]++
	sAddr = netip.AddrFrom4(b)
	return idx, cAddr, sAddr
}

// buildMesh is the whole plan: one spec per node. Pure, so it is tested with
// plain values.
func buildMesh(in meshIn) map[string]worker.MeshSpec {
	byID := map[string]meshNodeIn{}
	var all []mesh.Node
	for _, n := range in.Nodes {
		byID[n.ID] = n
		all = append(all, n.Node)
	}
	usable := func(n meshNodeIn) bool { return n.Range.IsValid() && n.Range.Bits() == mesh.NodePrefixLen }

	// Relay: the host's first public address and port stand in as the
	// endpoint for the pair; the pair itself runs on the host.
	relayAt := func(host string, port int) netip.AddrPort {
		if h, ok := byID[host]; ok && len(h.Public) > 0 {
			return netip.AddrPortFrom(h.Public[0], uint16(port))
		}
		return netip.AddrPort{}
	}
	relayFor := func(a, b string) netip.AddrPort {
		for _, r := range in.Relays {
			if (r.A == a && r.B == b) || (r.A == b && r.B == a) {
				return relayAt(r.Host, r.Port)
			}
		}
		return netip.AddrPort{}
	}

	// Per client node: its links by exit node and the sources routed over each.
	type linkPlan struct{ sources []string }
	clientLinks := map[string]map[string]*linkPlan{}
	specs := map[string]worker.MeshSpec{}

	for _, n := range in.Nodes {
		if !usable(n) {
			continue
		}
		spec := worker.MeshSpec{ListenPort: int(n.Port)}
		if t, err := mesh.TransitAddr(n.Range); err == nil {
			spec.Address = netip.PrefixFrom(t, mesh.Transit.Bits())
		}
		spec.Peers, _ = mesh.Peers(n.Node, all, relayFor)
		for _, r := range in.Relays {
			if r.Host != n.ID {
				continue
			}
			a, b := byID[r.A], byID[r.B]
			if len(a.Public) > 0 && len(b.Public) > 0 {
				spec.Relays = append(spec.Relays, relay.Pair{Port: r.Port, A: a.Public[0], B: b.Public[0]})
			}
		}

		// Subjects: envs of this node that share a ladder.
		groups := map[string][]string{}
		for _, e := range n.Envs {
			var l []string
			for _, id := range e.Ladder {
				if x, ok := byID[id]; id == n.ID || ok && usable(x) {
					l = append(l, id)
				}
			}
			if len(l) == 0 {
				continue
			}
			k := strings.Join(l, ",")
			groups[k] = append(groups[k], e.Subnet)
		}
		keys := make([]string, 0, len(groups))
		for k := range groups {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		clientLinks[n.ID] = map[string]*linkPlan{}
		pref := exitPrefBase
		self := rangeSlot(n.Range)
		for _, k := range keys {
			srcs := groups[k]
			sort.Strings(srcs)
			ids := strings.Split(k, ",")
			ls := worker.LadderSpec{Subject: exits.Subject{Sources: srcs, PrefBase: pref}}
			pref += 2 * len(srcs)
			for _, id := range ids {
				if id == n.ID {
					ls.Rungs = append(ls.Rungs, exits.Rung{NodeID: id, Own: true, Target: pingTarget})
					continue
				}
				x := byID[id]
				_, _, sAddr := pairBlock(self, rangeSlot(x.Range))
				ls.Rungs = append(ls.Rungs, exits.Rung{NodeID: id, Iface: exits.Iface(id),
					Table: exitTableLink + rangeSlot(x.Range), Target: sAddr.String()})
				lp := clientLinks[n.ID][id]
				if lp == nil {
					lp = &linkPlan{}
					clientLinks[n.ID][id] = lp
				}
				lp.sources = append(lp.sources, srcs...)
			}
			spec.Ladders = append(spec.Ladders, ls)
		}
		specs[n.ID] = spec
	}

	// Links on the client side, serves on the exit side, from the same table.
	for cid, links := range clientLinks {
		c := byID[cid]
		cs := rangeSlot(c.Range)
		spec := specs[cid]
		ids := make([]string, 0, len(links))
		for id := range links {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, xid := range ids {
			x := byID[xid]
			if c.ExitKey == "" || x.ExitKey == "" {
				continue // the node has not told us its exit key yet
			}
			xs := rangeSlot(x.Range)
			idx, cAddr, sAddr := pairBlock(cs, xs)
			srcs := uniq(links[xid].sources)
			xt, _ := mesh.TransitAddr(x.Range)
			spec.Links = append(spec.Links, exits.Link{
				Iface: exits.Iface(xid), Port: exitPortBase + idx, PeerKey: x.ExitKey,
				Endpoint: netip.AddrPortFrom(xt, uint16(exitPortBase+idx)).String(),
				Addr:     netip.PrefixFrom(cAddr, 30).String(),
				Table:    exitTableLink + xs, Sources: srcs,
			})
			ex := specs[xid]
			ex.Serves = append(ex.Serves, exits.Serve{
				Iface: exits.Iface(cid), Port: exitPortBase + idx, PeerKey: c.ExitKey,
				Addr:     netip.PrefixFrom(sAddr, 30).String(),
				PeerAddr: netip.PrefixFrom(cAddr, 32).String(),
				Subnets:  srcs,
				Table:    exitTableServ + cs, Mark: exitMarkBase + cs, Pref: exitPrefServe + cs,
			})
			specs[xid] = ex
		}
		specs[cid] = spec
	}
	for id, sp := range specs {
		sort.Slice(sp.Serves, func(i, j int) bool { return sp.Serves[i].Iface < sp.Serves[j].Iface })
		specs[id] = sp
	}
	return specs
}

func uniq(in []string) []string {
	s := append([]string(nil), in...)
	sort.Strings(s)
	return slices.Compact(s)
}

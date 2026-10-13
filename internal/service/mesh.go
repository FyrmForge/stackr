package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/netip"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/FyrmForge/stackr/internal/exits"
	"github.com/FyrmForge/stackr/internal/mesh"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/worker"
)

// The mesh service is the controller's half of the node mesh and the exits:
// it works out, for every node, the wg peers (a pair with no direct tunnel
// routed through a node both reach) and the exit links and ladders from the nodes and exit_ladders tables, and pushes each
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

const (
	// MeshWGPort is a node's mesh listen port until its facts name its own.
	MeshWGPort = mesh.DefaultPort

	exitPortBase  = 52000  // + pair index
	exitPrefBase  = 10000  // subject blocks, two prefs per source
	exitPrefServe = 20000  // + client slot, the exit side's fwmark rule
	exitTableLink = 1000   // + exit slot, client side
	exitTableServ = 1100   // + client slot, exit side
	exitMarkBase  = 0x5000 // + client slot
	maxSlots      = 16     // /16s in the supernet

	meshEvery = time.Minute
	// sampleEvery is how often facts are read between full pushes, so a
	// dead tunnel shows within a minute.
	sampleEvery = 20 * time.Second
	// factsFresh is how old a node's facts may be and still tell whether
	// its tunnels are up: two missed syncs and they say nothing.
	factsFresh = 2 * meshEvery
	// rxSilent is how long a peer may send nothing before its tunnel counts
	// as dead: two missed 25 s keepalives.
	rxSilent = 60 * time.Second
	// downConfirm is how long a pair must look dead before it is routed
	// through another node: one more sample, so a blip does not reroute.
	downConfirm    = sampleEvery
	routesKey      = "mesh.routes"
	forgottenRange = "mesh.forgotten_ranges"
)

var (
	exitNet    = mesh.ExitNet.Addr() // /30 per (client, exit) pair
	pingTarget = "1.1.1.1"           // the own uplink's probe
)

type meshSvc struct {
	st    *store.Store
	nodes MeshNodes
	// syncMu runs one plan-and-push at a time: a sync planned with a key
	// that a controller rotation is replacing must not push after it.
	syncMu sync.Mutex
	// Registry names the controller's registry for a worker's spec; nil
	// (or a nil answer) = none. Never set on the controller's own spec.
	Registry func(ctx context.Context) *worker.RegistrySpec
	ctlID    string
	// Panel is the panel's public URL, named in every worker's spec.
	Panel string

	kick chan struct{}

	mu     sync.Mutex
	facts  map[string]worker.Facts
	at     map[string]time.Time // when facts last came in, per node
	exitPK map[string]string    // exit public key per node
	wgPK   map[string]string    // wg public key per node, for a row that holds none (the controller's)
	pushed map[string][32]byte
	next   map[[2]string]string           // {src, dst}: src's next hop to dst, from the last sync
	down   map[[2]string]time.Time        // since when a pair looked down
	rx     map[string]map[string]rxSample // per node, per peer key
	specs  map[string]worker.MeshSpec
	// heard is the last rotated key and heardBy when the rotation gate stops
	// waiting to hear it (rotate.go).
	heard   string
	heardBy time.Time
}

// Spec is the node's mesh spec from the last sync: a joining worker gets
// its first one in the join answer, so its tunnel comes up without the
// controller dialling it outside the mesh.
func (m *meshSvc) Spec(nodeID string) (worker.MeshSpec, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.specs[nodeID]
	return s, ok
}

func newMeshSvc(st *store.Store, nodes MeshNodes) *meshSvc {
	return &meshSvc{st: st, nodes: nodes, kick: make(chan struct{}, 1),
		facts: map[string]worker.Facts{}, at: map[string]time.Time{}, down: map[[2]string]time.Time{}, rx: map[string]map[string]rxSample{}, exitPK: map[string]string{}, wgPK: map[string]string{}, pushed: map[string][32]byte{}}
}

// Via is a's first hop towards b, "" when they connect directly.
func (m *meshSvc) Via(a, b string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.next[[2]string{a, b}]
}

// Report takes facts a node sent itself (a recovery call through the panel)
// as if the controller had read them over the tunnel.
func (m *meshSvc) Report(ctx context.Context, id string, f worker.Facts) {
	m.mu.Lock()
	m.facts[id], m.at[id] = f, time.Now()
	m.sampleRx(id, f, m.at[id])
	m.mu.Unlock()
	m.keepPublic(ctx, id, publicAddrs(f))
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
	m.loadRoutes(ctx) // the first push keeps the routes from before a restart
	t := time.NewTicker(sampleEvery)
	defer t.Stop()
	force, last := true, time.Time{}
	for {
		if force {
			last = time.Now()
		}
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
			force = time.Since(last) >= meshEvery
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
	m.syncMu.Lock()
	defer m.syncMu.Unlock()
	in, specs, err := m.plan(ctx)
	if err != nil {
		return err
	}
	// Exit sides first, so a client's first probe finds its link answering.
	ids := make([]string, 0, len(specs))
	for id := range specs {
		if only == "" || id == only {
			ids = append(ids, id)
		}
	}
	sort.SliceStable(ids, func(i, j int) bool {
		return len(specs[ids[i]].Serves) > 0 && len(specs[ids[j]].Serves) == 0
	})
	return m.push(ctx, in, specs, ids, force)
}

// plan gathers the nodes, decides the routes and builds every node's spec,
// keeping them as the last sync's.
func (m *meshSvc) plan(ctx context.Context) (meshIn, map[string]worker.MeshSpec, error) {
	in, err := m.gather(ctx)
	if err != nil {
		return meshIn{}, nil, err
	}
	in.Next = m.routes(in, time.Now())
	specs := buildMesh(in)
	// A worker's spec names the registry and the panel's public URL (where
	// it recovers); the controller's own names neither.
	var reg *worker.RegistrySpec
	if m.Registry != nil {
		reg = m.Registry(ctx)
	}
	for id, sp := range specs {
		if id != m.ctlID {
			sp.Registry, sp.Panel = reg, m.Panel
			specs[id] = sp
		}
	}
	m.mu.Lock()
	changed := !maps.Equal(m.next, in.Next)
	m.next, m.specs = in.Next, specs
	m.mu.Unlock()
	if changed {
		m.saveRoutes(ctx, in.Next)
	}
	return in, specs, nil
}

// push sends ids their specs, in that order; unchanged ones are skipped
// unless force.
func (m *meshSvc) push(ctx context.Context, in meshIn, specs map[string]worker.MeshSpec, ids []string, force bool) error {
	var errs []error
	for _, id := range ids {
		spec := specs[id]
		cl, ok := m.nodes(id)
		if !ok {
			continue
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
	_ = m.st.Settings.Delete(ctx, publicKey(nodeID))
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
	Nodes []meshNodeIn
	// Next is, keyed {src, dst}, src's next hop to a dst it has no working
	// tunnel to.
	Next map[[2]string]string
}

type meshNodeIn struct {
	mesh.Node
	Name    string
	ExitKey string
	// Fresh is set when the node's facts are recent enough to say which of
	// its tunnels are up; Hears is, per peer public key, 1 when the tunnel
	// carries traffic (or a fresh handshake), -1 when the peer has sent
	// nothing for rxSilent; absent = can't tell.
	Fresh bool
	Hears map[string]int
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
	in := meshIn{}
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
		mn.Fresh = haveF && time.Since(m.at[n.ID]) < factsFresh
		mn.ExitKey = m.exitPK[n.ID]
		if mn.PublicKey == "" {
			mn.PublicKey = m.wgPK[n.ID]
		}
		m.mu.Unlock()
		if !haveF {
			// Not reached yet (a node that just joined): what it reported
			// at join, so peers can dial it before its tunnel is up.
			if jt, err := m.st.JoinTokens.GetByNode(ctx, n.ID); err == nil && jt.Facts != "" {
				haveF = json.Unmarshal([]byte(jt.Facts), &f) == nil
			}
		}
		if mn.Fresh {
			mn.Hears = map[string]int{}
			for k := range f.Seen {
				mn.Hears[k] = 1
			}
			m.mu.Lock()
			for k, smp := range m.rx[n.ID] {
				if ev := smp.ev(time.Now()); ev != 0 {
					mn.Hears[k] = ev // bytes beat a handshake up to 3 min old
				}
			}
			m.mu.Unlock()
		}
		if haveF {
			mn.Ifaces = ifacePrefixes(f)
			mn.Public = publicAddrs(f)
			if f.WGPort > 0 && f.WGPort < 65536 {
				mn.Port = uint16(f.WGPort)
			}
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
		m.facts[id], m.at[id] = f, time.Now()
		m.sampleRx(id, f, m.at[id])
		m.mu.Unlock()
		m.keepPublic(ctx, id, publicAddrs(f))
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

// publicOf is a node's public addresses from its last known facts.
func (m *meshSvc) publicOf(id string) []netip.Addr {
	m.mu.Lock()
	f, ok := m.facts[id]
	m.mu.Unlock()
	if ok {
		return publicAddrs(f)
	}
	// Not reached since the panel started (its tunnel may be down): the
	// addresses it last reported.
	v, _, _ := m.st.Settings.Get(context.Background(), publicKey(id))
	var out []netip.Addr
	for _, s := range strings.Fields(v) {
		if a, err := netip.ParseAddr(s); err == nil {
			out = append(out, a)
		}
	}
	return out
}

// publicKey is the setting holding a node's last reported public addresses,
// kept so the liveness check has somewhere to dial after a panel restart.
func publicKey(id string) string { return "node.public." + id }

// keepPublic stores a node's public addresses when they changed.
func (m *meshSvc) keepPublic(ctx context.Context, id string, as []netip.Addr) {
	var ss []string
	for _, a := range as {
		ss = append(ss, a.String())
	}
	v := strings.Join(ss, " ")
	if old, _, err := m.st.Settings.Get(ctx, publicKey(id)); err == nil && old != v {
		if err := m.st.Settings.Set(ctx, publicKey(id), v); err != nil {
			slog.Warn("mesh: keep public addresses", "node", id, "err", err)
		}
	}
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

// routes decides how nodes without a working tunnel reach each other. A
// pair is up when either side hears the other (Hears 1); one that a side
// reports silent (Hears -1) for downConfirm is down. A pair nobody can
// judge keeps its last state, so stale or missing facts never drop a route. Routes are
// shortest paths over the up pairs (mesh.Routes): any number of hops,
// preferring public nodes.
func (m *meshSvc) routes(in meshIn, now time.Time) map[[2]string]string {
	byID := map[string]meshNodeIn{}
	var ids []string
	for _, n := range in.Nodes {
		if n.Range.IsValid() && n.Range.Bits() == mesh.NodePrefixLen && n.PublicKey != "" {
			byID[n.ID] = n
			ids = append(ids, n.ID)
		}
	}
	sort.Strings(ids)
	hears := func(a, b meshNodeIn) int {
		if !a.Fresh {
			return 0
		}
		return a.Hears[b.PublicKey]
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	prev := m.next
	up := map[[2]string]bool{}
	for i, a := range ids {
		for _, b := range ids[i+1:] {
			na, nb := byID[a], byID[b]
			k := mesh.PairKey(a, b)
			ea, eb := hears(na, nb), hears(nb, na)
			switch {
			case ea > 0 || eb > 0:
				up[k] = true
				delete(m.down, k)
			case ea < 0 || eb < 0:
				if _, ok := m.down[k]; !ok {
					m.down[k] = now
				}
				// Until confirmed, as last time: a routed pair whose direct
				// path is still dead must not lose its route meanwhile.
				up[k] = now.Sub(m.down[k]) < downConfirm && prev[[2]string{a, b}] == ""
			default:
				up[k] = prev[[2]string{a, b}] == "" // unknown: as last time
			}
		}
	}
	pub := func(id string) bool { return len(mesh.FilterAddrs(byID[id].Public)) > 0 }
	// ponytail: hop count, not latency; weigh tunnels by latency once
	// clusters have paths of equal length that differ much.
	return mesh.Routes(ids, func(a, b string) bool { return up[mesh.PairKey(a, b)] }, pub)
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

	next := func(src, dst string) string { return in.Next[[2]string{src, dst}] }

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
		spec.Peers, _ = mesh.Peers(n.Node, all, next)

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

// rxSample is what the controller knows of the bytes a node received from
// one peer: the last count, when it was first read and when it last moved.
type rxSample struct {
	n              int64
	first, changed time.Time
}

// ev is the sample's verdict: 1 when bytes moved within rxSilent, -1 when
// they have not for rxSilent, 0 when there is not enough history yet.
func (s rxSample) ev(now time.Time) int {
	switch {
	case !s.changed.IsZero() && now.Sub(s.changed) < rxSilent:
		return 1
	}
	last := s.first
	if s.changed.After(last) {
		last = s.changed
	}
	if now.Sub(last) >= rxSilent {
		return -1
	}
	return 0
}

// sampleRx records a node's per-peer received bytes. m.mu is held.
func (m *meshSvc) sampleRx(id string, f worker.Facts, now time.Time) {
	if m.rx == nil {
		m.rx = map[string]map[string]rxSample{}
	}
	seen := m.rx[id]
	if seen == nil {
		seen = map[string]rxSample{}
		m.rx[id] = seen
	}
	for k, n := range f.Rx {
		s, ok := seen[k]
		switch {
		case !ok:
			s = rxSample{n: n, first: now}
		case n != s.n:
			s.n, s.changed = n, now
		}
		seen[k] = s
	}
}

// saveRoutes keeps the routing decision, so a restarted controller pushes
// the same routes before it has heard from any node.
func (m *meshSvc) saveRoutes(ctx context.Context, next map[[2]string]string) {
	rows := make([][3]string, 0, len(next))
	for k, v := range next {
		rows = append(rows, [3]string{k[0], k[1], v})
	}
	slices.SortFunc(rows, func(a, b [3]string) int { return strings.Compare(a[0]+a[1], b[0]+b[1]) })
	b, _ := json.Marshal(rows)
	if err := m.st.Settings.Set(ctx, routesKey, string(b)); err != nil {
		slog.Warn("mesh: keep routes", "err", err)
	}
}

// loadRoutes reads the routing decision saved before a restart.
func (m *meshSvc) loadRoutes(ctx context.Context) {
	v, ok, err := m.st.Settings.Get(ctx, routesKey)
	if err != nil || !ok || v == "" {
		return
	}
	var rows [][3]string
	if json.Unmarshal([]byte(v), &rows) != nil {
		return
	}
	next := map[[2]string]string{}
	for _, r := range rows {
		next[[2]string{r[0], r[1]}] = r[2]
	}
	m.mu.Lock()
	m.next = next
	m.mu.Unlock()
}

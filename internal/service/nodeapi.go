package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/backup"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/securitylog"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/worker"
)

// The API and CLI face of the node and placement verbs. Names in, names out:
// a node (admin verbs) is found by name, id or a unique id prefix and answered
// by name; pins, ingress picks and exit ladders take and answer placement
// names, never node names (D3). Nothing here owns data: tags, removal and
// forget are nodeadmin.go, pins, ingress picks (ingress_picks) and exit
// ladders are placement.go; these are thin wrappers that translate and shape
// the answer.

const (
	// StateNeedsRejoin marks a worker whose join command was re-issued; it
	// goes pending again when the new request comes in.
	StateNeedsRejoin = "needs-rejoin"
	auditNodeRejoin  = "node.rejoin"
	factsWait        = 2 * time.Second
)

// NodeOut is one node as `node ls` shows it. The system numbers come from
// the worker's facts and are zero when it has not reported (or is the
// controller).
type NodeOut struct {
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	Role  string   `json:"role"`
	State string   `json:"state"`
	Tags  []string `json:"tags"`
	Envs  int      `json:"envs"`
	// Placements are the names of the placements standing on the node.
	Placements []string `json:"placements"`
	Range      string   `json:"range"`
	WGAddress  string   `json:"wg_address"`
	// PublicIP is the address DNS records point at, "" = the detected one.
	PublicIP string `json:"public_ip"`
	// Address is the node's own DNS name, "" = none.
	Address   string `json:"address"`
	CPU       int    `json:"cpu"`
	MemTotal  uint64 `json:"mem_total"`
	DiskFree  uint64 `json:"disk_free"`
	DiskTotal uint64 `json:"disk_total"`
	Docker    string `json:"docker"`
	Arch      string `json:"arch"`
	// Version is the stackr build the node reported; Behind is a worker
	// whose build is not the panel's, TooOld one the panel refuses to drive.
	Version string `json:"version"`
	Behind  bool   `json:"behind"`
	TooOld  bool   `json:"too_old"`
	// Warnings are what is off on the node that stops nothing.
	Warnings []NodeWarning `json:"warnings"`
	// DefaultExit is the exit node names of the node's own ladder; empty
	// for an egress node.
	DefaultExit []string `json:"default_exit"`
}

// NodeVolume is a volume of an env on a node and when it was last backed up.
type NodeVolume struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	LastBackup *time.Time `json:"last_backup"`
	RunID      string     `json:"run_id,omitempty"`
}

// NodeEnvOut is an env that runs on (or ran on) a node, with its volumes.
type NodeEnvOut struct {
	ID        string       `json:"id"`
	Path      string       `json:"path"` // org/stack/env
	Org       string       `json:"org"`
	Placement string       `json:"placement"`
	Volumes   []NodeVolume `json:"volumes"`
}

// NodeCertOut is the node's live certificate.
type NodeCertOut struct {
	Serial   string    `json:"serial"`
	NotAfter time.Time `json:"not_after"`
	RenewsOn time.Time `json:"renews_on"`
}

// NodeInfo is `node show`.
type NodeInfo struct {
	NodeOut
	Facts      *worker.Facts `json:"facts,omitempty"`
	Cert       *NodeCertOut  `json:"certificate,omitempty"`
	EnvList    []NodeEnvOut  `json:"env_list"`
	ExitFor    []string      `json:"exit_for"` // scopes that list this node as an exit
	IngressFor []string      `json:"ingress_for"`
}

// nodeByRef finds a node by name, id or a unique id prefix.
func (o *Orchestrator) nodeByRef(ctx context.Context, ref string) (store.Node, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return store.Node{}, errs.ErrNotFound
	}
	ns, err := o.store.Nodes.List(ctx)
	if err != nil {
		return store.Node{}, err
	}
	var hit []store.Node
	for _, n := range ns {
		if n.Slug == ref || n.Name == ref || n.ID == ref {
			return n, nil
		}
		if len(ref) >= 4 && strings.HasPrefix(n.ID, ref) {
			hit = append(hit, n)
		}
	}
	if len(hit) == 1 {
		return hit[0], nil
	}
	return store.Node{}, errs.ErrNotFound
}

func (o *Orchestrator) nodeName(ctx context.Context, id string) string {
	if n, err := o.store.Nodes.Get(ctx, id); err == nil {
		return n.Name
	}
	return id
}

// placementOrg is the org id a scope sits under.
func (o *Orchestrator) placementOrg(ctx context.Context, kind, id string) (string, error) {
	_, og, err := o.chain(ctx, kind, id)
	return og.ID, err
}

// ScopePath words a scope for a list a person reads (the security log):
// acme/shop/prod/web, or "a removed env" once it is gone.
func (o *Orchestrator) ScopePath(ctx context.Context, kind, id string) string {
	if kind != scopeNode {
		if _, _, err := o.chain(ctx, kind, id); err != nil {
			return "a removed " + kind
		}
	}
	return o.scopePath(ctx, kind, id)
}

// scopePath words a scope: acme/shop/prod/web.
func (o *Orchestrator) scopePath(ctx context.Context, kind, id string) string {
	if kind == scopeNode {
		if n, err := o.store.Nodes.Get(ctx, id); err == nil {
			return "node " + n.Name
		}
		return kind + " " + id
	}
	ch, og, err := o.chain(ctx, kind, id)
	if err != nil {
		return kind + " " + id
	}
	parts := []string{og.Slug}
	for i := len(ch) - 2; i >= 0; i-- {
		switch c := ch[i]; c.kind {
		case ScopeStack:
			if st, err := o.store.Stacks.Get(ctx, c.id); err == nil {
				parts = append(parts, st.Slug)
			}
		case ScopeEnv:
			if e, err := o.store.Environments.Get(ctx, c.id); err == nil {
				parts = append(parts, e.Slug)
			}
		case ScopeTile:
			if t, err := o.store.Tiles.Get(ctx, c.id); err == nil {
				parts = append(parts, t.Slug)
			}
		}
	}
	return strings.Join(parts, "/")
}

// orgSlugs turns org ids into slugs; a gone org keeps its id.
func (o *Orchestrator) orgSlugs(ctx context.Context, ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if og, err := o.store.Orgs.Get(ctx, id); err == nil {
			id = og.Slug
		}
		out = append(out, id)
	}
	return out
}

// nodeEnvOuts are the envs whose node_id is the node, with their paths and
// volumes (and each volume's newest finished backup).
func (o *Orchestrator) nodeEnvOuts(ctx context.Context, nodeID string) ([]NodeEnvOut, error) {
	es, err := o.store.Environments.ListByNode(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	out := make([]NodeEnvOut, 0, len(es))
	for _, e := range es {
		ne, err := o.nodeEnvOut(ctx, e)
		if err != nil {
			return nil, err
		}
		out = append(out, ne)
	}
	return out, nil
}

func (o *Orchestrator) nodeEnvOut(ctx context.Context, e store.Environment) (NodeEnvOut, error) {
	st, err := o.store.Stacks.Get(ctx, e.StackID)
	if err != nil {
		return NodeEnvOut{}, err
	}
	og, err := o.store.Orgs.Get(ctx, st.OrgID)
	if err != nil {
		return NodeEnvOut{}, err
	}
	ne := NodeEnvOut{ID: e.ID, Path: og.Slug + "/" + st.Slug + "/" + e.Slug, Org: og.Slug, Placement: o.placementName(ctx, e.PlacementID), Volumes: []NodeVolume{}}
	vs, err := o.store.Volumes.ListByScope(ctx, "env", e.ID)
	if err != nil {
		return ne, err
	}
	for _, v := range vs {
		nv := NodeVolume{ID: v.ID, Name: v.Slug}
		runs, err := o.store.BackupRuns.ListByVolume(ctx, v.ID)
		if err != nil {
			return ne, err
		}
		for _, r := range runs {
			if r.Status == backup.Done && (nv.LastBackup == nil || r.CreatedAt.After(*nv.LastBackup)) {
				t := r.CreatedAt
				nv.LastBackup, nv.RunID = &t, r.ID
			}
		}
		ne.Volumes = append(ne.Volumes, nv)
	}
	return ne, nil
}

// reportedFacts is what a worker last reported: the mesh service's copy, else
// a brief ask; nil when it cannot answer.
func (o *Orchestrator) reportedFacts(ctx context.Context, n store.Node) *worker.Facts {
	if n.Role == node.Controller {
		return o.controllerFacts(ctx, n)
	}
	if n.Role != node.Worker || n.Status != node.StatusOnline {
		return nil
	}
	if o.mesh != nil {
		if f, ok := o.mesh.factsOf(n.ID); ok {
			return &f
		}
	}
	cl, err := o.nodeExtras(n.ID)
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, factsWait)
	defer cancel()
	f, err := cl.Facts(ctx)
	if err != nil {
		return nil
	}
	return &f
}

func (o *Orchestrator) nodeOut(ctx context.Context, n store.Node) (NodeOut, error) {
	es, err := o.store.Environments.ListByNode(ctx, n.ID)
	if err != nil {
		return NodeOut{}, err
	}
	ps, err := o.store.Placements.ListByNode(ctx, n.ID)
	if err != nil {
		return NodeOut{}, err
	}
	names := make([]string, 0, len(ps))
	for _, p := range ps {
		names = append(names, p.Name)
	}
	slices.Sort(names)
	out := NodeOut{
		ID: n.ID, Name: n.Name, Role: n.Role, State: o.shownStatus(n), Tags: node.Tags(n), Envs: len(es),
		Placements: names, Range: envNets(es), WGAddress: n.WGAddress, DefaultExit: []string{}, PublicIP: n.PublicIP, Address: n.Address,
	}
	if !hasEgress(n) {
		ids, err := scopeLadder(ctx, o.store, scopeNode, n.ID)
		if err != nil {
			return NodeOut{}, err
		}
		for _, id := range ids {
			if x, err := o.store.Nodes.Get(ctx, id); err == nil {
				out.DefaultExit = append(out.DefaultExit, x.Name)
			}
		}
	}
	f := o.reportedFacts(ctx, n)
	out.Warnings = o.nodeWarnings(ctx, n, f)
	if f != nil {
		out.CPU, out.MemTotal, out.DiskFree, out.DiskTotal, out.Docker, out.Arch = f.CPUs, f.MemBytes, f.DiskFree, f.DiskTotal, f.Docker, f.Arch
		out.Version, out.Behind = f.Version, o.workerBehind(n, f)
		out.TooOld = n.Role == node.Worker && !o.versionAllowed(ctx, f.Version)
	}
	return out, nil
}

// ListNodes is every confirmed node, controller first.
func (o *Orchestrator) ListNodes(ctx context.Context) ([]NodeOut, error) {
	if _, err := o.controllerID(ctx); err != nil {
		return nil, err
	}
	ns, err := o.nodeLeaf().List(ctx)
	if err != nil {
		return nil, err
	}
	ns = slices.DeleteFunc(ns, func(n store.Node) bool { return n.Status == node.StatusPending && !rejoin(n) })
	slices.SortFunc(ns, func(a, b store.Node) int {
		if (a.Role == node.Controller) != (b.Role == node.Controller) {
			if a.Role == node.Controller {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
	out := make([]NodeOut, len(ns))
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	for i, n := range ns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := o.nodeOut(ctx, n)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				firstErr = err
				return
			}
			out[i] = v
		}()
	}
	wg.Wait()
	return out, firstErr
}

// GetNode is `node show`.
func (o *Orchestrator) GetNode(ctx context.Context, ref string) (NodeInfo, error) {
	n, err := o.nodeByRef(ctx, ref)
	if err != nil {
		return NodeInfo{}, err
	}
	d, err := o.NodeDetail(ctx, n.ID) // pending nodes are not found
	if err != nil {
		return NodeInfo{}, err
	}
	v, err := o.nodeOut(ctx, n)
	if err != nil {
		return NodeInfo{}, err
	}
	out := NodeInfo{NodeOut: v, Facts: d.Facts, ExitFor: []string{}, IngressFor: []string{}}
	if out.EnvList, err = o.nodeEnvOuts(ctx, n.ID); err != nil {
		return out, err
	}
	for _, c := range d.Certs {
		if !c.Revoked && (out.Cert == nil || c.NotAfter.After(out.Cert.NotAfter)) {
			out.Cert = &NodeCertOut{Serial: c.Serial, NotAfter: c.NotAfter, RenewsOn: c.RenewFrom}
		}
	}
	targets, err := nodeTargets(ctx, o.store.Placements, n.ID)
	if err != nil {
		return out, err
	}
	for _, t := range targets {
		rungs, err := o.store.ExitLadders.ListByTarget(ctx, t)
		if err != nil {
			return out, err
		}
		for _, r := range rungs {
			out.ExitFor = append(out.ExitFor, o.scopePath(ctx, r.ScopeKind, r.ScopeID))
		}
	}
	picks, err := o.picksOfNode(ctx, n.ID)
	if err != nil {
		return out, err
	}
	for _, p := range picks {
		out.IngressFor = append(out.IngressFor, o.scopePath(ctx, p.ScopeKind, p.ScopeID))
	}
	return out, nil
}

// TagSpec is a tag change: both lists apply, adds first.
type TagSpec struct {
	Add    []string `json:"add"`
	Remove []string `json:"remove"`
}

// TagNode adds and removes capability tags (SetNodeTags holds the rules).
func (o *Orchestrator) TagNode(ctx context.Context, by *Principal, ref string, spec TagSpec) (NodeOut, error) {
	n, err := o.nodeByRef(ctx, ref)
	if err != nil {
		return NodeOut{}, err
	}
	tags := node.Tags(n)
	for _, t := range spec.Add {
		if !slices.Contains(tags, t) {
			tags = append(tags, t)
		}
	}
	tags = slices.DeleteFunc(tags, func(t string) bool { return slices.Contains(spec.Remove, t) })
	if err := o.SetNodeTags(ctx, by, n.ID, tags); err != nil {
		return NodeOut{}, err
	}
	if n, err = o.store.Nodes.Get(ctx, n.ID); err != nil {
		return NodeOut{}, err
	}
	return o.nodeOut(ctx, n)
}

// RemoveNodeRef queues the removal of a worker with no envs left (RemoveNode
// by name).
func (o *Orchestrator) RemoveNodeRef(ctx context.Context, by *Principal, ref string) (Job, error) {
	n, err := o.nodeByRef(ctx, ref)
	if err != nil {
		return Job{}, err
	}
	return o.RemoveNode(ctx, by, n.ID)
}

// ForgetOut is what a forget left behind.
type ForgetOut struct {
	Node string       `json:"node"`
	Lost []NodeEnvOut `json:"lost"`
	// Job cleans a node still online before it is forgotten; nil when the
	// node was forgotten at once.
	Job *Job `json:"job,omitempty"`
}

// ForgetNodeRef writes off a dead worker (ForgetNode by name; the caller has
// confirmed): its envs are lost until restored.
func (o *Orchestrator) ForgetNodeRef(ctx context.Context, by *Principal, ref string) (ForgetOut, error) {
	n, err := o.nodeByRef(ctx, ref)
	if err != nil {
		return ForgetOut{}, err
	}
	if err := nodeAdmin(by); err != nil {
		return ForgetOut{}, err
	}
	lost, err := o.nodeEnvOuts(ctx, n.ID)
	if err != nil {
		return ForgetOut{}, err
	}
	j, err := o.ForgetNode(ctx, by, n.ID, n.Name)
	if err != nil {
		return ForgetOut{}, err
	}
	out := ForgetOut{Node: n.Name, Lost: lost}
	if j.ID != "" {
		out.Job = &j
	}
	return out, nil
}

// rejoinMu makes expire-then-mint one step per panel.
var rejoinMu sync.Mutex

// RejoinNode answers a new join command for a worker that broke. The node
// keeps its name, range and envs; the old certs stop working when the new
// request is confirmed.
func (o *Orchestrator) RejoinNode(ctx context.Context, by *Principal, ref string) (JoinTokenOut, error) {
	if err := nodeAdmin(by); err != nil {
		return JoinTokenOut{}, err
	}
	n, err := o.nodeByRef(ctx, ref)
	if err != nil {
		return JoinTokenOut{}, err
	}
	if n.Role != node.Worker || n.Status == node.StatusPending {
		return JoinTokenOut{}, errs.Conflictf("%s cannot be rejoined (%s, %s)", n.Name, n.Role, n.Status)
	}
	n.Status = StateNeedsRejoin
	if err := o.store.Nodes.Update(ctx, n); err != nil {
		return JoinTokenOut{}, err
	}
	// One live command per node: a leaked older one must not outlive a new
	// ask, and two asks at once must not both mint after both expired.
	rejoinMu.Lock()
	defer rejoinMu.Unlock()
	if _, err := o.store.JoinTokens.ExpireUnspentFor(ctx, n.Name, time.Now().UTC()); err != nil {
		return JoinTokenOut{}, err
	}
	out, err := o.mintJoin(ctx, by, n.Name, "", node.Tags(n), 0)
	if err != nil {
		return out, err
	}
	err = o.store.Tx(ctx, func(tx store.Tx) error {
		return audit(tx).Append(ctx, securitylog.Event{
			Action: auditNodeRejoin, ActorID: by.User.ID, KeyID: by.KeyID, TargetKind: "node", TargetID: n.ID, Detail: "node " + n.Name,
		})
	})
	return out, err
}

// LostEnvsOf are the org's envs whose node was forgotten, with their volumes
// and newest backups. They come back by repointing their placement (D8).
func (o *Orchestrator) LostEnvsOf(ctx context.Context, orgSlug string) ([]NodeEnvOut, error) {
	fs, err := o.store.ForgottenNodes.List(ctx)
	if err != nil {
		return nil, err
	}
	out := []NodeEnvOut{}
	for _, f := range fs {
		es, err := o.nodeEnvOuts(ctx, f.ID)
		if err != nil {
			return nil, err
		}
		for _, e := range es {
			if e.Org == orgSlug {
				out = append(out, e)
			}
		}
	}
	return out, nil
}

// ---- cluster status ----

// ClusterStatusNode is a node in the cluster view.
type ClusterStatusNode struct {
	NodeOut
	Public []string `json:"public"` // public addresses directly on an interface
}

// ClusterGroup is nodes that share a network: one per private subnet, and
// "internet" for the ones with a public address.
type ClusterGroup struct {
	Name  string   `json:"name"`
	Nodes []string `json:"nodes"`
}

// ClusterStatusLink is the path between two nodes.
type ClusterStatusLink struct {
	A     string  `json:"a"`
	B     string  `json:"b"`
	Path  string  `json:"path"`         // lan | public | relay
	State string  `json:"state"`        // up | down
	MS    float64 `json:"ms,omitempty"` // pinged round trip over the mesh; 0 = not measured
}

// ClusterOut is `cluster status`.
type ClusterOut struct {
	Nodes    []ClusterStatusNode `json:"nodes"`
	Groups   []ClusterGroup      `json:"groups"`
	Links    []ClusterStatusLink `json:"links"`
	Problems []string            `json:"problems"`
}

// ClusterStatus is the cluster map (ClusterMap) in names: the nodes, who
// shares a network, and each pair's path and state.
func (o *Orchestrator) ClusterStatus(ctx context.Context) (ClusterOut, error) {
	ns, err := o.ListNodes(ctx)
	if err != nil {
		return ClusterOut{}, err
	}
	m, err := o.ClusterMap(ctx)
	if err != nil {
		return ClusterOut{}, err
	}
	out := ClusterOut{Nodes: []ClusterStatusNode{}, Groups: []ClusterGroup{}, Links: []ClusterStatusLink{}, Problems: []string{}}
	byID := map[string]ClusterNode{}
	for _, n := range m.Nodes {
		byID[n.ID] = n
	}
	name := func(id string) string { return byID[id].Name }
	var inet []string
	for _, v := range ns {
		cn := ClusterStatusNode{NodeOut: v, Public: []string{}}
		if mn, ok := byID[v.ID]; ok {
			cn.Public = append(cn.Public, mn.Public...)
			if mn.Status != NodeOnline {
				v.State = mn.Status
				cn.State = mn.Status
			}
		}
		if len(cn.Public) > 0 {
			inet = append(inet, v.Name)
		}
		out.Nodes = append(out.Nodes, cn)
		if cn.State != node.StatusOnline && cn.State != NodeOnline {
			out.Problems = append(out.Problems, fmt.Sprintf("%s is %s", v.Name, cn.State))
		}
	}
	if len(inet) > 0 {
		out.Groups = append(out.Groups, ClusterGroup{Name: "internet", Nodes: inet})
	}
	for _, nt := range m.Networks {
		if len(nt.Nodes) < 2 || nt.CIDR == "" {
			continue
		}
		g := ClusterGroup{Name: nt.CIDR}
		for _, id := range nt.Nodes {
			g.Nodes = append(g.Nodes, name(id))
		}
		out.Groups = append(out.Groups, g)
	}
	up := map[[2]string]ClusterLink{}
	pairs := m.Pairs
	if pairs == nil {
		pairs = m.Links
	}
	for _, l := range pairs {
		up[[2]string{l.A, l.B}] = l
	}
	// A down link has no kind; two nodes in one network still label as lan.
	sameNet := func(a, b string) bool {
		for _, nt := range m.Networks {
			if slices.Contains(nt.Nodes, a) && slices.Contains(nt.Nodes, b) {
				return true
			}
		}
		return false
	}
	for i, a := range m.Nodes {
		for _, b := range m.Nodes[i+1:] {
			l := ClusterStatusLink{A: a.Name, B: b.Name, State: "down"}
			ml, ok := up[[2]string{a.ID, b.ID}]
			if ok {
				l.State = "up"
				l.MS = max(ml.MS, 0)
			}
			kind := ml.Kind
			switch {
			case ml.Via != "":
				l.Path = "via " + ml.Via
			case kind == LinkLAN || kind == LinkPrivate || kind == "" && sameNet(a.ID, b.ID):
				l.Path = "lan"
			case len(a.Public) > 0 || len(b.Public) > 0:
				l.Path = "public"
			default:
				l.Path = "none" // no LAN, no public side, no route yet
			}
			out.Links = append(out.Links, l)
		}
	}
	return out, nil
}

// ---- placement by name ----

// PinOut is where new envs of a scope go; Placement is "" with no pin.
type PinOut struct {
	Scope     string `json:"scope"`
	Placement string `json:"placement"`
}

// PinOf answers where new envs of an org or stack go (its own pin, else the
// one above it), or the placement an env belongs to.
func (o *Orchestrator) PinOf(ctx context.Context, kind, id string) (PinOut, error) {
	ch, og, err := o.chain(ctx, kind, id)
	if err != nil {
		return PinOut{}, err
	}
	out := PinOut{Scope: o.scopePath(ctx, kind, id)}
	for _, c := range ch {
		var pin *string
		switch c.kind {
		case ScopeEnv:
			e, err := o.store.Environments.Get(ctx, c.id)
			if err != nil {
				return out, err
			}
			if e.PlacementID != "" {
				out.Placement = o.placementName(ctx, e.PlacementID)
				return out, nil
			}
		case ScopeStack:
			st, err := o.store.Stacks.Get(ctx, c.id)
			if err != nil {
				return out, err
			}
			pin = st.PlacementID
		case ScopeOrg:
			pin = og.PlacementID
		}
		if pin != nil {
			out.Placement = o.placementName(ctx, *pin)
			return out, nil
		}
	}
	return out, nil
}

// PlacementForNewEnv is the placement id a new env is asked to belong to
// (--placement); the org must have it open, and a placement with a node needs
// that node online.
func (o *Orchestrator) PlacementForNewEnv(ctx context.Context, by *Principal, stackID, name string) (string, error) {
	orgID, err := o.placementOrg(ctx, ScopeStack, stackID)
	if err != nil {
		return "", err
	}
	p, err := o.offers(ctx, by, orgID, name)
	if err != nil {
		return "", err
	}
	if st, err := o.nodeStateOf(ctx, p); err != nil {
		return "", err
	} else if st.HasNode && !st.Online {
		return "", errs.Conflictf("%s is offline.", p.Name)
	}
	return p.ID, nil
}

// IngressOut is the ingress placement of a scope and where that came from.
// Placement is "cluster default" when nothing is picked and the env's own
// node is not an ingress.
type IngressOut struct {
	Scope     string `json:"scope"`
	Placement string `json:"placement"`
	From      string `json:"from"` // the scope that set it, or "default"
	// Moved are the hosts a set sent to another proxy: their DNS must point
	// at it.
	Moved []string `json:"moved,omitempty"`
	// DNS is what the move does to the hosts' records: the hand ones to
	// repoint, the managed ones stackr repoints.
	DNS DNSMove `json:"dns"`
}

// IngressGet is the effective ingress of a scope: the nearest pick up the
// chain, else the env's own placement when its node is tagged ingress, else
// the cluster default.
func (o *Orchestrator) IngressGet(ctx context.Context, kind, id string) (IngressOut, error) {
	v := PlacementView{Kind: kind, ID: id}
	ch, _, err := o.chain(ctx, kind, id)
	if err != nil {
		return IngressOut{}, err
	}
	if err := o.ingressView(ctx, &v, ch); err != nil {
		return IngressOut{}, err
	}
	out := IngressOut{Scope: o.scopePath(ctx, kind, id), Placement: v.IngressName, From: "default"}
	for _, c := range ch {
		if pick, err := o.ingressPick(ctx, c.kind, c.id); err != nil {
			return out, err
		} else if pick != "" {
			out.From = o.scopePath(ctx, c.kind, c.id)
			break
		}
	}
	return out, nil
}

// IngressSet picks the ingress placement of a scope; ref "" clears the pick
// so the scope inherits again.
func (o *Orchestrator) IngressSet(ctx context.Context, by *Principal, kind, id, ref string) (IngressOut, error) {
	err := o.PickIngress(ctx, by, kind, id, ref, false)
	var hm *IngressHostsMove
	if errors.As(err, &hm) {
		err = o.SetIngress(ctx, by, kind, id, ref)
	}
	if err != nil {
		return IngressOut{}, err
	}
	out, err := o.IngressGet(ctx, kind, id)
	if hm != nil {
		out.Moved, out.DNS = hm.Hosts, hm.DNS
	}
	return out, err
}

// ExitRungOut is one exit in a scope's list.
type ExitRungOut struct {
	Position  int    `json:"position"`
	Placement string `json:"placement"`
	State     string `json:"state"` // in use | standby | down | "" when unknown
}

// ExitOut is the exit list of a scope. An empty list means the env's own node.
type ExitOut struct {
	Scope string        `json:"scope"`
	From  string        `json:"from"`
	Exits []ExitRungOut `json:"exits"`
	// NoInternet is set when every exit is down: the env's traffic is
	// dropped, never sent out its own uplink; Why says what the ladder saw.
	NoInternet bool   `json:"no_internet"`
	Why        string `json:"why,omitempty"`
}

// ExitGet is the list in force for a scope: its own, else the nearest scope
// above that has one.
func (o *Orchestrator) ExitGet(ctx context.Context, kind, id string) (ExitOut, error) {
	v, err := o.Placement(ctx, kind, id)
	if err != nil {
		return ExitOut{}, err
	}
	out := ExitOut{Scope: o.scopePath(ctx, kind, id), From: "default", Exits: []ExitRungOut{}}
	ch, _, err := o.chain(ctx, kind, id)
	if err != nil {
		return out, err
	}
	for _, c := range ch { // the nearest scope with a list of its own
		rs, err := o.store.ExitLadders.List(ctx, c.kind, c.id)
		if err != nil {
			return out, err
		}
		if len(rs) > 0 {
			out.From = o.scopePath(ctx, c.kind, c.id)
			break
		}
	}
	for i, r := range v.Rungs {
		out.Exits = append(out.Exits, ExitRungOut{Position: i + 1, Placement: r.Name, State: r.State})
	}
	if v.NoNet {
		out.NoInternet, out.Why = true, v.NoNetWhy
	}
	return out, nil
}

// ExitSet replaces the scope's exit list (placement names; "own" is the env's
// own uplink); none clears it.
func (o *Orchestrator) ExitSet(ctx context.Context, by *Principal, kind, id string, refs []string) (ExitOut, error) {
	if err := o.SetExits(ctx, by, kind, id, refs); err != nil {
		return ExitOut{}, err
	}
	return o.ExitGet(ctx, kind, id)
}

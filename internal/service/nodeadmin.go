package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/authz"
	"github.com/FyrmForge/stackr/internal/mesh"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/securitylog"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/worker"
)

// The Nodes admin verbs: list and show nodes, edit tags, remove and forget.
// Who may use a node is the placement's business (placements.go); the envs a
// forgotten node left behind come back through their placement (D8).

// Node action names in the security log.
const (
	auditNodeTags   = "node.tags"
	auditNodeRemove = "node.remove"
	auditNodeForget = "node.forget"
	auditEnvRestore = "env.restore"
)

// NodeTags are the tags an admin may set.
var NodeTags = []string{node.TagIngress, node.TagEgress, node.TagBuilder, node.TagStorage}

func (o *Orchestrator) meshSvc() *meshSvc { return o.mesh }

func (m *meshSvc) factsOf(id string) (worker.Facts, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.facts[id]
	return f, ok
}

// NodeCard is one node in the list.
type NodeCard struct {
	ID, Name, Role, Status, Range string
	Tags                          []string
	Placements                    []string // names of the placements standing on it
	Envs                          int
	Controller                    bool
}

// NodeEnv is an env on a node.
type NodeEnv struct {
	ID, Path  string // "org / stack / env"
	Placement string // the placement the env belongs to
	Pin       string // where its placement came from
	Tiles     int
}

// NodeCertInfo is one certificate of a node.
type NodeCertInfo struct {
	Serial    string
	NotAfter  time.Time
	IssuedAt  time.Time
	Revoked   bool
	RenewFrom time.Time
}

// NodeDetail is the node drawer.
type NodeDetail struct {
	NodeCard
	WGAddress string
	Facts     *worker.Facts // nil until the node has answered
	EnvList   []NodeEnv
	Certs     []NodeCertInfo
	// RoutesServed is what the controller refuses to stop serving (the
	// panel route and every external route); 0 for a worker.
	RoutesServed int
}

func nodeAdmin(by *Principal) error {
	if by == nil {
		return errs.ErrRefused
	}
	return authz.Can(by.Access, "node.manage", authz.Resource{})
}

// shownStatus is the node's status as the panel, API and CLI show it: an
// offline worker that still answers its liveness port is "tunnel-down".
func (o *Orchestrator) shownStatus(n store.Node) string {
	if o.notRejoined(n) {
		return NodeNotRejoined
	}
	if n.Role == node.Worker && n.Status != node.StatusPending && o.certExpired(n.ID) {
		return NodeCertExpired
	}
	if n.Status == node.StatusOffline && o.HostAlive(n.ID) {
		return NodeTunnelDown
	}
	return n.Status
}

func (o *Orchestrator) card(ctx context.Context, n store.Node) (NodeCard, error) {
	es, err := o.store.Environments.ListByNode(ctx, n.ID)
	if err != nil {
		return NodeCard{}, err
	}
	ps, err := o.store.Placements.ListByNode(ctx, n.ID)
	names := make([]string, 0, len(ps))
	for _, p := range ps {
		names = append(names, p.Name)
	}
	slices.Sort(names)
	return NodeCard{
		ID: n.ID, Name: n.Name, Role: n.Role, Status: o.shownStatus(n), Range: envNets(es),
		Tags: node.Tags(n), Placements: names, Envs: len(es), Controller: n.Role == node.Controller,
	}, err
}

// Nodes (server admin) lists the nodes that have joined, the controller first.
func (o *Orchestrator) Nodes(ctx context.Context) ([]NodeCard, error) {
	if _, err := o.controllerID(ctx); err != nil {
		return nil, err
	}
	ns, err := o.nodeLeaf().List(ctx)
	if err != nil {
		return nil, err
	}
	slices.SortStableFunc(ns, func(a, b store.Node) int {
		if (a.Role == node.Controller) != (b.Role == node.Controller) {
			if a.Role == node.Controller {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
	var out []NodeCard
	for _, n := range ns {
		if n.Status == node.StatusPending {
			continue
		}
		c, err := o.card(ctx, n)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// NodeDetail (server admin) is everything the node drawer shows.
func (o *Orchestrator) NodeDetail(ctx context.Context, id string) (NodeDetail, error) {
	n, err := o.nodeLeaf().Get(ctx, id)
	if err != nil {
		return NodeDetail{}, err
	}
	if n.Status == node.StatusPending {
		return NodeDetail{}, errs.ErrNotFound
	}
	c, err := o.card(ctx, n)
	if err != nil {
		return NodeDetail{}, err
	}
	d := NodeDetail{NodeCard: c, WGAddress: n.WGAddress}
	if m := o.meshSvc(); m != nil {
		if f, ok := m.factsOf(id); ok {
			d.Facts = &f
		}
	}
	if d.EnvList, err = o.nodeEnvs(ctx, id); err != nil {
		return d, err
	}
	certs, err := o.store.NodeCerts.ListByNode(ctx, id)
	if err != nil {
		return d, err
	}
	for _, c := range certs {
		// a certificate renews in its last third
		life := c.NotAfter.Sub(c.CreatedAt)
		d.Certs = append(d.Certs, NodeCertInfo{
			Serial: c.Serial, NotAfter: c.NotAfter, IssuedAt: c.CreatedAt,
			Revoked: c.RevokedAt != nil, RenewFrom: c.NotAfter.Add(-life / 3),
		})
	}
	if c.Controller {
		rs, err := o.routes.List(ctx)
		if err != nil {
			return d, err
		}
		d.RoutesServed = len(rs) + 1
	}
	return d, nil
}

// nodeEnvs is the envs on a node with where each one's node came from.
func (o *Orchestrator) nodeEnvs(ctx context.Context, nodeID string) ([]NodeEnv, error) {
	es, err := o.store.Environments.ListByNode(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	var out []NodeEnv
	for _, e := range es {
		st, err := o.stacks.Get(ctx, e.StackID)
		if err != nil {
			return nil, err
		}
		og, err := o.orgs.Get(ctx, st.OrgID)
		if err != nil {
			return nil, err
		}
		ts, err := o.tiles.List(ctx, e.ID)
		if err != nil {
			return nil, err
		}
		pin := "pinned on the env"
		switch {
		case st.PlacementID != nil && *st.PlacementID == e.PlacementID:
			pin = "pinned on the stack"
		case og.PlacementID != nil && *og.PlacementID == e.PlacementID:
			pin = "pinned on the org"
		}
		out = append(out, NodeEnv{ID: e.ID, Path: og.Slug + "/" + st.Slug + "/" + e.Slug, Placement: o.placementName(ctx, e.PlacementID), Pin: pin, Tiles: len(ts)})
	}
	return out, nil
}

// SetNodeTags (server admin) replaces a node's tags. The controller keeps its
// ingress tag while it serves the panel and routes and no other node does.
func (o *Orchestrator) SetNodeTags(ctx context.Context, by *Principal, id string, tags []string) error {
	if err := nodeAdmin(by); err != nil {
		return err
	}
	for _, t := range tags {
		if !slices.Contains(NodeTags, t) {
			return errs.Invalidf("tags", "%q is not a tag; use %s", t, strings.Join(NodeTags, ", "))
		}
	}
	tags = slices.Compact(slices.Sorted(slices.Values(tags)))
	n, err := o.nodeByRef(ctx, id) // a name, an id or a unique id prefix
	if err != nil {
		return err
	}
	if n.Status == node.StatusPending {
		return errs.ErrNotFound
	}
	if slices.Contains(node.Tags(n), node.TagIngress) && !slices.Contains(tags, node.TagIngress) {
		if di, err := o.DefaultIngress(ctx); err != nil {
			return err
		} else if di == n.ID {
			rs, err := o.routes.List(ctx)
			if err != nil {
				return err
			}
			return errs.Conflictf("%s is the default ingress: it serves the panel and %d routes. Set another default ingress first.", n.Name, len(rs))
		}
	}
	if slices.Contains(node.Tags(n), node.TagStorage) && !slices.Contains(tags, node.TagStorage) {
		es, err := o.store.Environments.ListByNode(ctx, n.ID)
		if err != nil {
			return err
		}
		for _, e := range es {
			if need, err := o.envNeeds(ctx, e.ID); err != nil {
				return err
			} else if need.Storage {
				return errs.Conflictf("%s still holds data for %s; move it first.", n.Name, o.scopePath(ctx, ScopeEnv, e.ID))
			}
		}
	}
	if slices.Contains(node.Tags(n), node.TagEgress) && !slices.Contains(tags, node.TagEgress) && o.DefaultExit(ctx) == n.ID {
		return errs.Conflictf("%s is the default exit. Set another default exit (or clear it) first.", n.Name)
	}
	losesEgress := slices.Contains(node.Tags(n), node.TagEgress) && !slices.Contains(tags, node.TagEgress)
	if losesEgress {
		// Without egress its traffic leaves through other nodes, so it needs one.
		if ex, err := scopeLadder(ctx, o.store, scopeNode, n.ID); err != nil {
			return err
		} else if len(ex) == 0 {
			return errs.Conflictf("Pick a default exit for %s first.", n.Name)
		}
	}
	gainsEgress := !slices.Contains(node.Tags(n), node.TagEgress) && slices.Contains(tags, node.TagEgress)
	if slices.Contains(node.Tags(n), node.TagIngress) && !slices.Contains(tags, node.TagIngress) {
		picks, err := o.picksOfNode(ctx, n.ID)
		if err != nil {
			return err
		}
		if len(picks) > 0 {
			return errs.Conflictf("%s is the ingress of %s; pick another ingress there first.", n.Name, o.scopePath(ctx, picks[0].ScopeKind, picks[0].ScopeID))
		}
	}
	ingressChanged := slices.Contains(node.Tags(n), node.TagIngress) != slices.Contains(tags, node.TagIngress)
	raw, _ := json.Marshal(tags)
	n.Tags = string(raw)
	err = o.store.Tx(ctx, func(tx store.Tx) error {
		if err := tx.Nodes.Update(ctx, n); err != nil {
			return err
		}
		if gainsEgress { // it leaves by its own IP now: no default exit to keep
			if err := tx.ExitLadders.Set(ctx, scopeNode, n.ID, nil); err != nil {
				return err
			}
		}
		return audit(tx).Append(ctx, securitylog.Event{
			Action: auditNodeTags, ActorID: by.User.ID, KeyID: by.KeyID,
			TargetKind: "node", TargetID: n.ID, Detail: strings.Join(tags, ","),
		})
	})
	if err == nil {
		if m := o.meshSvc(); m != nil {
			m.Kick()
		}
		if ingressChanged {
			go o.ingressTagged(n.ID)
		}
	}
	return err
}

// dropNode revokes the node's certs, takes it out of the mesh and deletes the
// row; its transit address is free again. forget also records the node so its
// envs show as lost.
func (o *Orchestrator) dropNode(ctx context.Context, by *Principal, n store.Node, action string, forget bool) error {
	if m := o.meshSvc(); m != nil {
		if err := m.RemoveNode(ctx, n.ID); err != nil {
			return err
		}
	}
	err := o.store.Tx(ctx, func(tx store.Tx) error {
		now := time.Now().UTC()
		if _, err := tx.NodeCerts.RevokeNode(ctx, n.ID, now); err != nil {
			return err
		}
		if forget {
			if err := tx.ForgottenNodes.Create(ctx, store.ForgottenNode{ID: n.ID, Name: n.Name, ForgottenAt: now}); err != nil {
				return err
			}
		}
		if err := tx.Nodes.Delete(ctx, n.ID); err != nil {
			return err
		}
		return audit(tx).Append(ctx, securitylog.Event{
			Action: action, ActorID: by.User.ID, KeyID: by.KeyID,
			TargetKind: "node", TargetID: n.ID, Detail: n.Name,
		})
	})
	if err == nil {
		o.refreshNodes(ctx)
		o.kickMesh()
	}
	return err
}

// refreshNodes drops the deleted node's client.
func (o *Orchestrator) refreshNodes(ctx context.Context) {
	if o.cluster != nil {
		_ = o.refreshNodeClients(ctx)
	}
}

func (o *Orchestrator) workerNode(ctx context.Context, id string) (store.Node, error) {
	n, err := o.nodeLeaf().Get(ctx, id)
	if err != nil {
		return n, err
	}
	if n.Role == node.Controller {
		return n, errs.Refusedf("%s is the controller; it cannot be removed.", n.Name)
	}
	return n, nil
}

// ForgetPreview (server admin) is the envs that are lost if the node is forgotten.
func (o *Orchestrator) ForgetPreview(ctx context.Context, id string) ([]NodeEnv, error) {
	if _, err := o.workerNode(ctx, id); err != nil {
		return nil, err
	}
	return o.nodeEnvs(ctx, id)
}

// ForgetNode (server admin) drops a node that is gone for good: its certs are
// revoked and its links cut; its envs become lost until redeployed. confirm
// must be the node's name. An offline node is dropped at once (Job is zero).
// One still online is cleaned first by a job, as a remove cleans it, so the
// box can join again without stackr's old mesh and networks in the way.
func (o *Orchestrator) ForgetNode(ctx context.Context, by *Principal, id, confirm string) (Job, error) {
	if err := nodeAdmin(by); err != nil {
		return Job{}, err
	}
	n, err := o.workerNode(ctx, id)
	if err != nil {
		return Job{}, err
	}
	if confirm != n.Name {
		return Job{}, errs.Invalidf("confirm", "type %s to confirm", n.Name)
	}
	// Forgetting would silently clear that node's exit, as a remove refuses to.
	if err := o.exitGuard(ctx, n); err != nil {
		return Job{}, err
	}
	if o.nodeUp(ctx, n.ID) == nil {
		return o.enqueue(ctx, kindForgetNode, nodeJob{NodeID: n.ID, By: by.User.ID, KeyID: by.KeyID}, "node:"+n.ID)
	}
	return Job{}, o.dropNode(ctx, by, n, auditNodeForget, true)
}

const kindForgetNode jobs.Kind = "forget-node"

// runForgetNode is the "forget-node" job: clean the box while the mesh still
// reaches it, then forget the node. A failed clean is logged, not fatal: the
// node is gone for good either way, and its installer clears what is left
// when the box joins again.
func (o *Orchestrator) runForgetNode(ctx context.Context, r *jobs.Run, p nodeJob) error {
	n, err := o.workerNode(ctx, p.NodeID)
	if err != nil {
		return err
	}
	if err := o.exitGuard(ctx, n); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(r.Log, "cleaning %s\n", n.Name)
	if err := hostUninstall(o, ctx, n.ID, r.Log); err != nil {
		_, _ = fmt.Fprintf(r.Log, "could not clean %s: %v; forgetting it anyway\n", n.Name, err)
	}
	by := &Principal{User: User{ID: p.By}, KeyID: p.KeyID}
	if err := o.dropNode(ctx, by, n, auditNodeForget, true); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(r.Log, "%s forgotten\n", n.Name)
	return nil
}

// LostEnv is an env whose node was forgotten.
type LostEnv struct {
	ID, Path, Node string // Node is the forgotten node's name
	Placement      string // repoint it to restore its envs (D8)
	Volumes        int
	Tiles          int
}

// LostEnvs (server admin) lists the envs of forgotten nodes.
func (o *Orchestrator) LostEnvs(ctx context.Context) ([]LostEnv, error) {
	fs, err := o.store.ForgottenNodes.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []LostEnv
	for _, f := range fs {
		es, err := o.nodeEnvs(ctx, f.ID)
		if err != nil {
			return nil, err
		}
		for _, e := range es {
			vs, err := o.pmVolumes(ctx, e.ID) // orphans are not restored; the repoint wizard counts the same
			if err != nil {
				return nil, err
			}
			out = append(out, LostEnv{ID: e.ID, Path: e.Path, Node: f.Name, Placement: e.Placement, Volumes: len(vs), Tiles: e.Tiles})
		}
	}
	return out, nil
}

// PendingNode is a machine that called in with a join token. Addresses are
// the ones a person knows the machine by: no loopback, link-local or Docker
// and stackr bridges.
type PendingNode struct {
	ID, Name    string
	Tags        []string
	Addresses   []string
	Facts       worker.Facts
	RequestedAt time.Time
}

// PendingNodes (server admin) is every machine that called in and waits for
// a confirm or a reject, the ones a confirm refused among them.
func (o *Orchestrator) PendingNodes(ctx context.Context) ([]PendingNode, error) {
	ps, err := o.pendingJoins(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]PendingNode, 0, len(ps))
	for _, p := range ps {
		n := PendingNode{ID: p.NodeID, Name: p.Name, Tags: p.Tags, RequestedAt: p.RequestedAt}
		_ = json.Unmarshal(p.Facts, &n.Facts)
		n.Addresses = hostAddresses(n.Facts.Interfaces)
		out = append(out, n)
	}
	return out, nil
}

// PendingNodeByName (server admin) is the pending node of that name, or
// ErrNotFound while the machine has not called in.
func (o *Orchestrator) PendingNodeByName(ctx context.Context, name string) (PendingNode, error) {
	ps, err := o.PendingNodes(ctx)
	if err != nil {
		return PendingNode{}, err
	}
	for _, p := range ps {
		if p.Name == name {
			return p, nil
		}
	}
	return PendingNode{}, errs.ErrNotFound
}

// hostAddresses are the addresses of the machine's own interfaces: loopback,
// link-local, Docker's and stackr's bridges and veths are left out.
func hostAddresses(is []worker.Iface) []string {
	var out []string
	for _, i := range is {
		if i.Name == "lo" || i.Name == "docker0" || i.Name == mesh.Iface ||
			strings.HasPrefix(i.Name, "br-") || strings.HasPrefix(i.Name, "veth") {
			continue
		}
		for _, s := range i.Prefixes {
			p, err := netip.ParsePrefix(s)
			if err != nil || p.Addr().IsLoopback() || p.Addr().IsLinkLocalUnicast() {
				continue
			}
			out = append(out, s)
		}
	}
	return out
}

// envNets is the networks of envs es, as a node shows them: its blocks of the
// cluster pool, in order.
func envNets(es []store.Environment) string {
	var out []string
	for _, e := range es {
		if e.Subnet != "" {
			out = append(out, e.Subnet)
		}
	}
	slices.Sort(out)
	return strings.Join(out, ", ")
}

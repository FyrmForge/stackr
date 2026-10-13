package service

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/authz"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/securitylog"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/worker"
)

// The Nodes admin verbs: list and show nodes, edit tags, open a node to orgs,
// remove, forget, and redeploy the envs a forgotten node left behind.
//

// Node action names in the security log.
const (
	auditNodeTags   = "node.tags"
	auditNodeOpen   = "node.open"
	auditNodeRemove = "node.remove"
	auditNodeForget = "node.forget"
	auditEnvRestore = "env.restore"
)

// NodeTags are the tags an admin may set.
var NodeTags = []string{node.TagIngress, node.TagEgress, node.TagBuilder}

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
	Envs                          int
	Controller                    bool
}

// NodeEnv is an env on a node.
type NodeEnv struct {
	ID, Path string // "org / stack / env"
	Pin      string // where its node came from
	Tiles    int
}

// NodeCertInfo is one certificate of a node.
type NodeCertInfo struct {
	Serial    string
	NotAfter  time.Time
	IssuedAt  time.Time
	Revoked   bool
	RenewFrom time.Time
}

// NodeOrg is an org and whether the node is opened to it.
type NodeOrg struct {
	ID, Name string
	On       bool
}

// NodeDetail is the node drawer.
type NodeDetail struct {
	NodeCard
	WGAddress string
	Facts     *worker.Facts // nil until the node has answered
	EnvList   []NodeEnv
	Certs     []NodeCertInfo
	Orgs      []NodeOrg
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
	return NodeCard{
		ID: n.ID, Name: n.Name, Role: n.Role, Status: o.shownStatus(n), Range: envNets(es),
		Tags: node.Tags(n), Envs: len(es), Controller: n.Role == node.Controller,
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
	if d.Orgs, err = o.nodeOrgList(ctx, n); err != nil {
		return d, err
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
		case st.NodeID != nil && *st.NodeID == nodeID:
			pin = "pinned on the stack"
		case og.NodeID != nil && *og.NodeID == nodeID:
			pin = "pinned on the org"
		}
		out = append(out, NodeEnv{ID: e.ID, Path: og.Slug + " / " + st.Slug + " / " + e.Slug, Pin: pin, Tiles: len(ts)})
	}
	return out, nil
}

func (o *Orchestrator) nodeOrgList(ctx context.Context, n store.Node) ([]NodeOrg, error) {
	ogs, err := o.orgs.ListAll(ctx)
	if err != nil {
		return nil, err
	}
	on, err := o.store.NodeOrgs.OrgIDs(ctx, n.ID)
	if err != nil {
		return nil, err
	}
	var out []NodeOrg
	for _, og := range ogs {
		out = append(out, NodeOrg{ID: og.ID, Name: og.Name, On: n.Role == node.Controller || slices.Contains(on, og.ID)})
	}
	return out, nil
}

// NodeOpenToOrg reports whether an org's owners may pin to the node: the
// controller is open to every org, a worker only to the orgs an admin opened
// it to. Admins pin anywhere; the placement verbs ask this for owners.
func (o *Orchestrator) NodeOpenToOrg(ctx context.Context, nodeID, orgID string) (bool, error) {
	n, err := o.nodeLeaf().Get(ctx, nodeID)
	if err != nil {
		return false, err
	}
	if n.Role == node.Controller {
		return true, nil
	}
	return o.store.NodeOrgs.Has(ctx, nodeID, orgID)
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
	n, err := o.nodeLeaf().Get(ctx, id)
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
	if slices.Contains(node.Tags(n), node.TagEgress) && !slices.Contains(tags, node.TagEgress) && o.DefaultExit(ctx) == n.ID {
		return errs.Conflictf("%s is the default exit. Set another default exit (or clear it) first.", n.Name)
	}
	if slices.Contains(node.Tags(n), node.TagIngress) && !slices.Contains(tags, node.TagIngress) {
		picks, err := o.store.IngressPicks.ListByNode(ctx, n.ID)
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

// SetNodeOrgs (server admin) opens a worker to exactly these orgs. The
// controller is always open to all.
func (o *Orchestrator) SetNodeOrgs(ctx context.Context, by *Principal, id string, orgIDs []string) error {
	if err := nodeAdmin(by); err != nil {
		return err
	}
	n, err := o.nodeLeaf().Get(ctx, id)
	if err != nil {
		return err
	}
	if n.Role == node.Controller {
		return errs.Conflictf("%s is open to every organization.", n.Name)
	}
	for _, og := range orgIDs {
		if _, err := o.orgs.Get(ctx, og); err != nil {
			return errs.Invalidf("orgs", "unknown organization")
		}
	}
	orgIDs = slices.Compact(slices.Sorted(slices.Values(orgIDs)))
	return o.store.Tx(ctx, func(tx store.Tx) error {
		if err := tx.NodeOrgs.Set(ctx, id, orgIDs); err != nil {
			return err
		}
		return audit(tx).Append(ctx, securitylog.Event{
			Action: auditNodeOpen, ActorID: by.User.ID, KeyID: by.KeyID,
			TargetKind: "node", TargetID: id, Detail: fmt.Sprintf("%d orgs", len(orgIDs)),
		})
	})
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

// RemoveNode (server admin) removes a worker that has no envs left. Move them
// first.
func (o *Orchestrator) RemoveNode(ctx context.Context, by *Principal, id string) error {
	if err := nodeAdmin(by); err != nil {
		return err
	}
	n, err := o.workerNode(ctx, id)
	if err != nil {
		return err
	}
	es, err := o.store.Environments.ListByNode(ctx, id)
	if err != nil {
		return err
	}
	if len(es) > 0 {
		return errs.Conflictf("%s still runs %d envs. Move them first, or forget the node if it is gone for good.", n.Name, len(es))
	}
	return o.dropNode(ctx, by, n, auditNodeRemove, false)
}

// ForgetPreview (server admin) is the envs that are lost if the node is forgotten.
func (o *Orchestrator) ForgetPreview(ctx context.Context, id string) ([]NodeEnv, error) {
	if _, err := o.workerNode(ctx, id); err != nil {
		return nil, err
	}
	return o.nodeEnvs(ctx, id)
}

// ForgetNode (server admin) drops a node that is gone for good: its certs are
// revoked and its links cut at once; its envs become lost until redeployed.
// confirm must be the node's name.
func (o *Orchestrator) ForgetNode(ctx context.Context, by *Principal, id, confirm string) error {
	if err := nodeAdmin(by); err != nil {
		return err
	}
	n, err := o.workerNode(ctx, id)
	if err != nil {
		return err
	}
	if confirm != n.Name {
		return errs.Invalidf("confirm", "type %s to confirm", n.Name)
	}
	return o.dropNode(ctx, by, n, auditNodeForget, true)
}

// LostEnv is an env whose node was forgotten.
type LostEnv struct {
	ID, Path, Node string // Node is the forgotten node's name
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
			vs, err := o.volumes.List(ctx, volume.Scope{Kind: "env", ID: e.ID})
			if err != nil {
				return nil, err
			}
			out = append(out, LostEnv{ID: e.ID, Path: e.Path, Node: f.Name, Volumes: len(vs), Tiles: e.Tiles})
		}
	}
	return out, nil
}

// RestoreLostEnv (server admin) puts a lost env on a node and queues a
// deploy of each of its tiles. Volumes start empty; restore their backups
// from the volume once the env is up.
func (o *Orchestrator) RestoreLostEnv(ctx context.Context, by *Principal, envID, toNode string) ([]Job, error) {
	if err := nodeAdmin(by); err != nil {
		return nil, err
	}
	e, err := o.store.Environments.Get(ctx, envID)
	if err != nil {
		return nil, err
	}
	fs, err := o.store.ForgottenNodes.List(ctx)
	if err != nil {
		return nil, err
	}
	if !slices.ContainsFunc(fs, func(f store.ForgottenNode) bool { return f.ID == e.NodeID }) {
		return nil, errs.Conflictf("That env is not lost.")
	}
	to, err := o.nodeLeaf().Get(ctx, toNode)
	if err != nil {
		return nil, err
	}
	if to.Status != node.StatusOnline {
		return nil, errs.Conflictf("node %s is %s", to.Name, to.Status)
	}
	if err := o.store.Environments.SetNode(ctx, envID, to.ID); err != nil {
		return nil, err
	}
	if err := o.store.Tx(ctx, func(tx store.Tx) error {
		return audit(tx).Append(ctx, securitylog.Event{
			Action: auditEnvRestore, ActorID: by.User.ID, KeyID: by.KeyID,
			TargetKind: "env", TargetID: envID, Detail: "redeploy on " + to.Name,
		})
	}); err != nil {
		return nil, err
	}
	ts, err := o.tiles.List(ctx, envID)
	if err != nil {
		return nil, err
	}
	var jobs []Job
	for _, t := range deployOrder(ts) {
		j, err := o.Deploy(ctx, t.ID)
		if err != nil {
			return jobs, err
		}
		jobs = append(jobs, j)
	}
	return jobs, nil
}

// PendingNode is a machine that called in with a join token.
type PendingNode struct {
	ID, Name  string
	Addresses []string
	Facts     worker.Facts
}

// PendingNodeByName (server admin) is the pending node of that name, or
// ErrNotFound while the machine has not called in.
func (o *Orchestrator) PendingNodeByName(ctx context.Context, name string) (PendingNode, error) {
	ps, err := o.PendingJoins(ctx)
	if err != nil {
		return PendingNode{}, err
	}
	for _, p := range ps {
		if p.Name != name {
			continue
		}
		out := PendingNode{ID: p.NodeID, Name: p.Name}
		_ = json.Unmarshal(p.Facts, &out.Facts)
		for _, i := range out.Facts.Interfaces {
			out.Addresses = append(out.Addresses, i.Prefixes...)
		}
		return out, nil
	}
	return PendingNode{}, errs.ErrNotFound
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

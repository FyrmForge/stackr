package service

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/FyrmForge/stackr/internal/exits"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/securitylog"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Egress model: the egress tag means traffic may leave from the node's own
// IP. A node without it has a default exit ladder of its own (scope "node")
// and nothing of its env traffic goes out its uplink. One resolver serves
// the mesh, the placement view and the map.

const (
	scopeNode     = "node" // exit_ladders scope_kind of a node's default exit
	auditNodeExit = "node.exit"
	maxNodeExits  = 3
)

func hasEgress(n store.Node) bool { return slices.Contains(node.Tags(n), node.TagEgress) }

// scopeLadder is the stored ladder of one scope, node ids in order. Rungs of
// org, stack, env and tile scopes name placements: each is its node, and a
// placement with no node is dropped.
func scopeLadder(ctx context.Context, st *store.Store, kind, id string) ([]string, error) {
	rs, err := st.ExitLadders.List(ctx, kind, id)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range rs {
		t := r.TargetID
		if kind != scopeNode {
			if t, err = placementNode(ctx, st.Placements, t); err != nil {
				return nil, err
			} else if t == "" {
				continue
			}
		}
		out = append(out, t)
	}
	return out, nil
}

// resolveExits is exits.Resolve for something running on node n: the node's
// own default exit counts only when n lacks egress, OwnRung is n, and rungs
// whose node lacks egress are dropped. own is true when n leaves by its own
// uplink: it has egress and the ladder is empty or only itself. A ladder
// whose rungs were all dropped is not own: it is no internet (no silent
// fallback to the uplink). The returned Nodes are empty for own.
func resolveExits(ctx context.Context, st *store.Store, lv exits.Levels, n store.Node) (exits.Resolution, bool, error) {
	egress := hasEgress(n)
	if !egress {
		var err error
		if lv.Node, err = scopeLadder(ctx, st, scopeNode, n.ID); err != nil {
			return exits.Resolution{}, false, err
		}
	}
	res := exits.Resolve(lv, defaultExitIDs(ctx, st))
	raw := len(res.Nodes)
	var kept []string
	for _, id := range res.Nodes {
		if id == OwnRung {
			id = n.ID
		}
		if slices.Contains(kept, id) {
			continue
		}
		x := n
		if id != n.ID {
			var err error
			if x, err = st.Nodes.Get(ctx, id); errors.Is(err, errs.ErrNotFound) {
				continue
			} else if err != nil {
				return exits.Resolution{}, false, err
			}
		}
		if hasEgress(x) {
			kept = append(kept, id)
		}
	}
	own := egress && (raw == 0 || len(kept) == 1 && kept[0] == n.ID)
	if own {
		kept = nil
	}
	res.Nodes = kept
	return res, own, nil
}

// envExits is the env's exit ladder as routing uses it: env, stack, org, the
// node's default exit (only when the node lacks egress), the cluster default.
// A node without egress never lists itself; nothing left = no internet. Nil
// ladder with own=true = the node's own uplink.
func envExits(ctx context.Context, st *store.Store, e store.Environment, orgID string, n store.Node) (ladder []string, own bool, err error) {
	var lv exits.Levels
	if lv.Env, err = scopeLadder(ctx, st, "env", e.ID); err != nil {
		return nil, false, err
	}
	if lv.Stack, err = scopeLadder(ctx, st, "stack", e.StackID); err != nil {
		return nil, false, err
	}
	if lv.Org, err = scopeLadder(ctx, st, "org", orgID); err != nil {
		return nil, false, err
	}
	res, own, err := resolveExits(ctx, st, lv, n)
	return res.Nodes, own, err
}

// SetNodeExit (server admin) sets the node's default exit ladder, names in
// and out like ExitSet. Every rung must be tagged egress and not the node
// itself; at most three. None clears it, which a node without egress refuses:
// its traffic would have nowhere to go.
func (o *Orchestrator) SetNodeExit(ctx context.Context, by *Principal, ref string, refs []string) (NodeOut, error) {
	n, err := o.setNodeExit(ctx, by, ref, refs)
	if err != nil {
		return NodeOut{}, err
	}
	return o.nodeOut(ctx, n)
}

func (o *Orchestrator) setNodeExit(ctx context.Context, by *Principal, ref string, refs []string) (store.Node, error) {
	if err := nodeAdmin(by); err != nil {
		return store.Node{}, err
	}
	n, err := o.nodeByRef(ctx, ref)
	if err != nil {
		return n, err
	}
	if n.Status == node.StatusPending {
		return n, errs.ErrNotFound
	}
	return n, o.setNodeLadder(ctx, by, n, refs)
}

// setNodeLadder is SetNodeExit's checks and write for a node already found.
func (o *Orchestrator) setNodeLadder(ctx context.Context, by *Principal, n store.Node, refs []string) error {
	if len(refs) > maxNodeExits {
		return errs.Invalidf("exits", "at most %d exits", maxNodeExits)
	}
	if len(refs) == 0 && !hasEgress(n) {
		return errs.Conflictf("Pick a default exit for %s first.", n.Name)
	}
	ids := make([]string, 0, len(refs))
	for _, r := range refs {
		x, err := o.nodeByRef(ctx, r)
		if err != nil {
			return err
		}
		switch {
		case x.ID == n.ID:
			return errs.Invalidf("exits", "%s cannot be its own exit", n.Name)
		case slices.Contains(ids, x.ID):
			return errs.Invalidf("exits", "an exit is listed twice")
		case x.Status == node.StatusPending || !hasEgress(x):
			return errs.Invalidf("exits", "%s is not tagged egress", x.Name)
		}
		ids = append(ids, x.ID)
	}
	err := o.store.Tx(ctx, func(tx store.Tx) error {
		if err := tx.ExitLadders.Set(ctx, scopeNode, n.ID, ids); err != nil {
			return err
		}
		return audit(tx).Append(ctx, securitylog.Event{
			Action: auditNodeExit, ActorID: by.User.ID, KeyID: by.KeyID,
			TargetKind: "node", TargetID: n.ID, Detail: strings.Join(refs, ","),
		})
	})
	if err == nil {
		o.kickMesh()
	}
	return err
}

// NodeExitView is what the node form needs: the ladder now, the nodes it may
// name and the closest one.
type NodeExitView struct {
	Ladder    []string        // exit names, in order
	Options   []PlacementNode // other nodes tagged egress
	Suggested string          // name of the egress node with the lowest ping from this node, "" = unknown
}

// NodeExits is the node's default exit ladder and its choices.
func (o *Orchestrator) NodeExits(ctx context.Context, id string) (NodeExitView, error) {
	n, err := o.nodeLeaf().Get(ctx, id)
	if err != nil {
		return NodeExitView{}, err
	}
	var v NodeExitView
	ids, err := scopeLadder(ctx, o.store, scopeNode, n.ID)
	if err != nil {
		return v, err
	}
	name := o.nodeNamer(ctx)
	for _, x := range ids {
		v.Ladder = append(v.Ladder, name(x))
	}
	all, err := o.nodeLeaf().List(ctx)
	if err != nil {
		return v, err
	}
	var best float64
	pings := o.nodePings(n.ID)
	for _, x := range all {
		if x.ID == n.ID || x.Status == node.StatusPending || !hasEgress(x) {
			continue
		}
		v.Options = append(v.Options, PlacementNode{ID: x.ID, Name: x.Name, Controller: x.Role == node.Controller,
			Ingress: slices.Contains(node.Tags(x), node.TagIngress), Egress: true})
		if rtt := pings[transitOf(x).String()]; rtt > 0 && (best == 0 || rtt < best) {
			best, v.Suggested = rtt, x.Name
		}
	}
	return v, nil
}

// nodePings is the round trip in ms this node measured to each peer, by
// transit address; empty when the node has not reported.
func (o *Orchestrator) nodePings(id string) map[string]float64 {
	out := map[string]float64{}
	if o.mesh == nil {
		return out
	}
	if f, ok := o.mesh.factsOf(id); ok {
		for a, p := range f.Ping {
			out[a] = p.RTTms
		}
	}
	return out
}

// SaveNodeTags sets the node's default exit ladder and then its tags, so the
// web form is one call. A node that keeps egress has no default exit.
func (o *Orchestrator) SaveNodeTags(ctx context.Context, by *Principal, id string, tags, exitRefs []string) error {
	if err := nodeAdmin(by); err != nil {
		return err
	}
	n, err := o.nodeLeaf().Get(ctx, id)
	if err != nil {
		return err
	}
	if n.Status == node.StatusPending {
		return errs.ErrNotFound
	}
	if !slices.Contains(tags, node.TagEgress) {
		if err := o.setNodeLadder(ctx, by, n, exitRefs); err != nil {
			return err
		}
	}
	return o.SetNodeTags(ctx, by, id, tags)
}

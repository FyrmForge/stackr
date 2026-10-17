package service

import (
	"context"
	"fmt"
	"slices"

	"github.com/FyrmForge/stackr/internal/authz"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/serverconfig"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/planfile"
)

// placementsLive fills the nodes, placements and, for each repoint the file
// asks for, the move checks and the envs it takes along. f is nil for an
// export, which asks for no repoint.
func (o *Orchestrator) placementsLive(ctx context.Context, f *serverconfig.File, live *serverconfig.Live) error {
	ns, err := o.store.Nodes.List(ctx)
	if err != nil {
		return err
	}
	names := map[string]string{} // node id -> name, forgotten ones too
	defIngress, err := o.DefaultIngress(ctx)
	if err != nil {
		return err
	}
	defExit := o.DefaultExit(ctx)
	for _, n := range ns {
		names[n.ID] = n.Name
		nl := serverconfig.NodeLive{
			ID: n.ID, Name: n.Name, Tags: node.Tags(n),
			Pending: n.Status == node.StatusPending,
			Online:  n.Role == node.Controller || n.Status == node.StatusOnline,
			Locked:  map[string]string{},
		}
		// what SetNodeTags refuses to drop
		if n.ID == defIngress {
			nl.Locked[node.TagIngress] = "it is the default ingress; set another default ingress first"
		}
		if n.ID == defExit {
			nl.Locked[node.TagEgress] = "it is the default exit; set another default exit first"
		}
		live.Nodes = append(live.Nodes, nl)
	}
	fs, err := o.store.ForgottenNodes.List(ctx)
	if err != nil {
		return err
	}
	for _, n := range fs {
		names[n.ID] = n.Name
		live.Nodes = append(live.Nodes, serverconfig.NodeLive{ID: n.ID, Name: n.Name, Forgotten: true})
	}
	ps, err := o.store.Placements.List(ctx)
	if err != nil {
		return err
	}
	pins, err := o.placementPins(ctx)
	if err != nil {
		return err
	}
	slugs := map[string]string{}
	orgs, err := o.orgs.ListAll(ctx)
	if err != nil {
		return err
	}
	for _, og := range orgs {
		slugs[og.ID] = og.Slug
	}
	for _, p := range ps {
		pl := serverconfig.PlacementLive{ID: p.ID, Name: p.Name, Node: names[p.NodeID], OpenToAll: p.OpenToAll, Used: pins[p.ID]}
		if p.NodeID != "" && pl.Node == "" {
			pl.Node = p.NodeID // a node gone without a trace: name it by id
		}
		if !p.OpenToAll {
			ids, err := o.store.PlacementOrgs.OrgIDs(ctx, p.ID)
			if err != nil {
				return err
			}
			for _, id := range ids {
				pl.Orgs = append(pl.Orgs, slugs[id])
			}
			slices.Sort(pl.Orgs)
		}
		es, err := o.store.Environments.ListByPlacement(ctx, p.ID)
		if err != nil {
			return err
		}
		pl.Used += len(es)
		live.Placements = append(live.Placements, pl)
	}
	if f == nil {
		return nil
	}
	return o.repointsLive(ctx, f, live)
}

// placementPins counts the org and stack pins on each placement.
func (o *Orchestrator) placementPins(ctx context.Context) (map[string]int, error) {
	n := map[string]int{}
	orgs, err := o.orgs.ListAll(ctx)
	if err != nil {
		return nil, err
	}
	for _, og := range orgs {
		if og.PlacementID != nil {
			n[*og.PlacementID]++
		}
		ss, err := o.store.Stacks.ListByOrg(ctx, og.ID)
		if err != nil {
			return nil, err
		}
		for _, s := range ss {
			if s.PlacementID != nil {
				n[*s.PlacementID]++
			}
		}
	}
	return n, nil
}

// repointsLive asks the move flow about each repoint the file names: a
// placement with a node that the file points at another joined node. A plan
// that cannot be made reads as a failed check, so the plan is blocked with
// the reason instead of the whole diff failing.
func (o *Orchestrator) repointsLive(ctx context.Context, f *serverconfig.File, live *serverconfig.Live) error {
	live.Moves = map[string][]planfile.Check{}
	live.MoveEnvs = map[string][]string{}
	for name, fp := range f.Placements {
		i := slices.IndexFunc(live.Placements, func(p serverconfig.PlacementLive) bool { return p.Name == name })
		if i < 0 || fp.Node == "" || live.Placements[i].Node == "" || live.Placements[i].Node == fp.Node {
			continue
		}
		if !slices.ContainsFunc(live.Nodes, func(n serverconfig.NodeLive) bool { return n.Name == fp.Node && !n.Pending && !n.Forgotten }) {
			continue
		}
		key := serverconfig.MoveKey(name, fp.Node)
		plan, err := o.PlanPlacementMove(ctx, name, fp.Node)
		if err != nil {
			live.Moves[key] = []planfile.Check{{Name: "plan", Detail: err.Error()}}
			continue
		}
		live.Moves[key] = plan.Checks
		for _, e := range plan.Envs {
			live.MoveEnvs[key] = append(live.MoveEnvs[key], e.Path)
		}
	}
	return nil
}

// by is who the walk acts as: the approver, or for an auto-apply a stand-in
// admin (the plan was already cleared to apply itself).
func (w *serverWalk) by() (*Principal, error) {
	if w.approver != "" {
		return w.o.SessionPrincipal(w.ctx, w.approver)
	}
	// The security log reads the actor as "serverconfig".
	const actor = "serverconfig"
	return &Principal{User: User{ID: actor}, Access: authz.User{ID: actor, Admin: true, Active: true}}, nil
}

func (w *serverWalk) nodeID(name string) (string, error) {
	i := slices.IndexFunc(w.live.Nodes, func(n serverconfig.NodeLive) bool { return n.Name == name && !n.Pending && !n.Forgotten })
	if i < 0 {
		return "", fmt.Errorf("no node named %s", name)
	}
	return w.live.Nodes[i].ID, nil
}

func (w *serverWalk) placementID(name string) (string, error) {
	i := slices.IndexFunc(w.live.Placements, func(p serverconfig.PlacementLive) bool { return p.Name == name })
	if i < 0 {
		return "", fmt.Errorf("no placement named %s", name)
	}
	return w.live.Placements[i].ID, nil
}

// joined says the node has joined (it is a node of the live cluster).
func (w *serverWalk) joined(name string) bool {
	_, err := w.nodeID(name)
	return err == nil
}

// placements walks the node tags, then the placements: new ones, access, a
// first node, and last the repoints. A repoint starts its move job and does
// not wait: the job is kept out of the redeploys this apply awaits, so the
// apply ends while the envs move (the placement's move status tells).
func (w *serverWalk) placements() error {
	rows := w.pick("node-tags", "placement", "placement-open", "placement-node", "placement-move")
	if len(rows) == 0 {
		return nil
	}
	by, err := w.by()
	if err != nil {
		return err
	}
	for _, kind := range []string{"node-tags", "placement", "placement-open", "placement-node", "placement-move"} {
		for _, c := range rows {
			if c.Kind != kind {
				continue
			}
			w.say(c)
			if err := w.placementRow(by, c); err != nil {
				return fmt.Errorf("%s %s: %w", c.Kind, c.Tile, err)
			}
		}
	}
	return nil
}

func (w *serverWalk) placementRow(by *Principal, c serverconfig.Change) error {
	switch c.Kind {
	case "node-tags":
		id, err := w.nodeID(c.Tile)
		if err != nil {
			return err
		}
		return w.o.SetNodeTags(w.ctx, by, id, w.f.Nodes[c.Tile].Tags)
	case "placement":
		fp := w.f.Placements[c.Tile]
		all, orgs := fp.Access()
		spec := PlacementSpec{Name: c.Tile, OpenToAll: all, Orgs: orgs}
		if w.joined(fp.Node) { // a node that has not joined: the placement waits without one
			spec.Node = fp.Node
		}
		_, err := w.o.CreatePlacement(w.ctx, by, spec)
		return err
	case "placement-open":
		all, orgs := w.f.Placements[c.Tile].Access()
		return w.o.SetPlacementOpen(w.ctx, by, c.Tile, all, orgs)
	case "placement-node":
		pid, err := w.placementID(c.Tile)
		if err != nil {
			return err
		}
		nid, err := w.nodeID(c.New)
		if err != nil {
			return err
		}
		return w.o.placementNodeSet(w.ctx, by, pid, nid)
	default: // placement-move
		// The move job is not awaited: a context with no redeploy list keeps it
		// out of the wait at the end of the apply.
		_, err := w.o.MovePlacement(context.WithValue(w.ctx, redeploysKey{}, nil), by, c.Tile, c.New, c.Tile, false)
		return err
	}
}

func (w *serverWalk) deletePlacement(name string) error {
	by, err := w.by()
	if err != nil {
		return err
	}
	return w.o.DeletePlacement(w.ctx, by, name)
}

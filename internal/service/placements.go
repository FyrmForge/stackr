package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/worker"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/securitylog"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/planfile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Placements: a named place envs belong to, standing on one node at a time
// and repointed by an admin. Access is per placement (open to all, or to a
// list of orgs); node names are for admins only.

// DefaultPlacement is the reserved placement an env lands on when nothing is
// pinned. It can be repointed, never deleted.
const DefaultPlacement = "default"

// Placement is one placement as an admin sees it.
type Placement struct {
	ID, Name      string
	NodeID        string   // "" = no node yet
	NodeName      string   // admin views only
	Tags          []string // the node's tags
	Public        bool     // the node has a public IP
	Online        bool
	OpenToAll     bool
	Orgs          []string // org slugs, when not open to all
	Envs          int
	Default       bool
	Moving        *PlacementMoveStatus
	NodeForgotten bool
}

// PlacementSpec is what an admin asks for.
type PlacementSpec struct {
	Name, Node string // Node is a node name; "" = none yet
	OpenToAll  bool
	Orgs       []string // slugs
}

// PlacementOffer is one row of the dev list; never names a node.
type PlacementOffer struct {
	Name    string
	Tags    []string
	Public  bool
	Online  bool
	HasNode bool
	Usable  bool
	Why     string // "" when usable: "no node yet", "offline"
}

// Needs is what an env asks of its placement's node.
type Needs = planfile.Needs

// Placement actions in the security log.
const (
	auditPlacementNew    = "placement.create"
	auditPlacementOpen   = "placement.open"
	auditPlacementDelete = "placement.delete"
)

// placementRow finds a placement by the name a caller typed.
func (o *Orchestrator) placementRow(ctx context.Context, name string) (store.Placement, error) {
	p, err := o.store.Placements.GetByName(ctx, strings.TrimSpace(name))
	if errors.Is(err, errs.ErrNotFound) {
		return p, errs.Invalidf("placement", "no placement named %q", name)
	}
	return p, err
}

// placementName is the name of a stored placement id; an unknown id comes
// back as is.
func (o *Orchestrator) placementName(ctx context.Context, id string) string {
	if p, err := o.store.Placements.Get(ctx, id); err == nil {
		return p.Name
	}
	return id
}

// PlacementName is the name of a placement id (an env's PlacementID), for
// views that list envs; an unknown id comes back as is.
func (o *Orchestrator) PlacementName(ctx context.Context, id string) string {
	return o.placementName(ctx, id)
}

// nodeState is the facts about a placement's node the views share: the node
// row (zero when the placement has none or its node was forgotten), whether
// the placement has a node it can use, and whether that node answers.
type nodeState struct {
	Node      store.Node
	HasNode   bool // a node row stands behind the placement
	Online    bool
	Forgotten bool
}

func (o *Orchestrator) nodeStateOf(ctx context.Context, p store.Placement) (nodeState, error) {
	var st nodeState
	if p.NodeID == "" {
		return st, nil
	}
	n, err := o.store.Nodes.Get(ctx, p.NodeID)
	if errors.Is(err, errs.ErrNotFound) {
		fs, ferr := o.store.ForgottenNodes.List(ctx)
		st.Forgotten = slices.ContainsFunc(fs, func(f store.ForgottenNode) bool { return f.ID == p.NodeID })
		return st, ferr
	}
	if err != nil {
		return st, err
	}
	st.Node, st.HasNode = n, n.Status != node.StatusPending
	st.Online = st.HasNode && (n.Role == node.Controller || n.Status == node.StatusOnline)
	return st, nil
}

// nodePublic is whether a node reported a public address. Only what the
// panel already holds is read; nothing is asked of the box.
func (o *Orchestrator) nodePublic(ctx context.Context, n store.Node) bool {
	var f *worker.Facts
	if n.Role == node.Controller {
		f = o.controllerFacts(ctx, n)
	} else if o.mesh != nil {
		if got, ok := o.mesh.factsOf(n.ID); ok {
			f = &got
		}
	}
	return f != nil && len(publicAddrs(*f)) > 0
}

// placementOpenTo reports whether the org may place things on the placement.
func (o *Orchestrator) placementOpenTo(ctx context.Context, placementID, orgID string) (bool, error) {
	p, err := o.store.Placements.Get(ctx, placementID)
	if err != nil {
		return false, err
	}
	if p.OpenToAll {
		return true, nil
	}
	return o.store.PlacementOrgs.Has(ctx, placementID, orgID)
}

// nodeOpenToOrg reports whether any placement open to the org stands on the
// node. Admins skip it; the legacy node-addressed verbs (MoveEnv) ask it.
func (o *Orchestrator) nodeOpenToOrg(ctx context.Context, nodeID, orgID string) (bool, error) {
	ps, err := o.store.Placements.ListByNode(ctx, nodeID)
	if err != nil {
		return false, err
	}
	for _, p := range ps {
		if ok, err := o.placementOpenTo(ctx, p.ID, orgID); err != nil || ok {
			return ok, err
		}
	}
	return false, nil
}

// offers returns the placement by name, or errs unless it is open to the
// org. A server admin may use any.
func (o *Orchestrator) offers(ctx context.Context, by *Principal, orgID, name string) (store.Placement, error) {
	admin := by != nil && by.Access.Admin
	p, err := o.store.Placements.GetByName(ctx, strings.TrimSpace(name))
	switch {
	case errors.Is(err, errs.ErrNotFound) && !admin: // no oracle for names a member cannot see
		return p, errs.Invalidf("placement", "%s is not open to this organization", strings.TrimSpace(name))
	case err != nil:
		return o.placementRow(ctx, name)
	case admin:
		return p, nil
	}
	if ok, err := o.placementOpenTo(ctx, p.ID, orgID); err != nil {
		return p, err
	} else if !ok {
		return p, errs.Invalidf("placement", "%s is not open to this organization", p.Name)
	}
	return p, nil
}

// sortedPlacements is every placement, default first, then by name.
func (o *Orchestrator) sortedPlacements(ctx context.Context) ([]store.Placement, error) {
	ps, err := o.store.Placements.List(ctx)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(ps, func(a, b store.Placement) int {
		if (a.Name == DefaultPlacement) != (b.Name == DefaultPlacement) {
			if a.Name == DefaultPlacement {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
	return ps, nil
}

// placementOut is the admin view of one placement.
func (o *Orchestrator) placementOut(ctx context.Context, p store.Placement) (Placement, error) {
	st, err := o.nodeStateOf(ctx, p)
	if err != nil {
		return Placement{}, err
	}
	out := Placement{
		ID: p.ID, Name: p.Name, NodeID: p.NodeID, OpenToAll: p.OpenToAll,
		Default: p.Name == DefaultPlacement, Online: st.Online, NodeForgotten: st.Forgotten,
		Tags: []string{}, Orgs: []string{},
	}
	if st.HasNode {
		out.NodeName, out.Tags, out.Public = st.Node.Name, node.Tags(st.Node), o.nodePublic(ctx, st.Node)
	} else if st.Forgotten {
		fs, _ := o.store.ForgottenNodes.List(ctx)
		for _, f := range fs {
			if f.ID == p.NodeID {
				out.NodeName = f.Name
			}
		}
	}
	if !p.OpenToAll {
		ids, err := o.store.PlacementOrgs.OrgIDs(ctx, p.ID)
		if err != nil {
			return out, err
		}
		out.Orgs = o.orgSlugs(ctx, ids)
	}
	es, err := o.store.Environments.ListByPlacement(ctx, p.ID)
	if err != nil {
		return out, err
	}
	out.Envs = len(es)
	mv, err := o.PlacementMove(ctx, p.Name)
	switch {
	case err != nil:
		return out, err
	case mv != nil && mv.State != "done":
		out.Moving = mv
	}
	return out, nil
}

// Placements lists every placement, default first (admin).
func (o *Orchestrator) Placements(ctx context.Context) ([]Placement, error) {
	ps, err := o.sortedPlacements(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Placement, 0, len(ps))
	for _, p := range ps {
		v, err := o.placementOut(ctx, p)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// PlacementByName is one placement (admin).
func (o *Orchestrator) PlacementByName(ctx context.Context, name string) (Placement, error) {
	p, err := o.store.Placements.GetByName(ctx, name)
	if err != nil {
		return Placement{}, err
	}
	return o.placementOut(ctx, p)
}

// OrgPlacements is the placements open to the org, for its members: what a
// dev may pin to. A row never names a node.
func (o *Orchestrator) OrgPlacements(ctx context.Context, orgID string) ([]PlacementOffer, error) {
	ps, err := o.sortedPlacements(ctx)
	if err != nil {
		return nil, err
	}
	out := []PlacementOffer{}
	for _, p := range ps {
		if ok, err := o.placementOpenTo(ctx, p.ID, orgID); err != nil {
			return nil, err
		} else if !ok {
			continue
		}
		st, err := o.nodeStateOf(ctx, p)
		if err != nil {
			return nil, err
		}
		of := PlacementOffer{Name: p.Name, Tags: []string{}, HasNode: st.HasNode, Online: st.Online}
		if st.HasNode {
			of.Tags, of.Public = node.Tags(st.Node), o.nodePublic(ctx, st.Node)
		}
		switch {
		case !st.HasNode:
			of.Why = "no node yet"
		case !st.Online:
			of.Why = "offline"
		default:
			of.Usable = true
		}
		out = append(out, of)
	}
	return out, nil
}

// CreatePlacement (admin) makes a placement, on a node or none yet.
func (o *Orchestrator) CreatePlacement(ctx context.Context, by *Principal, s PlacementSpec) (Placement, error) {
	if err := nodeAdmin(by); err != nil {
		return Placement{}, err
	}
	name := strings.TrimSpace(s.Name)
	if !joinNameRE.MatchString(name) {
		return Placement{}, errs.Invalidf("name", "use lowercase letters, digits and dashes, up to 32 characters")
	}
	p := store.Placement{ID: uuid.NewString(), Name: name, OpenToAll: s.OpenToAll, CreatedAt: time.Now().UTC()}
	if s.Node != "" {
		n, err := o.nodeByRef(ctx, s.Node)
		if err != nil || n.Status == node.StatusPending {
			return Placement{}, errs.Invalidf("node", "no node named %q", s.Node)
		}
		p.NodeID = n.ID
	}
	orgIDs, err := o.orgIDsOf(ctx, s.Orgs)
	if err != nil {
		return Placement{}, err
	}
	if s.OpenToAll {
		orgIDs = nil
	}
	err = o.store.Tx(ctx, func(tx store.Tx) error {
		if err := tx.Placements.Create(ctx, p); err != nil {
			if _, ok := errs.IsConflict(err); ok {
				return errs.Conflictf("A placement named %q already exists.", name)
			}
			return err
		}
		if err := tx.PlacementOrgs.Set(ctx, p.ID, orgIDs); err != nil {
			return err
		}
		return o.auditPlacement(ctx, tx, by, auditPlacementNew, "placement", p.ID, name)
	})
	if err != nil {
		return Placement{}, err
	}
	return o.placementOut(ctx, p)
}

// orgIDsOf turns org slugs into ids, sorted and without repeats.
func (o *Orchestrator) orgIDsOf(ctx context.Context, slugs []string) ([]string, error) {
	var ids []string
	for _, sl := range slugs {
		og, err := o.store.Orgs.GetBySlug(ctx, sl)
		if errors.Is(err, errs.ErrNotFound) {
			return nil, errs.Invalidf("orgs", "no organization %q", sl)
		} else if err != nil {
			return nil, err
		}
		ids = append(ids, og.ID)
	}
	return slices.Compact(slices.Sorted(slices.Values(ids))), nil
}

// SetPlacementOpen (admin) opens a placement to all orgs or to the listed
// ones. Envs already on it stay.
func (o *Orchestrator) SetPlacementOpen(ctx context.Context, by *Principal, name string, all bool, orgSlugs []string) error {
	if err := nodeAdmin(by); err != nil {
		return err
	}
	p, err := o.placementRow(ctx, name)
	if err != nil {
		return err
	}
	ids, err := o.orgIDsOf(ctx, orgSlugs)
	if err != nil {
		return err
	}
	if all {
		ids = nil
	}
	return o.store.Tx(ctx, func(tx store.Tx) error {
		if err := tx.Placements.SetOpen(ctx, p.ID, all); err != nil {
			return err
		}
		if err := tx.PlacementOrgs.Set(ctx, p.ID, ids); err != nil {
			return err
		}
		detail := "open to all orgs"
		if !all {
			detail = fmt.Sprintf("open to %d orgs", len(ids))
		}
		return o.auditPlacement(ctx, tx, by, auditPlacementOpen, "placement", p.ID, p.Name+": "+detail)
	})
}

// DeletePlacement (admin) deletes a placement no pin, env, ingress pick or
// exit rung references. The default placement is never deleted.
func (o *Orchestrator) DeletePlacement(ctx context.Context, by *Principal, name string) error {
	if err := nodeAdmin(by); err != nil {
		return err
	}
	p, err := o.placementRow(ctx, name)
	if err != nil {
		return err
	}
	if p.Name == DefaultPlacement {
		return errs.Refusedf("The default placement cannot be deleted; repoint it instead.")
	}
	if why, err := o.placementUse(ctx, p); err != nil {
		return err
	} else if why != "" {
		return errs.Conflictf("%s is still used by %s.", p.Name, why)
	}
	return o.store.Tx(ctx, func(tx store.Tx) error {
		if err := tx.Placements.Delete(ctx, p.ID); err != nil {
			return err
		}
		return o.auditPlacement(ctx, tx, by, auditPlacementDelete, "placement", p.ID, p.Name)
	})
}

// placementUse words the first thing that references the placement, "" when
// nothing does.
func (o *Orchestrator) placementUse(ctx context.Context, p store.Placement) (string, error) {
	es, err := o.store.Environments.ListByPlacement(ctx, p.ID)
	if err != nil {
		return "", err
	}
	if len(es) > 0 {
		return fmt.Sprintf("%d envs", len(es)), nil
	}
	ogs, err := o.orgs.ListAll(ctx)
	if err != nil {
		return "", err
	}
	for _, og := range ogs {
		if og.PlacementID != nil && *og.PlacementID == p.ID {
			return "the pin of org " + og.Slug, nil
		}
		sts, err := o.store.Stacks.ListByOrg(ctx, og.ID)
		if err != nil {
			return "", err
		}
		for _, st := range sts {
			if st.PlacementID != nil && *st.PlacementID == p.ID {
				return "the pin of stack " + og.Slug + "/" + st.Slug, nil
			}
		}
	}
	picks, err := o.store.IngressPicks.ListByPlacement(ctx, p.ID)
	if err != nil {
		return "", err
	}
	if len(picks) > 0 {
		return "the ingress of " + o.scopePath(ctx, picks[0].ScopeKind, picks[0].ScopeID), nil
	}
	rungs, err := o.store.ExitLadders.ListByTarget(ctx, p.ID)
	if err != nil {
		return "", err
	}
	if len(rungs) > 0 {
		return "an exit of " + o.scopePath(ctx, rungs[0].ScopeKind, rungs[0].ScopeID), nil
	}
	if mv, err := o.store.PlacementMoves.Latest(ctx, p.ID); err == nil && mv.State == "running" {
		return "a running move", nil
	}
	return "", nil
}

// envNeeds is what an env asks of its placement's node: storage once it has
// a volume or a managed tile.
func (o *Orchestrator) envNeeds(ctx context.Context, envID string) (Needs, error) {
	vs, err := o.store.Volumes.ListByScope(ctx, "env", envID)
	if err != nil {
		return Needs{}, err
	}
	if len(vs) > 0 {
		return Needs{Storage: true}, nil
	}
	ts, err := o.store.Tiles.ListByEnv(ctx, envID)
	if err != nil {
		return Needs{}, err
	}
	return Needs{Storage: slices.ContainsFunc(ts, func(t store.Tile) bool { return t.Kind == tile.Managed })}, nil
}

// unfit is why the placement's node does not fit n, "" when it fits or has
// no node yet (the plan warns about that, it does not refuse).
func (o *Orchestrator) unfit(ctx context.Context, p store.Placement, n Needs) (string, error) {
	st, err := o.nodeStateOf(ctx, p)
	if err != nil || !st.HasNode {
		return "", err
	}
	return strings.Join(planfile.Fits(node.Tags(st.Node), n), ", "), nil
}

// placementNode is the node a stored placement id stands on: "" when the
// placement has no node, OwnRung and "" come back as is, and an id that is
// no placement is "" too.
func placementNode(ctx context.Context, ps store.PlacementStore, id string) (string, error) {
	if id == "" || id == OwnRung {
		return id, nil
	}
	p, err := ps.Get(ctx, id)
	if errors.Is(err, errs.ErrNotFound) {
		return "", nil
	}
	return p.NodeID, err
}

// nodeTargets are the ids an exit rung or ingress pick may name a node by:
// the node id (node scope) and every placement on it.
func nodeTargets(ctx context.Context, ps store.PlacementStore, nodeID string) ([]string, error) {
	on, err := ps.ListByNode(ctx, nodeID)
	out := []string{nodeID}
	for _, p := range on {
		out = append(out, p.ID)
	}
	return out, err
}

// picksOfNode is every ingress pick that names a placement on the node.
func (o *Orchestrator) picksOfNode(ctx context.Context, nodeID string) ([]store.IngressPick, error) {
	targets, err := nodeTargets(ctx, o.store.Placements, nodeID)
	if err != nil {
		return nil, err
	}
	var out []store.IngressPick
	for _, t := range targets {
		ps, err := o.store.IngressPicks.ListByPlacement(ctx, t)
		if err != nil {
			return nil, err
		}
		out = append(out, ps...)
	}
	return out, nil
}

// adoptDefault gives the default placement the controller when it has no
// node: a database migrated before the controller row existed.
func adoptDefault(ctx context.Context, st *store.Store, ctlID string) error {
	p, err := st.Placements.GetByName(ctx, DefaultPlacement)
	if err != nil || p.NodeID != "" {
		return err
	}
	return st.Placements.SetNode(ctx, p.ID, ctlID)
}

func (o *Orchestrator) auditPlacement(ctx context.Context, tx store.Tx, by *Principal, action, kind, id, detail string) error {
	if by == nil {
		return nil
	}
	return audit(tx).Append(ctx, securitylog.Event{
		Action: action, ActorID: by.User.ID, KeyID: by.KeyID,
		TargetKind: kind, TargetID: id, Detail: detail,
	})
}

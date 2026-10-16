package service

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
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

// Placements lists every placement (admin).
func (o *Orchestrator) Placements(ctx context.Context) ([]Placement, error) {
	return nil, errs.ErrNotBuilt
}

// PlacementByName is one placement (admin).
func (o *Orchestrator) PlacementByName(ctx context.Context, name string) (Placement, error) {
	return Placement{}, errs.ErrNotBuilt
}

// OrgPlacements is the placements open to the org (members).
func (o *Orchestrator) OrgPlacements(ctx context.Context, orgID string) ([]PlacementOffer, error) {
	return nil, errs.ErrNotBuilt
}

// CreatePlacement (admin) makes a placement, on a node or none yet.
func (o *Orchestrator) CreatePlacement(ctx context.Context, by *Principal, s PlacementSpec) (Placement, error) {
	return Placement{}, errs.ErrNotBuilt
}

// SetPlacementOpen (admin) opens a placement to all orgs or to the listed ones.
func (o *Orchestrator) SetPlacementOpen(ctx context.Context, by *Principal, name string, all bool, orgSlugs []string) error {
	return errs.ErrNotBuilt
}

// DeletePlacement (admin) deletes a placement no pin or env references.
func (o *Orchestrator) DeletePlacement(ctx context.Context, by *Principal, name string) error {
	return errs.ErrNotBuilt
}

func (o *Orchestrator) placementOpenTo(ctx context.Context, placementID, orgID string) (bool, error) { //nolint:unused // lane A
	return false, errs.ErrNotBuilt
}

func (o *Orchestrator) envNeeds(ctx context.Context, envID string) (Needs, error) { //nolint:unused // lane A
	return Needs{}, errs.ErrNotBuilt
}

// ponytail: W0 shim until lane A. Pins, ingress picks and exit rungs name
// placements in the store; the verbs and readers around them still speak node
// ids. These map between the two, so behaviour stays as before.

// placementNode is the node a stored id stands on. An id that is not a
// placement (a node id written before lane A, or OwnRung) comes back as is;
// a placement with no node is "".
func placementNode(ctx context.Context, ps store.PlacementStore, id string) (string, error) {
	if id == "" || id == OwnRung {
		return id, nil
	}
	p, err := ps.Get(ctx, id)
	if errors.Is(err, errs.ErrNotFound) {
		return id, nil
	}
	return p.NodeID, err
}

// pinNode is placementNode for a pin column: nil, and a placement with no
// node, are no pin.
func (o *Orchestrator) pinNode(ctx context.Context, pin *string) (*string, error) {
	if pin == nil {
		return nil, nil
	}
	n, err := placementNode(ctx, o.store.Placements, *pin)
	if err != nil || n == "" {
		return nil, err
	}
	return &n, nil
}

// ingressPick is the scope's ingress pick as a node id ("" = inherit).
func (o *Orchestrator) ingressPick(ctx context.Context, kind, id string) (string, error) {
	pick, err := o.store.IngressPicks.Get(ctx, kind, id)
	if err != nil {
		return "", err
	}
	return placementNode(ctx, o.store.Placements, pick)
}

// nodePlacement is the placement a node id is stored as: the controller's is
// default; a worker's is the placement on it, made and named after the node
// when there is none.
func nodePlacement(ctx context.Context, t store.Tables, nodeID string) (string, error) {
	n, err := t.Nodes.Get(ctx, nodeID)
	if err != nil {
		return "", err
	}
	if n.Role == node.Controller {
		p, err := t.Placements.GetByName(ctx, DefaultPlacement)
		return p.ID, err
	}
	ps, err := t.Placements.ListByNode(ctx, nodeID)
	if err != nil {
		return "", err
	}
	if i := slices.IndexFunc(ps, func(p store.Placement) bool { return p.Name == n.Name }); i >= 0 {
		return ps[i].ID, nil
	}
	if len(ps) > 0 {
		return ps[0].ID, nil
	}
	p := store.Placement{ID: uuid.NewString(), Name: n.Name, NodeID: n.ID, OpenToAll: true, CreatedAt: time.Now().UTC()}
	return p.ID, t.Placements.Create(ctx, p)
}

// nodeTargets are the ids a pick or rung may name a node by: the node id
// and every placement on it.
func nodeTargets(ctx context.Context, ps store.PlacementStore, nodeID string) ([]string, error) {
	on, err := ps.ListByNode(ctx, nodeID)
	out := []string{nodeID}
	for _, p := range on {
		out = append(out, p.ID)
	}
	return out, err
}

// picksOfNode is every ingress pick that names the node, by id or placement.
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

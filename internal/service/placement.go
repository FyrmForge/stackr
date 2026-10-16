package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/exits"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/planfile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Placement: where an org, stack, env or tile runs (a pin for new envs, the
// env's own placement once made), which placement its public traffic comes in
// on, and the ordered exits its outbound traffic leaves by. A scope with no
// value of its own inherits from the one above it. A placement names one node
// at a time; members see placement names, never node names (D3).
//
// Only placements open to the org are offered (a server admin may use any).
// Verbs here do not check the caller's right: the route's middleware does
// (org.write, stack.write, env.write), except the ones for admins.

// Placement scope kinds, top down.
const (
	ScopeOrg   = "org"
	ScopeStack = "stack"
	ScopeEnv   = "env"
	ScopeTile  = "tile"
)

// OwnRung in an exit ladder is "the node the app runs on": a rung that is
// the own uplink, resolved per env when the mesh is built.
const OwnRung = "own"

// Exit rung states, as the ladder on the node reports them.
const (
	RungInUse   = "in use"
	RungStandby = "standby"
	RungDown    = "down"
)

// PlacementNode is a node offered to the org (OfferedNodes, admin and move
// code), or, in a PlacementView, a placement offered to the org: ID and Name
// are then both the placement name, never the node's.
type PlacementNode struct {
	ID, Name   string
	Controller bool
	Ingress    bool   // tagged ingress
	Egress     bool   // tagged egress
	Why        string // a placement that does not fit the scope: "needs storage", "no node yet", "offline"
}

// PlacementRung is one rung of an exit ladder. NodeID is the placement name
// (OwnRung for the own uplink). Up and Down are the whole ladder (placement
// names) after moving this rung; nil = it is at that end.
type PlacementRung struct {
	NodeID   string
	Name     string
	Own      bool
	State    string // RungInUse, RungStandby, RungDown, "" = not known
	Detail   string // "no answer for 40 s"
	Up, Down []string
}

// PlacementEnvs is the existing envs under a stack or org that a pin does not
// move: Count of them are not on the pin, on Where (their placement names).
// Each is moved by pinning the env itself (SetPin on the env).
type PlacementEnvs struct {
	Count int
	Where string
}

// PlacementView is everything the Placement screen of one scope shows.
type PlacementView struct {
	Kind, ID string
	Nodes    []PlacementNode

	// Runs on. Pin is the placement name picked here ("" = inherit); for an
	// env it is the placement it belongs to. PinFrom is the "From stack: x."
	// hint.
	Pin     string
	PinName string
	PinFrom string
	Stay    PlacementEnvs // org and stack only

	// Ingress: the pick here (a placement name), and where an inherited one
	// comes from. IngressName is the placement in effect, or "cluster default".
	Ingress     string
	IngressName string
	IngressFrom string

	// Exits. Inherited: the ladder shown is the one above's. From names it.
	Rungs     []PlacementRung
	Inherited bool
	ExitsFrom string
	NoNet     bool   // every rung is down and the own node is not one
	NoNetWhy  string // the ladder's reason
	Spare     []PlacementNode
	OwnListed bool
}

type scopeRef struct{ kind, id, name string }

// chain is the scope and the ones above it, nearest first, with the org id.
func (o *Orchestrator) chain(ctx context.Context, kind, id string) ([]scopeRef, store.Org, error) {
	var out []scopeRef
	var og store.Org
	switch kind {
	case ScopeTile:
		t, err := o.store.Tiles.Get(ctx, id)
		if err != nil {
			return nil, og, err
		}
		out = append(out, scopeRef{ScopeTile, t.ID, t.Name})
		kind, id = ScopeEnv, t.EnvironmentID
		fallthrough
	case ScopeEnv:
		if kind == ScopeEnv {
			e, err := o.store.Environments.Get(ctx, id)
			if err != nil {
				return nil, og, err
			}
			out = append(out, scopeRef{ScopeEnv, e.ID, e.Name})
			kind, id = ScopeStack, e.StackID
		}
		fallthrough
	case ScopeStack:
		if kind == ScopeStack {
			s, err := o.store.Stacks.Get(ctx, id)
			if err != nil {
				return nil, og, err
			}
			out = append(out, scopeRef{ScopeStack, s.ID, s.Name})
			id = s.OrgID
		}
		fallthrough
	case ScopeOrg:
		g, err := o.store.Orgs.Get(ctx, id)
		if err != nil {
			return nil, og, err
		}
		out = append(out, scopeRef{ScopeOrg, g.ID, g.Name})
		return out, g, nil
	}
	return nil, og, errs.Invalidf("scope", "unknown scope %q", kind)
}

// OfferedNodes are the nodes the org may place things on: the controller and
// every confirmed node that holds a placement open to the org, controller
// first. Admin and move code; members see OfferedPlacements.
func (o *Orchestrator) OfferedNodes(ctx context.Context, orgID string) ([]PlacementNode, error) {
	return o.offered(ctx, orgID, false)
}

// offered lists the offered nodes; anywhere (a server admin) skips the
// open-to-the-org check.
func (o *Orchestrator) offered(ctx context.Context, orgID string, anywhere bool) ([]PlacementNode, error) {
	all, err := o.nodeLeaf().List(ctx)
	if err != nil {
		return nil, err
	}
	var out []PlacementNode
	for _, n := range all {
		if n.Status == node.StatusPending {
			continue
		}
		if !anywhere {
			if ok, err := o.nodeOpenToOrg(ctx, n.ID, orgID); err != nil {
				return nil, err
			} else if !ok {
				continue
			}
		}
		tags := node.Tags(n)
		out = append(out, PlacementNode{
			ID: n.ID, Name: n.Name, Controller: n.Role == node.Controller,
			Ingress: slices.Contains(tags, node.TagIngress), Egress: slices.Contains(tags, node.TagEgress),
		})
	}
	slices.SortStableFunc(out, func(a, b PlacementNode) int {
		if a.Controller != b.Controller {
			if a.Controller {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out, nil
}

// OfferedPlacements are the placements open to the org, default first. need
// is what the scope asks of a node: a placement that does not fit says why.
func (o *Orchestrator) OfferedPlacements(ctx context.Context, orgID string, need Needs) ([]PlacementNode, error) {
	ps, err := o.sortedPlacements(ctx)
	if err != nil {
		return nil, err
	}
	var out []PlacementNode
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
		pn := PlacementNode{ID: p.Name, Name: p.Name}
		switch {
		case !st.HasNode:
			pn.Why = "no node yet"
		default:
			tags := node.Tags(st.Node)
			pn.Ingress, pn.Egress = slices.Contains(tags, node.TagIngress), slices.Contains(tags, node.TagEgress)
			pn.Why = strings.Join(planfile.Fits(tags, need), ", ")
			if pn.Why == "" && !st.Online {
				pn.Why = "offline"
			}
		}
		out = append(out, pn)
	}
	return out, nil
}

// scopeNeeds is what the envs a scope covers ask of a node.
func (o *Orchestrator) scopeNeeds(ctx context.Context, kind, id string) (Needs, error) {
	var n Needs
	switch kind {
	case ScopeEnv:
		return o.envNeeds(ctx, id)
	case ScopeOrg, ScopeStack:
		es, err := o.envsUnder(ctx, kind, id)
		if err != nil {
			return n, err
		}
		for _, e := range es {
			en, err := o.envNeeds(ctx, e.ID)
			if err != nil {
				return n, err
			}
			n.Storage = n.Storage || en.Storage
		}
	}
	return n, nil
}

// Placement is the screen for one scope.
func (o *Orchestrator) Placement(ctx context.Context, kind, id string) (PlacementView, error) {
	v := PlacementView{Kind: kind, ID: id}
	ch, og, err := o.chain(ctx, kind, id)
	if err != nil {
		return v, err
	}
	need, err := o.scopeNeeds(ctx, kind, id)
	if err != nil {
		return v, err
	}
	if v.Nodes, err = o.OfferedPlacements(ctx, og.ID, need); err != nil {
		return v, err
	}
	if err := o.pinView(ctx, &v, ch, og); err != nil {
		return v, err
	}
	if err := o.ingressView(ctx, &v, ch); err != nil {
		return v, err
	}
	return v, o.exitsView(ctx, &v, ch)
}

// ingressView fills the ingress part: the nearest pick wins; with none, an
// env enters on its own placement when that node is tagged ingress, else on
// the cluster default (D10).
func (o *Orchestrator) ingressView(ctx context.Context, v *PlacementView, ch []scopeRef) error {
	for i, s := range ch {
		pick, err := o.ingressPick(ctx, s.kind, s.id)
		if err != nil {
			return err
		}
		if pick == "" {
			continue
		}
		name := o.placementName(ctx, pick)
		if i == 0 {
			v.Ingress = name
		}
		v.IngressName = name
		if i > 0 {
			v.IngressFrom = fromHint(s, name)
		}
		return nil
	}
	v.IngressName = "cluster default"
	v.IngressFrom = "From default."
	if v.Kind == ScopeEnv || v.Kind == ScopeTile {
		envID := ch[len(ch)-3].id
		if v.Kind == ScopeEnv {
			envID = ch[0].id
		}
		e, err := o.store.Environments.Get(ctx, envID)
		if err != nil {
			return err
		}
		if n, err := o.store.Nodes.Get(ctx, e.NodeID); err == nil && slices.Contains(node.Tags(n), node.TagIngress) {
			v.IngressName, v.IngressFrom = o.placementName(ctx, e.PlacementID), "From the placement."
		}
	}
	return nil
}

func fromHint(s scopeRef, node string) string {
	return "From " + s.kind + ": " + node + "."
}

// nodeNamer maps a node id to its name, the id itself when unknown.
func (o *Orchestrator) nodeNamer(ctx context.Context) func(string) string {
	names := map[string]string{}
	if ns, err := o.nodeLeaf().List(ctx); err == nil {
		for _, n := range ns {
			names[n.ID] = n.Name
		}
	}
	return func(id string) string {
		if n, ok := names[id]; ok {
			return n
		}
		return id
	}
}

func (o *Orchestrator) pinView(ctx context.Context, v *PlacementView, ch []scopeRef, og store.Org) error {
	if v.Kind == ScopeTile {
		return nil
	}
	pinName := func(pin *string) string {
		if pin == nil {
			return ""
		}
		return o.placementName(ctx, *pin)
	}
	ogPin := pinName(og.PlacementID)
	var stackPin string
	switch v.Kind {
	case ScopeOrg:
		v.Pin = ogPin
	case ScopeStack:
		st, err := o.store.Stacks.Get(ctx, v.ID)
		if err != nil {
			return err
		}
		v.Pin = pinName(st.PlacementID)
		if ogPin != "" {
			v.PinFrom = fromHint(scopeRef{kind: ScopeOrg}, ogPin)
		}
	case ScopeEnv:
		e, err := o.store.Environments.Get(ctx, v.ID)
		if err != nil {
			return err
		}
		st, err := o.store.Stacks.Get(ctx, e.StackID)
		if err != nil {
			return err
		}
		v.Pin = o.placementName(ctx, e.PlacementID)
		stackPin = pinName(st.PlacementID)
		switch {
		case stackPin != "" && stackPin == v.Pin:
			v.PinFrom = fromHint(scopeRef{kind: ScopeStack}, v.Pin)
		case ogPin != "" && ogPin == v.Pin:
			v.PinFrom = fromHint(scopeRef{kind: ScopeOrg}, v.Pin)
		}
	}
	v.PinName = v.Pin
	if v.Kind == ScopeEnv {
		return nil
	}
	// The effective pin new envs get, and the envs that are not on it.
	eff := v.Pin
	if eff == "" && v.Kind == ScopeStack {
		eff = ogPin
	}
	if eff == "" {
		return nil
	}
	v.PinName = eff
	envs, err := o.envsUnder(ctx, v.Kind, v.ID)
	if err != nil {
		return err
	}
	where := map[string]bool{}
	for _, e := range envs {
		if n := o.placementName(ctx, e.PlacementID); n != eff {
			v.Stay.Count++
			where[n] = true
		}
	}
	names := make([]string, 0, len(where))
	for n := range where {
		names = append(names, n)
	}
	slices.Sort(names)
	v.Stay.Where = strings.Join(names, ", ")
	return nil
}

// envsUnder is every env of the org or stack.
func (o *Orchestrator) envsUnder(ctx context.Context, kind, id string) ([]store.Environment, error) {
	sts := []store.Stack{{ID: id}}
	if kind == ScopeOrg {
		var err error
		if sts, err = o.store.Stacks.ListByOrg(ctx, id); err != nil {
			return nil, err
		}
	}
	var out []store.Environment
	for _, st := range sts {
		es, err := o.store.Environments.ListByStack(ctx, st.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, es...)
	}
	return out, nil
}

// ladders reads the stored ladders of a chain, nearest first, as node ids
// (what the mesh and the resolver use).
func (o *Orchestrator) ladders(ctx context.Context, ch []scopeRef) ([][]string, error) {
	out := make([][]string, len(ch))
	for i, s := range ch {
		l, err := scopeLadder(ctx, o.store, s.kind, s.id)
		if err != nil {
			return nil, err
		}
		out[i] = l
	}
	return out, nil
}

// rawLadders are the stored ladders of a chain, nearest first, as placement
// ids (OwnRung stays), whether or not the placement has a node.
func (o *Orchestrator) rawLadders(ctx context.Context, ch []scopeRef) ([][]string, error) {
	out := make([][]string, len(ch))
	for i, s := range ch {
		rs, err := o.store.ExitLadders.List(ctx, s.kind, s.id)
		if err != nil {
			return nil, err
		}
		for _, r := range rs {
			out[i] = append(out[i], r.TargetID)
		}
	}
	return out, nil
}

func (o *Orchestrator) exitsView(ctx context.Context, v *PlacementView, ch []scopeRef) error {
	ls, err := o.rawLadders(ctx, ch)
	if err != nil {
		return err
	}
	shown, from := ls[0], -1
	if len(shown) == 0 {
		v.Inherited = true
		for i := 1; i < len(ls); i++ {
			if len(ls[i]) > 0 {
				shown, from = ls[i], i
				break
			}
		}
	}
	name := func(id string) string {
		if id == OwnRung {
			return OwnRung
		}
		return o.placementName(ctx, id)
	}
	names := make([]string, len(shown))
	for i, id := range shown {
		names[i] = name(id)
	}
	if from > 0 {
		v.ExitsFrom = "From " + ch[from].kind + ": " + strings.Join(mapNames(names), ", ") + "."
	}
	states, why, down := o.rungStates(ctx, v.Kind, v.ID, shown)
	v.NoNetWhy = why
	for i, id := range shown {
		r := PlacementRung{NodeID: names[i], Own: id == OwnRung, Name: names[i]}
		if r.Own {
			r.Name = "own node (no exit)"
			v.OwnListed = true
		}
		if st, ok := states[id]; ok {
			r.State, r.Detail = st.state, st.detail
		}
		if !v.Inherited {
			r.Up, r.Down = swapped(names, i, i-1), swapped(names, i, i+1)
		}
		v.Rungs = append(v.Rungs, r)
	}
	v.NoNet = down && !v.OwnListed
	for _, n := range v.Nodes {
		if n.Egress && !slices.Contains(names, n.Name) {
			v.Spare = append(v.Spare, n)
		}
	}
	return nil
}

func mapNames(names []string) []string {
	out := slices.Clone(names)
	for i, n := range out {
		if n == OwnRung {
			out[i] = "own node"
		}
	}
	return out
}

func swapped(ids []string, i, j int) []string {
	if j < 0 || j >= len(ids) {
		return nil
	}
	out := slices.Clone(ids)
	out[i], out[j] = out[j], out[i]
	return out
}

type rungState struct{ state, detail string }

// rungStates is the live state of the rungs an env or tile uses, from the
// ladder on the env's node: the one in use, the others standby, or all down.
// Org and stack scopes cover many envs and show none. Unknown (no worker
// connection, the controller's own node, no ladder running) is an empty map.
func (o *Orchestrator) rungStates(ctx context.Context, kind, id string, ids []string) (map[string]rungState, string, bool) {
	envID := id
	if kind == ScopeTile {
		t, err := o.store.Tiles.Get(ctx, id)
		if err != nil {
			return nil, "", false
		}
		envID = t.EnvironmentID
	} else if kind != ScopeEnv {
		return nil, "", false
	}
	e, err := o.store.Environments.Get(ctx, envID)
	if err != nil || e.NodeID == "" || e.Subnet == "" {
		return nil, "", false
	}
	c, err := o.nodeExtras(e.NodeID)
	if err != nil || c == nil {
		return nil, "", false
	}
	info, err := c.MeshInfo(ctx)
	if err != nil {
		return nil, "", false
	}
	for _, l := range info.Ladders {
		if !slices.Contains(l.Sources, e.Subnet) {
			continue
		}
		st, out := l.State, map[string]rungState{}
		since := time.Since(st.Since).Round(time.Second)
		for _, rid := range ids {
			real := e.NodeID
			if rid != OwnRung {
				real, _ = placementNode(ctx, o.store.Placements, rid)
			}
			switch {
			case st.Down:
				out[rid] = rungState{RungDown, fmt.Sprintf("no answer for %d s", int(since.Seconds()))}
			case st.NodeID == real:
				out[rid] = rungState{RungInUse, ""}
			default:
				out[rid] = rungState{RungStandby, ""}
			}
		}
		return out, st.Reason, st.Down
	}
	return nil, "", false
}

// ladderNames is the ladder in effect at a scope as placement names (OwnRung
// stays): its own, else the nearest above.
func (o *Orchestrator) ladderNames(ctx context.Context, kind, id string) ([]string, error) {
	ch, _, err := o.chain(ctx, kind, id)
	if err != nil {
		return nil, err
	}
	ls, err := o.rawLadders(ctx, ch)
	if err != nil {
		return nil, err
	}
	for _, l := range ls {
		if len(l) > 0 {
			names := make([]string, len(l))
			for i, t := range l {
				names[i] = t
				if t != OwnRung {
					names[i] = o.placementName(ctx, t)
				}
			}
			return names, nil
		}
	}
	return nil, nil
}

// effectivePlacement is the placement a new env of the stack belongs to: the
// stack's pin, the org's, else default.
func (o *Orchestrator) effectivePlacement(ctx context.Context, stackID string) (store.Placement, error) {
	st, err := o.store.Stacks.Get(ctx, stackID)
	if err != nil {
		return store.Placement{}, err
	}
	pin := st.PlacementID
	if pin == nil {
		og, err := o.store.Orgs.Get(ctx, st.OrgID)
		if err != nil {
			return store.Placement{}, err
		}
		pin = og.PlacementID
	}
	if pin != nil {
		return o.store.Placements.Get(ctx, *pin)
	}
	return o.store.Placements.GetByName(ctx, DefaultPlacement)
}

// SetPin pins an org, stack or env to a placement by name ("" = no pin,
// inherit; an env then falls back to what its stack, org or default gives).
//
//   - Org and stack: new envs below it belong to the placement. Existing envs
//     on the same node as the placement follow it; the rest stay until the env
//     itself is pinned (the view lists them as Stay).
//   - Env: pinning it to a placement on another node moves it (D4). The move
//     is refused here with *MoveNeeded; the caller shows the plan and confirms
//     through MoveEnvToPlacement.
func (o *Orchestrator) SetPin(ctx context.Context, by *Principal, kind, id, name string) error {
	if kind != ScopeOrg && kind != ScopeStack && kind != ScopeEnv {
		return errs.Invalidf("scope", "only an org, a stack or an env is pinned")
	}
	_, og, err := o.chain(ctx, kind, id)
	if err != nil {
		return err
	}
	var p store.Placement
	if name != "" {
		if p, err = o.offers(ctx, by, og.ID, name); err != nil {
			return err
		}
	}
	if kind == ScopeEnv {
		return o.pinEnv(ctx, by, id, p, name == "")
	}
	if name != "" {
		if err := o.pinFits(ctx, kind, id, p); err != nil {
			return err
		}
	}
	err = o.store.Tx(ctx, func(tx store.Tx) error {
		var pin *string
		if name != "" {
			pin = &p.ID
		}
		if kind == ScopeOrg {
			og.PlacementID = pin
			if err := tx.Orgs.Update(ctx, og); err != nil {
				return err
			}
		} else {
			st, err := tx.Stacks.Get(ctx, id)
			if err != nil {
				return err
			}
			st.PlacementID = pin
			if err := tx.Stacks.Update(ctx, st); err != nil {
				return err
			}
		}
		return o.auditPlacement(ctx, tx, by, "placement.pin", kind, id, "placement "+name)
	})
	if err != nil {
		return err
	}
	if err := o.followPin(ctx, kind, id); err != nil {
		return err
	}
	o.kickMesh()
	return nil
}

// pinFits refuses a pin whose node cannot hold the envs it would take over:
// those under the scope that are not on the placement already.
func (o *Orchestrator) pinFits(ctx context.Context, kind, id string, p store.Placement) error {
	es, err := o.envsUnder(ctx, kind, id)
	if err != nil {
		return err
	}
	var need Needs
	for _, e := range es {
		if e.PlacementID == p.ID {
			continue
		}
		en, err := o.envNeeds(ctx, e.ID)
		if err != nil {
			return err
		}
		need.Storage = need.Storage || en.Storage
	}
	if why, err := o.unfit(ctx, p, need); err != nil {
		return err
	} else if why != "" {
		return errs.Invalidf("placement", "%s does not fit the envs here: %s", p.Name, why)
	}
	return nil
}

// followPin re-homes the envs under an org or stack whose effective
// placement changed to one on their own node. Moving to another node is not
// done here.
func (o *Orchestrator) followPin(ctx context.Context, kind, id string) error {
	es, err := o.envsUnder(ctx, kind, id)
	if err != nil {
		return err
	}
	for _, e := range es {
		eff, err := o.effectivePlacement(ctx, e.StackID)
		if err != nil {
			return err
		}
		if eff.ID != e.PlacementID && eff.NodeID == e.NodeID {
			if err := o.store.Environments.SetPlacement(ctx, e.ID, eff.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

// pinEnv is SetPin on an env. inherit pins it to what its stack, org or the
// default gives.
func (o *Orchestrator) pinEnv(ctx context.Context, by *Principal, envID string, p store.Placement, inherit bool) error {
	e, err := o.store.Environments.Get(ctx, envID)
	if err != nil {
		return err
	}
	if inherit {
		if p, err = o.effectivePlacement(ctx, e.StackID); err != nil {
			return err
		}
	}
	if e.PlacementID == p.ID {
		return nil
	}
	need, err := o.envNeeds(ctx, envID)
	if err != nil {
		return err
	}
	if why, err := o.unfit(ctx, p, need); err != nil {
		return err
	} else if why != "" {
		return errs.Invalidf("placement", "%s does not fit this env: %s", p.Name, why)
	}
	switch {
	case e.NodeID == p.NodeID: // same node: only the belonging changes
	case e.NodeID == "": // waiting for a node (D6): it takes the placement's
	case p.NodeID == "":
		return errs.Conflictf("%s has no node yet; %s would have nowhere to run.", p.Name, e.Slug)
	default:
		plan, err := o.PlanEnvPin(ctx, envID, p.Name)
		if err != nil {
			return err
		}
		return &MoveNeeded{Plan: plan}
	}
	err = o.store.Tx(ctx, func(tx store.Tx) error {
		if err := tx.Environments.SetPlacement(ctx, envID, p.ID); err != nil {
			return err
		}
		return o.auditPlacement(ctx, tx, by, "placement.pin", ScopeEnv, envID, "placement "+p.Name)
	})
	if err != nil {
		return err
	}
	if e.NodeID == "" && p.NodeID != "" {
		return o.placementNodeSet(ctx, by, p.ID, p.NodeID)
	}
	o.kickMesh()
	return nil
}

// SetIngress picks the placement whose node's Caddy serves a scope's public
// routes ("" = inherit). The placement must be open to the org and, once it
// has a node, that node must be tagged ingress.
func (o *Orchestrator) SetIngress(ctx context.Context, by *Principal, kind, id, name string) error {
	_, og, err := o.chain(ctx, kind, id)
	if err != nil {
		return err
	}
	var pick string
	if name != "" {
		p, err := o.offers(ctx, by, og.ID, name)
		if err != nil {
			return err
		}
		if why, err := o.unfit(ctx, p, Needs{Ingress: true}); err != nil {
			return err
		} else if why != "" {
			return errs.Invalidf("placement", "%s cannot be an ingress: %s", p.Name, why)
		}
		pick = p.ID
	}
	return o.store.Tx(ctx, func(tx store.Tx) error {
		if err := tx.IngressPicks.Set(ctx, kind, id, pick); err != nil {
			return err
		}
		return o.auditPlacement(ctx, tx, by, "placement.ingress", kind, id, "placement "+name)
	})
}

// SetExits replaces a scope's exit ladder: placement names top to bottom,
// OwnRung for the own node's uplink. None = inherit. Each placement must be
// open to the org and, once it has a node, that node must be tagged egress.
func (o *Orchestrator) SetExits(ctx context.Context, by *Principal, kind, id string, names []string) error {
	_, og, err := o.chain(ctx, kind, id)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	ids := make([]string, 0, len(names))
	for _, n := range names {
		if seen[n] {
			return errs.Invalidf("exits", "an exit is listed twice")
		}
		seen[n] = true
		if n == OwnRung {
			ids = append(ids, OwnRung)
			continue
		}
		p, err := o.offers(ctx, by, og.ID, n)
		if err != nil {
			return err
		}
		if why, err := o.unfit(ctx, p, Needs{Egress: true}); err != nil {
			return err
		} else if why != "" {
			return errs.Invalidf("exits", "%s cannot be an exit: %s", p.Name, why)
		}
		ids = append(ids, p.ID)
	}
	err = o.store.Tx(ctx, func(tx store.Tx) error {
		if err := tx.ExitLadders.Set(ctx, kind, id, ids); err != nil {
			return err
		}
		return o.auditPlacement(ctx, tx, by, "placement.exits", kind, id, strings.Join(names, ","))
	})
	if err == nil {
		o.kickMesh()
	}
	return err
}

// ExitsAdd appends a rung; ExitsRemove drops one. Both read the ladder in
// effect at the scope itself (an inherited ladder is copied first).
func (o *Orchestrator) ExitsAdd(ctx context.Context, by *Principal, kind, id, name string) error {
	cur, err := o.ladderNames(ctx, kind, id)
	if err != nil {
		return err
	}
	return o.SetExits(ctx, by, kind, id, append(cur, name))
}

func (o *Orchestrator) ExitsRemove(ctx context.Context, by *Principal, kind, id, name string) error {
	cur, err := o.ladderNames(ctx, kind, id)
	if err != nil {
		return err
	}
	return o.SetExits(ctx, by, kind, id, slices.DeleteFunc(cur, func(n string) bool { return n == name }))
}

// ResolveExits is the ladder a tile uses (tile, env, stack, org, its node), for
// callers that wire it (the mesh builds its own from env, stack and org).
func (o *Orchestrator) ResolveExits(ctx context.Context, tileID string) (exits.Resolution, error) {
	ch, _, err := o.chain(ctx, ScopeTile, tileID)
	if err != nil {
		return exits.Resolution{}, err
	}
	ls, err := o.ladders(ctx, ch)
	if err != nil {
		return exits.Resolution{}, err
	}
	e, err := o.store.Environments.Get(ctx, ch[1].id)
	if err != nil {
		return exits.Resolution{}, err
	}
	n, err := o.store.Nodes.Get(ctx, e.NodeID)
	if err != nil {
		return exits.Resolution{}, err
	}
	res, _, err := resolveExits(ctx, o.store, exits.Levels{Tile: ls[0], Env: ls[1], Stack: ls[2], Org: ls[3]}, n)
	return res, err
}

// ingressPick is the scope's own ingress pick as a placement id ("" = inherit).
func (o *Orchestrator) ingressPick(ctx context.Context, kind, id string) (string, error) {
	return o.store.IngressPicks.Get(ctx, kind, id)
}

// IngressFor is the ingress node id in effect for a tile (D10): the node of
// the nearest picked placement; else the env's own node when it is tagged
// ingress; else "" = the default ingress node.
func (o *Orchestrator) IngressFor(ctx context.Context, tileID string) (string, error) {
	ch, _, err := o.chain(ctx, ScopeTile, tileID)
	if err != nil {
		return "", err
	}
	for _, s := range ch {
		pick, err := o.ingressPick(ctx, s.kind, s.id)
		if err != nil {
			return "", err
		}
		if pick == "" {
			continue
		}
		if nid, err := placementNode(ctx, o.store.Placements, pick); err != nil {
			return "", err
		} else if nid != "" {
			if _, err := o.store.Nodes.Get(ctx, nid); err == nil {
				return nid, nil
			} else if !errors.Is(err, errs.ErrNotFound) {
				return "", err
			}
		}
	}
	e, err := o.store.Environments.Get(ctx, ch[1].id)
	if err != nil {
		return "", err
	}
	if n, err := o.store.Nodes.Get(ctx, e.NodeID); err == nil && slices.Contains(node.Tags(n), node.TagIngress) {
		return n.ID, nil
	}
	return "", nil
}

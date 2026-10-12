package service

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/exits"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/securitylog"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Placement: where an org, stack, env or tile runs (a pin for new envs, the
// env's own node once made), which node its public traffic comes in on, and
// the ordered exits its outbound traffic leaves by. A scope with no value
// of its own inherits from the one above it; the controller is the default
// ingress and a missing ladder means the own node's uplink.
//
// Only nodes opened to the org are offered (the controller always is).
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

// PlacementNode is a node offered to the org.
type PlacementNode struct {
	ID, Name   string
	Controller bool
	Ingress    bool // tagged ingress
	Egress     bool // tagged egress
}

// PlacementRung is one rung of an exit ladder. Up and Down are the whole
// ladder (node ids) after moving this rung; nil = it is at that end.
type PlacementRung struct {
	NodeID   string
	Name     string
	Own      bool
	State    string // RungInUse, RungStandby, RungDown, "" = not known
	Detail   string // "no answer for 40 s"
	Up, Down []string
}

// PlacementEnvs is the existing envs under a stack or org that a pin does not
// move: Count of them are not on the pin, on Where (their node names).
type PlacementEnvs struct {
	Count int
	Where string
}

// PlacementView is everything the Placement screen of one scope shows.
type PlacementView struct {
	Kind, ID string
	Nodes    []PlacementNode

	// Runs on. Pin is the node id picked here ("" = inherit); for an env it
	// is the node it runs on now. PinFrom is the "From stack: x." hint.
	Pin     string
	PinName string
	PinFrom string
	Stay    PlacementEnvs // org and stack only

	// Ingress: the pick here, and where an inherited one comes from.
	Ingress     string
	IngressName string // the node in effect
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
			kind, id = ScopeOrg, s.OrgID
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
// every confirmed node opened to the org, controller first.
func (o *Orchestrator) OfferedNodes(ctx context.Context, orgID string) ([]PlacementNode, error) {
	return o.offered(ctx, orgID, false)
}

// offered lists the offered nodes; anywhere (a server admin) skips the
// opened-to-the-org check.
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
			if ok, err := o.NodeOpenToOrg(ctx, n.ID, orgID); err != nil {
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
		switch {
		case a.Controller != b.Controller:
			if a.Controller {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out, nil
}

// Placement is the screen for one scope.
func (o *Orchestrator) Placement(ctx context.Context, kind, id string) (PlacementView, error) {
	v := PlacementView{Kind: kind, ID: id}
	ch, og, err := o.chain(ctx, kind, id)
	if err != nil {
		return v, err
	}
	if v.Nodes, err = o.OfferedNodes(ctx, og.ID); err != nil {
		return v, err
	}
	name := o.nodeNamer(ctx)
	ctl, err := o.controllerID(ctx)
	if err != nil {
		return v, err
	}
	if err := o.pinView(ctx, &v, ch, og, name); err != nil {
		return v, err
	}
	// Ingress: the nearest pick wins.
	for i, s := range ch {
		pick, err := o.store.IngressPicks.Get(ctx, s.kind, s.id)
		if err != nil {
			return v, err
		}
		if i == 0 {
			v.Ingress = pick
		}
		if pick != "" {
			v.IngressName = name(pick)
			if i > 0 {
				v.IngressFrom = fromHint(s, name(pick))
			}
			break
		}
	}
	if v.IngressName == "" {
		v.IngressName = name(ctl)
		if len(ch) > 1 || v.Ingress == "" {
			v.IngressFrom = "From default: " + name(ctl) + "."
		}
	}
	return v, o.exitsView(ctx, &v, ch, name)
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

func (o *Orchestrator) pinView(ctx context.Context, v *PlacementView, ch []scopeRef, og store.Org, name func(string) string) error {
	var stack *store.Stack
	switch v.Kind {
	case ScopeTile:
		return nil
	case ScopeOrg:
		if og.NodeID != nil {
			v.Pin = *og.NodeID
		}
	case ScopeStack:
		st, err := o.store.Stacks.Get(ctx, v.ID)
		if err != nil {
			return err
		}
		stack = &st
		if st.NodeID != nil {
			v.Pin = *st.NodeID
		}
		if og.NodeID != nil {
			v.PinFrom = fromHint(scopeRef{kind: ScopeOrg}, name(*og.NodeID))
		}
	case ScopeEnv:
		e, err := o.store.Environments.Get(ctx, v.ID)
		if err != nil {
			return err
		}
		v.Pin = e.NodeID
		st, err := o.store.Stacks.Get(ctx, e.StackID)
		if err != nil {
			return err
		}
		switch {
		case st.NodeID != nil && *st.NodeID == e.NodeID:
			v.PinFrom = fromHint(scopeRef{kind: ScopeStack}, name(e.NodeID))
		case og.NodeID != nil && *og.NodeID == e.NodeID:
			v.PinFrom = fromHint(scopeRef{kind: ScopeOrg}, name(e.NodeID))
		}
	}
	if v.Pin != "" {
		v.PinName = name(v.Pin)
	}
	if v.Kind == ScopeEnv {
		return nil
	}
	// The effective pin new envs get, and the envs that are not on it.
	eff := v.Pin
	if eff == "" && stack != nil && og.NodeID != nil {
		eff = *og.NodeID
	}
	if eff == "" {
		return nil
	}
	v.PinName = name(eff)
	envs, err := o.envsUnder(ctx, v.Kind, v.ID)
	if err != nil {
		return err
	}
	where := map[string]bool{}
	for _, e := range envs {
		if e.NodeID != eff {
			v.Stay.Count++
			where[name(e.NodeID)] = true
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

// ladders reads the stored ladders of a chain, nearest first.
func (o *Orchestrator) ladders(ctx context.Context, ch []scopeRef) ([][]string, error) {
	out := make([][]string, len(ch))
	for i, s := range ch {
		rs, err := o.store.ExitLadders.List(ctx, s.kind, s.id)
		if err != nil {
			return nil, err
		}
		for _, r := range rs {
			out[i] = append(out[i], r.NodeID)
		}
	}
	return out, nil
}

func (o *Orchestrator) exitsView(ctx context.Context, v *PlacementView, ch []scopeRef, name func(string) string) error {
	ls, err := o.ladders(ctx, ch)
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
	if from > 0 {
		v.ExitsFrom = "From " + ch[from].kind + ": " + strings.Join(mapNames(shown, name), ", ") + "."
	}
	states, why, down := o.rungStates(ctx, v.Kind, v.ID, name)
	v.NoNetWhy = why
	for i, id := range shown {
		r := PlacementRung{NodeID: id, Own: id == OwnRung, Name: name(id)}
		if r.Own {
			r.Name = "own node (no exit)"
			v.OwnListed = true
		}
		if st, ok := states[id]; ok {
			r.State, r.Detail = st.state, st.detail
		}
		if !v.Inherited {
			r.Up, r.Down = swapped(shown, i, i-1), swapped(shown, i, i+1)
		}
		v.Rungs = append(v.Rungs, r)
	}
	v.NoNet = down && !v.OwnListed
	for _, n := range v.Nodes {
		if n.Egress && !slices.Contains(shown, n.ID) {
			v.Spare = append(v.Spare, n)
		}
	}
	return nil
}

func mapNames(ids []string, name func(string) string) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = name(id)
		if id == OwnRung {
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
func (o *Orchestrator) rungStates(ctx context.Context, kind, id string, name func(string) string) (map[string]rungState, string, bool) {
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
		ids, _ := o.ladderIDs(ctx, kind, id)
		for _, rid := range ids {
			real := rid
			if rid == OwnRung {
				real = e.NodeID
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

// ladderIDs is the ladder in effect at a scope: its own, else the nearest above.
func (o *Orchestrator) ladderIDs(ctx context.Context, kind, id string) ([]string, error) {
	ch, _, err := o.chain(ctx, kind, id)
	if err != nil {
		return nil, err
	}
	ls, err := o.ladders(ctx, ch)
	if err != nil {
		return nil, err
	}
	for _, l := range ls {
		if len(l) > 0 {
			return l, nil
		}
	}
	return nil, nil
}

// offers errs unless nodeID is offered to the scope's org.
// A server admin may pin anywhere.
func (o *Orchestrator) offers(ctx context.Context, by *Principal, orgID, nodeID string) (PlacementNode, error) {
	ns, err := o.offered(ctx, orgID, by != nil && by.Access.Admin)
	if err != nil {
		return PlacementNode{}, err
	}
	for _, n := range ns {
		if n.ID == nodeID {
			return n, nil
		}
	}
	return PlacementNode{}, errs.Invalidf("node", "that node is not open to this organization")
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

// SetPin pins an org or stack: new envs below it start on nodeID ("" = no
// pin, inherit). It never moves an existing env (see MoveEnvsToPin).
func (o *Orchestrator) SetPin(ctx context.Context, by *Principal, kind, id, nodeID string) error {
	if kind != ScopeOrg && kind != ScopeStack {
		return errs.Invalidf("scope", "only an org or a stack is pinned; an env moves")
	}
	_, og, err := o.chain(ctx, kind, id)
	if err != nil {
		return err
	}
	if nodeID != "" {
		if _, err := o.offers(ctx, by, og.ID, nodeID); err != nil {
			return err
		}
	}
	var pin *string
	if nodeID != "" {
		pin = &nodeID
	}
	err = o.store.Tx(ctx, func(tx store.Tx) error {
		if kind == ScopeOrg {
			og.NodeID = pin
			if err := tx.Orgs.Update(ctx, og); err != nil {
				return err
			}
		} else {
			st, err := tx.Stacks.Get(ctx, id)
			if err != nil {
				return err
			}
			st.NodeID = pin
			if err := tx.Stacks.Update(ctx, st); err != nil {
				return err
			}
		}
		return o.auditPlacement(ctx, tx, by, "placement.pin", kind, id, "node "+nodeID)
	})
	if err == nil {
		o.kickMesh()
	}
	return err
}

// MoveEnvsToPin (server admin) moves every env under the org or stack that
// is not on the scope's pin to it, live, one job each. The jobs are
// returned; the first refusal stops the rest.
func (o *Orchestrator) MoveEnvsToPin(ctx context.Context, by *Principal, kind, id string) ([]Job, error) {
	v, err := o.Placement(ctx, kind, id)
	if err != nil {
		return nil, err
	}
	pin := v.Pin
	if pin == "" && kind == ScopeStack {
		_, og, err := o.chain(ctx, kind, id)
		if err != nil {
			return nil, err
		}
		if og.NodeID != nil {
			pin = *og.NodeID
		}
	}
	if pin == "" {
		return nil, errs.Conflictf("nothing is pinned here")
	}
	envs, err := o.envsUnder(ctx, kind, id)
	if err != nil {
		return nil, err
	}
	var jobs []Job
	for _, e := range envs {
		if e.NodeID == pin {
			continue
		}
		j, err := o.MoveEnv(ctx, by, e.ID, pin, true)
		if err != nil {
			return jobs, err
		}
		jobs = append(jobs, j)
	}
	return jobs, nil
}

// SetIngress picks the node whose Caddy serves a scope's public routes
// ("" = inherit). The node must be tagged ingress and open to the org.
func (o *Orchestrator) SetIngress(ctx context.Context, by *Principal, kind, id, nodeID string) error {
	_, og, err := o.chain(ctx, kind, id)
	if err != nil {
		return err
	}
	if nodeID != "" {
		n, err := o.offers(ctx, by, og.ID, nodeID)
		if err != nil {
			return err
		}
		if !n.Ingress {
			return errs.Invalidf("node", "%s is not tagged ingress", n.Name)
		}
	}
	return o.store.Tx(ctx, func(tx store.Tx) error {
		if err := tx.IngressPicks.Set(ctx, kind, id, nodeID); err != nil {
			return err
		}
		return o.auditPlacement(ctx, tx, by, "placement.ingress", kind, id, "node "+nodeID)
	})
}

// SetExits replaces a scope's exit ladder: nodeIDs top to bottom, OwnRung
// for the own node's uplink. None = inherit. Each node must be tagged egress
// and open to the org.
func (o *Orchestrator) SetExits(ctx context.Context, by *Principal, kind, id string, nodeIDs []string) error {
	_, og, err := o.chain(ctx, kind, id)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, n := range nodeIDs {
		if seen[n] {
			return errs.Invalidf("exits", "an exit is listed twice")
		}
		seen[n] = true
		if n == OwnRung {
			continue
		}
		p, err := o.offers(ctx, by, og.ID, n)
		if err != nil {
			return err
		}
		if !p.Egress {
			return errs.Invalidf("exits", "%s is not tagged egress", p.Name)
		}
	}
	err = o.store.Tx(ctx, func(tx store.Tx) error {
		if err := tx.ExitLadders.Set(ctx, kind, id, nodeIDs); err != nil {
			return err
		}
		return o.auditPlacement(ctx, tx, by, "placement.exits", kind, id, strings.Join(nodeIDs, ","))
	})
	if err == nil {
		o.kickMesh()
	}
	return err
}

// ExitsAdd appends a rung; ExitsRemove drops one. Both read the ladder in
// effect at the scope itself (an inherited ladder is copied first).
func (o *Orchestrator) ExitsAdd(ctx context.Context, by *Principal, kind, id, nodeID string) error {
	cur, err := o.ownLadder(ctx, kind, id)
	if err != nil {
		return err
	}
	return o.SetExits(ctx, by, kind, id, append(cur, nodeID))
}

func (o *Orchestrator) ExitsRemove(ctx context.Context, by *Principal, kind, id, nodeID string) error {
	cur, err := o.ownLadder(ctx, kind, id)
	if err != nil {
		return err
	}
	return o.SetExits(ctx, by, kind, id, slices.DeleteFunc(cur, func(n string) bool { return n == nodeID }))
}

// ownLadder is the scope's own ladder, or the inherited one to start from.
func (o *Orchestrator) ownLadder(ctx context.Context, kind, id string) ([]string, error) {
	return o.ladderIDs(ctx, kind, id)
}

// ResolveExits is the ladder a tile uses (tile, env, stack, org), for
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
	return exits.Resolve(exits.Levels{Tile: ls[0], Env: ls[1], Stack: ls[2], Org: ls[3]}, nil), nil
}

// IngressFor is the ingress node id in effect for a tile: the nearest pick
// up the chain, "" = the default ingress node.
func (o *Orchestrator) IngressFor(ctx context.Context, tileID string) (string, error) {
	ch, _, err := o.chain(ctx, ScopeTile, tileID)
	if err != nil {
		return "", err
	}
	for _, s := range ch {
		pick, err := o.store.IngressPicks.Get(ctx, s.kind, s.id)
		if err != nil || pick != "" {
			return pick, err
		}
	}
	return "", nil
}

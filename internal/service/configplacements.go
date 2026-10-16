package service

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/orgconfig"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/promote"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/planfile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Config files and placements: the org file and the stack file name a pin,
// an ingress and exits by placement name. This file gathers the facts their
// plans check (flows never read the store) and applies the changes through
// the same verbs the panel uses (SetPin, SetIngress, SetExits).

// placementNames is every placement id -> name.
func (o *Orchestrator) placementNames(ctx context.Context) (map[string]string, error) {
	ps, err := o.store.Placements.List(ctx)
	names := make(map[string]string, len(ps))
	for _, p := range ps {
		names[p.ID] = p.Name
	}
	return names, err
}

// placementFacts is the placements as org orgID sees them, by name: open to
// the org, has a node, node online, node tags.
func (o *Orchestrator) placementFacts(ctx context.Context, orgID string) (planfile.PlacementFacts, error) {
	f := planfile.PlacementFacts{Known: map[string]planfile.PlacementFact{}}
	ps, err := o.store.Placements.List(ctx)
	if err != nil {
		return f, err
	}
	for _, p := range ps {
		fact := planfile.PlacementFact{Open: p.OpenToAll}
		if !fact.Open {
			if fact.Open, err = o.store.PlacementOrgs.Has(ctx, p.ID, orgID); err != nil {
				return f, err
			}
		}
		if p.NodeID != "" {
			switch n, err := o.store.Nodes.Get(ctx, p.NodeID); {
			case err == nil:
				fact.HasNode = true
				fact.Online = n.Role == node.Controller || n.Status == node.StatusOnline
				fact.Tags = node.Tags(n)
			case !errors.Is(err, errs.ErrNotFound): // a forgotten node: the placement names it, nothing stands there
				return f, err
			}
		}
		f.Known[p.Name] = fact
	}
	return f, nil
}

// placingOf is the pin, ingress and exits set at one scope, by name. pin is
// the scope's pin id, nil for none.
func (o *Orchestrator) placingOf(ctx context.Context, kind, id string, pin *string, names map[string]string) (planfile.Placing, error) {
	name := func(x string) string {
		if n, ok := names[x]; ok {
			return n
		}
		return x // own
	}
	var p planfile.Placing
	if pin != nil {
		p.Pin = name(*pin)
	}
	pick, err := o.store.IngressPicks.Get(ctx, kind, id)
	if err != nil {
		return p, err
	}
	if pick != "" {
		p.Ingress = name(pick)
	}
	rungs, err := o.store.ExitLadders.List(ctx, kind, id)
	for _, r := range rungs {
		p.Exits = append(p.Exits, name(r.TargetID))
	}
	return p, err
}

// envPath is "stack/env", the key plans use for an env.
func envPath(st store.Stack, e store.Environment) string { return st.Slug + "/" + e.Slug }

// pinMoves is, per env path, the move pinning envs to placement would cause:
// the envs whose node would change (PlanEnvPin). Nothing is computed for a
// placement that is not declared or has no node; the plan reports those.
func (o *Orchestrator) pinMoves(ctx context.Context, facts planfile.PlacementFacts, placement string, envs map[string]store.Environment) (map[string]planfile.MoveFact, error) {
	if f, ok := facts.Known[placement]; !ok || !f.HasNode {
		return nil, nil
	}
	target, err := o.store.Placements.GetByName(ctx, placement)
	if err != nil {
		return nil, err
	}
	out := map[string]planfile.MoveFact{}
	for _, path := range slices.Sorted(maps.Keys(envs)) {
		e := envs[path]
		if e.PlacementID == target.ID {
			continue
		}
		pl, err := o.PlanEnvPin(ctx, e.ID, placement)
		if err != nil {
			return nil, err
		}
		if len(pl.Envs) == 0 { // same node: the env is re-homed, nothing moves
			continue
		}
		out[path] = planfile.MoveFact{From: pl.From, To: placement, Checks: pl.Checks, Downtime: pl.Envs[0].Downtime}
	}
	return out, nil
}

// movable are the stack's envs a pin can move: static ones, by path.
func (o *Orchestrator) movable(ctx context.Context, st store.Stack, skip ...string) (map[string]store.Environment, error) {
	es, err := o.envs.List(ctx, st.ID)
	out := map[string]store.Environment{}
	for _, e := range es {
		if e.Type != environment.Ephemeral && !slices.Contains(skip, e.Slug) {
			out[envPath(st, e)] = e
		}
	}
	return out, err
}

// orgPlacements fills what the org plan checks its pin, ingress and exits
// against; orgMoves adds what depends on the file.
func (o *Orchestrator) orgPlacements(ctx context.Context, live *orgconfig.Live) error {
	var err error
	if live.Placements, err = o.placementFacts(ctx, live.Org.ID); err != nil {
		return err
	}
	names, err := o.placementNames(ctx)
	if err != nil {
		return err
	}
	if live.Current, err = o.placingOf(ctx, ScopeOrg, live.Org.ID, live.Org.PlacementID, names); err != nil {
		return err
	}
	return nil
}

// orgMoves fills what depends on the parsed file, and only when it names a
// pin: EnvNeeds, what the envs the org pin covers ask of their node, and
// Moves, the envs the pin would move. A stack with its own pin is not covered.
func (o *Orchestrator) orgMoves(ctx context.Context, live *orgconfig.Live, f *orgconfig.File) error {
	live.Moves, live.EnvNeeds = nil, nil
	if f.Pin == "" {
		return nil
	}
	envs := map[string]store.Environment{}
	for _, sl := range live.Stacks {
		if sl.Stack.PlacementID != nil {
			continue
		}
		es, err := o.movable(ctx, sl.Stack)
		if err != nil {
			return err
		}
		maps.Copy(envs, es)
	}
	live.EnvNeeds = map[string]planfile.Needs{}
	for path, e := range envs {
		n, err := o.envNeeds(ctx, e.ID)
		if err != nil {
			return err
		}
		live.EnvNeeds[path] = n
	}
	if f.Pin == live.Current.Pin {
		return nil
	}
	var err error
	live.Moves, err = o.pinMoves(ctx, live.Placements, f.Pin, envs)
	return err
}

// stackPlacements is promote's Placements: the facts and lookups for the
// stack plan of one env.
func (o *Orchestrator) stackPlacements(ctx context.Context, envID string) (promote.PlacementLive, error) {
	var live promote.PlacementLive
	e, err := o.envs.Get(ctx, envID)
	if err != nil {
		return live, err
	}
	st, err := o.stacks.Get(ctx, e.StackID)
	if err != nil {
		return live, err
	}
	og, err := o.orgs.Get(ctx, st.OrgID)
	if err != nil {
		return live, err
	}
	names, err := o.placementNames(ctx)
	if err != nil {
		return live, err
	}
	if live.Placements, err = o.placementFacts(ctx, og.ID); err != nil {
		return live, err
	}
	if live.Stack, err = o.placingOf(ctx, ScopeStack, st.ID, st.PlacementID, names); err != nil {
		return live, err
	}
	if live.Env, err = o.placingOf(ctx, ScopeEnv, e.ID, &e.PlacementID, names); err != nil {
		return live, err
	}
	if e.PlacementID == "" {
		live.Env.Pin = ""
	}
	live.Inherit = DefaultPlacement
	switch {
	case live.Stack.Pin != "":
		live.Inherit = live.Stack.Pin
	case og.PlacementID != nil:
		live.Inherit = names[*og.PlacementID]
	}
	all, err := o.movable(ctx, st)
	if err != nil {
		return live, err
	}
	live.Needs = func(ctx context.Context) (map[string]planfile.Needs, error) {
		out := map[string]planfile.Needs{}
		for _, x := range all {
			n, err := o.envNeeds(ctx, x.ID)
			if err != nil {
				return nil, err
			}
			out[x.Slug] = n
		}
		return out, nil
	}
	live.Moves = func(ctx context.Context, scope, placement string, except []string) (map[string]planfile.MoveFact, error) {
		envs := map[string]store.Environment{envPath(st, e): e}
		if scope == ScopeStack {
			var err error
			if envs, err = o.movable(ctx, st, except...); err != nil {
				return nil, err
			}
		}
		return o.pinMoves(ctx, live.Placements, placement, envs)
	}
	live.Set = func(ctx context.Context, w promote.PlacementWrite) error { return o.setPlacing(ctx, st, w) }
	return live, nil
}

// setPlacing applies one planned stack-file change.
func (o *Orchestrator) setPlacing(ctx context.Context, st store.Stack, w promote.PlacementWrite) error {
	switch w.Field {
	case "pin":
		return o.setPin(ctx, st, w.Scope, w.ScopeID, w.Refs[0], w.Moving)
	case "ingress":
		return o.SetIngress(ctx, nil, w.Scope, w.ScopeID, w.Refs[0])
	case "exits":
		return o.SetExits(ctx, nil, w.Scope, w.ScopeID, w.Refs)
	}
	return nil
}

// setPin pins a scope and moves the envs the plan said would move (D4). The
// plan was approved, so each move is confirmed with the env's slug. A move is
// a queued job, not awaited.
// ponytail: SetPin may refuse with *MoveNeeded until the envs are on the
// placement; the second SetPin ignores that, since the moves are in flight.
func (o *Orchestrator) setPin(ctx context.Context, st store.Stack, kind, id, placement string, moving []string) error {
	// the moves act as the file: the security log reads "configfile"; no
	// admin rights, so the org's open-to check still holds
	by := &Principal{User: User{ID: "configfile"}}
	err := o.SetPin(ctx, nil, kind, id, placement)
	var need *MoveNeeded
	refused := errors.As(err, &need)
	if err != nil && !refused {
		return err
	}
	if kind == ScopeEnv {
		if !refused {
			return nil
		}
		e, err := o.envs.Get(ctx, id)
		if err != nil {
			return err
		}
		_, err = o.MoveEnvToPlacement(ctx, by, id, placement, e.Slug)
		return err
	}
	target, err := o.store.Placements.GetByName(ctx, placement)
	if err != nil {
		return err
	}
	for _, path := range moving {
		e, err := o.envByPath(ctx, st.OrgID, path)
		if err != nil {
			return err
		}
		if e.PlacementID == target.ID {
			continue
		}
		if _, err := o.MoveEnvToPlacement(ctx, by, e.ID, placement, e.Slug); err != nil {
			return err
		}
	}
	if !refused {
		return nil
	}
	if err = o.SetPin(ctx, nil, kind, id, placement); errors.As(err, &need) {
		return nil
	}
	return err
}

// envByPath is the env "stack/env" of the org.
func (o *Orchestrator) envByPath(ctx context.Context, orgID, path string) (store.Environment, error) {
	sl, es, _ := strings.Cut(path, "/")
	st, err := o.stacks.GetBySlug(ctx, orgID, sl)
	if err != nil {
		return store.Environment{}, err
	}
	return o.envs.GetBySlug(ctx, st.ID, es)
}

// applyOrgPlacing applies the org file's pin, ingress and exits changes. The
// moving envs are the ones the diff just computed.
func (o *Orchestrator) applyOrgPlacing(ctx context.Context, og store.Org, f *orgconfig.File, live orgconfig.Live, kind string) error {
	switch kind {
	case "pin":
		return o.setPin(ctx, store.Stack{OrgID: og.ID}, ScopeOrg, og.ID, f.Pin, slices.Sorted(maps.Keys(live.Moves)))
	case "ingress":
		return o.SetIngress(ctx, nil, ScopeOrg, og.ID, f.Ingress)
	case "exits":
		return o.SetExits(ctx, nil, ScopeOrg, og.ID, f.Exits)
	}
	return nil
}

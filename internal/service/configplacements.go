package service

import (
	"context"
	"errors"
	"io"
	"maps"
	"slices"

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

// stackFileNeeds is, per env slug, what the stack's file at its config
// branch's head asks of a node (volumes, a managed tile): a pin is checked
// against an env the file declares before its first deploy writes it down.
// No config repo: nil. A file that cannot be read refuses, as the stack
// plan does, rather than let a pin through unchecked.
func (o *Orchestrator) stackFileNeeds(ctx context.Context, og store.Org, st store.Stack) (map[string]planfile.Needs, error) {
	if st.ConfigRepo == "" {
		return nil, nil
	}
	_, sha, err := o.configHead(ctx, st)
	if err != nil {
		return nil, errs.Conflictf("stack %s: its file cannot be read to check the pin: %v", st.Slug, err)
	}
	data, fetch, err := o.stackFile(ctx, st, sha, io.Discard)
	var r *promote.Resolved
	if err == nil {
		r, err = promote.Load(data, fetch, og.Slug)
	}
	if err != nil {
		return nil, errs.Conflictf("stack %s: its file cannot be read to check the pin: %v", st.Slug, err)
	}
	out := make(map[string]planfile.Needs, len(r.Envs))
	for slug, e := range r.Envs {
		out[slug] = promote.FileNeeds(e)
	}
	return out, nil
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
	live.Moves, live.EnvNeeds, live.FileErrors = nil, nil, nil
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
	o.orgFileNeeds(ctx, live, f)
	if f.Pin == live.Current.Pin {
		return nil
	}
	var err error
	live.Moves, err = o.pinMoves(ctx, live.Placements, f.Pin, envs)
	return err
}

// orgFileNeeds adds to live.EnvNeeds what the stack files of the stacks the
// org pin covers declare, read at the binding the org file gives them (a
// stack it adds too), so a volume no deploy wrote down yet still needs
// storage. A file that cannot be read is a blocker in FileErrors.
func (o *Orchestrator) orgFileNeeds(ctx context.Context, live *orgconfig.Live, f *orgconfig.File) {
	covered := map[string]store.Stack{}
	pinned := map[string]bool{}
	for _, sl := range live.Stacks {
		if sl.Stack.PlacementID != nil {
			pinned[sl.Stack.Slug] = true
		} else {
			covered[sl.Stack.Slug] = sl.Stack
		}
	}
	for slug, ref := range f.Stacks {
		if pinned[slug] {
			continue
		}
		st, ok := covered[slug]
		if !ok { // the apply creates it; its clone gets a directory of its own
			st = store.Stack{ID: "new-" + live.Org.ID + "-" + slug, OrgID: live.Org.ID, Slug: slug}
		}
		b := ref.Binding(live.Org)
		st.ConfigRepo, st.ConfigBranch, st.ConfigPath, st.ConfigConnectorID = b.Repo, b.Branch, b.Path, b.ConnectorID
		covered[slug] = st
	}
	for _, slug := range slices.Sorted(maps.Keys(covered)) {
		fn, err := o.stackFileNeeds(ctx, live.Org, covered[slug])
		if err != nil {
			live.FileErrors = append(live.FileErrors, err.Error())
			continue
		}
		for env, n := range fn {
			path := slug + "/" + env
			cur := live.EnvNeeds[path]
			cur.Storage = cur.Storage || n.Storage
			live.EnvNeeds[path] = cur
		}
	}
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
	live.Set = func(ctx context.Context, w promote.PlacementWrite) error { return o.setPlacing(ctx, w) }
	return live, nil
}

// setPlacing applies one planned stack-file change.
func (o *Orchestrator) setPlacing(ctx context.Context, w promote.PlacementWrite) error {
	switch w.Field {
	case "pin":
		return o.setPin(ctx, w.Scope, w.ScopeID, w.Refs[0], w.Except)
	case "ingress":
		return o.SetIngress(ctx, nil, w.Scope, w.ScopeID, w.Refs[0])
	case "exits":
		return o.SetExits(ctx, nil, w.Scope, w.ScopeID, w.Refs)
	}
	return nil
}

// setPin pins a scope and moves the envs it takes along (D4), but the ones
// in except. The plan was approved, so the moves are confirmed: they act as
// the config file (the security log reads "configfile"; no admin rights, so
// the org's open-to check still holds). A move is a queued job, not awaited.
func (o *Orchestrator) setPin(ctx context.Context, kind, id, placement string, except []string) error {
	by := &Principal{User: User{ID: "configfile"}}
	_, err := o.PinScope(ctx, by, kind, id, placement, o.scopeSlug(ctx, kind, id), except...)
	return err
}

// applyOrgPlacing applies the org file's pin, ingress and exits changes. The
// moving envs are the ones the diff just computed.
func (o *Orchestrator) applyOrgPlacing(ctx context.Context, og store.Org, f *orgconfig.File, live orgconfig.Live, kind string) error {
	switch kind {
	case "pin":
		return o.setPin(ctx, ScopeOrg, og.ID, f.Pin, nil)
	case "ingress":
		return o.SetIngress(ctx, nil, ScopeOrg, og.ID, f.Ingress)
	case "exits":
		return o.SetExits(ctx, nil, ScopeOrg, og.ID, f.Exits)
	}
	return nil
}

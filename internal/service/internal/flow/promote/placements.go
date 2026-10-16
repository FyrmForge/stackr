package promote

import (
	"context"
	"fmt"
	"io"
	"maps"
	"slices"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/planfile"
)

func (f *File) placing() planfile.Placing {
	return planfile.Placing{Pin: f.Pin, Ingress: f.Ingress, Exits: f.Exits}
}

func (e EnvConf) placing() planfile.Placing {
	return planfile.Placing{Pin: e.Pin, Ingress: e.Ingress, Exits: e.Exits}
}

// PlacementWrite is one pin, ingress or exits change the apply hands to
// PlacementLive.Set. Field is pin, ingress or exits; Scope is stack or env
// and ScopeID its row. Moving is the "stack/env" paths a pin change moves.
type PlacementWrite struct {
	Scope, ScopeID string
	Field          string
	Refs           []string // pin and ingress: one placement; exits: the ladder
	Moving         []string
}

// placeStep is a PlacementWrite the plan decided on.
type placeStep struct{ PlacementWrite }

// planPlacing plans the stack file's pin, ingress and exits against the
// placements the service gathered: the stack's own keys land with the bottom
// rung (as its defaults do), an env's with that env. Nothing here runs a move;
// apply hands each change to PlacementLive.Set after the rollout.
func (f *Flow) planPlacing(ctx context.Context, p *Plan, w *work, r *Resolved) error {
	re := w.re
	stackKeys, envKeys := r.Placing, re.Placing
	if w.e.Type == environment.Ephemeral { // a PR env is made, not placed, by its base
		envKeys = planfile.Placing{}
	}
	if (stackKeys.IsZero() || !w.bottom) && envKeys.IsZero() {
		return nil
	}
	if f.Placements == nil {
		p.Warnings = append(p.Warnings, "pin, ingress and exits are not read here")
		return nil
	}
	live, err := f.Placements(ctx, w.e.ID)
	if err != nil {
		return err
	}
	w.place = &live
	if w.bottom && !stackKeys.IsZero() {
		var except []string // envs the file pins itself are not moved by the stack's pin
		for _, n := range slices.Sorted(maps.Keys(r.Envs)) {
			if r.Envs[n].Placing.Pin != "" {
				except = append(except, n)
			}
		}
		var need planfile.Needs
		if stackKeys.Pin != "" && live.Needs != nil {
			ns, err := live.Needs(ctx)
			if err != nil {
				return err
			}
			for _, n := range ns {
				need.Storage = need.Storage || n.Storage
			}
		}
		if err := f.planScope(ctx, p, w, "stack", w.st.ID, w.st.Slug, stackKeys, live.Stack, need, except); err != nil {
			return err
		}
	}
	if envKeys.IsZero() {
		return nil
	}
	var need planfile.Needs
	if envKeys.Pin != "" && live.Needs != nil {
		ns, err := live.Needs(ctx)
		if err != nil {
			return err
		}
		need = ns[w.e.Slug]
	}
	return f.planScope(ctx, p, w, "env", w.e.ID, w.e.Slug, envKeys, live.Env, need, nil)
}

func (f *Flow) planScope(
	ctx context.Context,
	p *Plan,
	w *work,
	scope, scopeID, tile string,
	want, have planfile.Placing,
	need planfile.Needs,
	except []string,
) error {
	in := planfile.PlacingIn{Scope: scope, Tile: tile, Want: want, Have: have, Need: need, Facts: w.place.Placements}
	if want.Pin != "" && want.Pin != have.Pin && w.place.Moves != nil {
		var err error
		if in.Moves, err = w.place.Moves(ctx, scope, want.Pin, except); err != nil {
			return err
		}
	}
	cs, blockers, warns := planfile.PlacingPlan(in)
	for _, b := range blockers {
		p.block("%s", b)
	}
	// A promote has no confirm step, so a pin that moves running envs waits
	// for the move to be confirmed where it can be; once they are there, the
	// pin moves nothing and lands.
	for _, c := range cs {
		if c.Kind == "pin" && c.Impact != "" {
			p.block("pin %s %s; confirm that move on the Placement tab or with stackr pin, then promote again", c.New, c.Impact)
		}
	}
	p.Warnings = append(p.Warnings, warns...)
	for _, c := range cs {
		p.add(c)
		st := placeStep{PlacementWrite{Scope: scope, ScopeID: scopeID, Field: c.Kind, Refs: []string{c.New}}}
		switch c.Kind {
		case "exits":
			st.Refs = want.Exits
		case "pin":
			st.Moving = slices.Sorted(maps.Keys(in.Moves))
		}
		w.placing = append(w.placing, st)
	}
	return nil
}

// applyPlacing writes the planned changes, last: a move queues behind the
// rollout that just ran, never alongside it.
func (f *Flow) applyPlacing(ctx context.Context, w *work, log io.Writer) error {
	if len(w.placing) == 0 || w.place == nil || w.place.Set == nil {
		return nil
	}
	for _, s := range w.placing {
		logf(log, "%s %s %s\n", s.Field, s.Scope, s.Refs)
		if err := w.place.Set(ctx, s.PlacementWrite); err != nil {
			return fmt.Errorf("%s: %w", s.Field, err)
		}
	}
	return nil
}

// placementsOf gathers the facts for env, nil when nothing is wired.
func (f *Flow) placementsOf(ctx context.Context, envID string) (*PlacementLive, error) {
	if f.Placements == nil {
		return nil, nil
	}
	live, err := f.Placements(ctx, envID)
	return &live, err
}

// putPlacing writes a scope's pin, ingress and exits, the ones it holds.
func putPlacing(m *omap, p planfile.Placing) {
	if p.Pin != "" {
		m.put("pin", p.Pin)
	}
	if p.Ingress != "" {
		m.put("ingress", p.Ingress)
	}
	if len(p.Exits) > 0 {
		m.put("exits", p.Exits)
	}
}

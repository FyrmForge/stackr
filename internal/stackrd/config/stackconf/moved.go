package stackconf

// The `moved:` directive: declarative renames (docs/plans/06-moved-directive.md).
//
// Map keys are identity to the differ, so renaming one in the file plans a
// delete and a create. For a tile with a volume that destroys data. A `moved:`
// entry tells the differ the two keys are the same thing.
//
// Renames are never inferred. A vanished key plus an appeared key with a
// similar body is exactly the shape of two unrelated edits in one commit, and
// guessing wrong moves a volume onto a tile that should never have had it.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/envnet"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// MovedEntry is one rename. Both sides are kind-dotted names in the file's own
// scope ("tile.web", "env.staging"): a stack file must never be able to declare
// a move in another stack, so there are no org:stack: path prefixes.
type MovedEntry struct {
	From string `yaml:"from" json:"from"`
	To   string `yaml:"to" json:"to"`
}

// Move is a parsed entry: the kind, and the two slugs.
type Move struct {
	Kind string // tile | env
	From string
	To   string
}

// ParseMoves validates the list and splits every entry into kind and slugs.
// kinds is what this file type may address, named in the error so the author
// is not left guessing.
func ParseMoves(entries []MovedEntry, kinds ...string) ([]Move, error) {
	valid := map[string]bool{}
	for _, k := range kinds {
		valid[k] = true
	}
	var out []Move
	seenFrom, seenTo := map[string]bool{}, map[string]bool{}
	for _, e := range entries {
		fk, fs, err := splitMove(e.From, valid, kinds)
		if err != nil {
			return nil, fmt.Errorf("moved: from: %w", err)
		}
		tk, ts, err := splitMove(e.To, valid, kinds)
		if err != nil {
			return nil, fmt.Errorf("moved: to: %w", err)
		}
		if fk != tk {
			return nil, fmt.Errorf("moved: %s -> %s moves between kinds; a move is a rename, not a conversion", e.From, e.To)
		}
		if fs == ts {
			// A no-op rename is a typo, not an instruction.
			return nil, fmt.Errorf("moved: %s -> %s is the same name", e.From, e.To)
		}
		key := fk + "." + fs
		if seenFrom[key] {
			return nil, fmt.Errorf("moved: %s appears as from: twice", e.From)
		}
		if seenTo[fk+"."+ts] {
			return nil, fmt.Errorf("moved: %s appears as to: twice", e.To)
		}
		seenFrom[key], seenTo[fk+"."+ts] = true, true
		out = append(out, Move{Kind: fk, From: fs, To: ts})
	}
	// A chain (a->b, b->c) means the author skipped an apply: after the first
	// hop the live row is b, and both entries would claim it.
	for _, m := range out {
		if seenFrom[m.Kind+"."+m.To] {
			return nil, fmt.Errorf("moved: %s.%s is both a target and a source; one hop per entry, apply in between",
				m.Kind, m.To)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind+out[i].From < out[j].Kind+out[j].From })
	return out, nil
}

// splitMove reads "kind.name" and slugifies the name, so a display-name key
// and a raw slug both land on the same thing.
func splitMove(ref string, valid map[string]bool, kinds []string) (string, string, error) {
	kind, name, ok := strings.Cut(strings.TrimSpace(ref), ".")
	if !ok || name == "" {
		return "", "", fmt.Errorf("%q needs a kind prefix (%s)", ref, strings.Join(kinds, ", "))
	}
	if !valid[kind] {
		return "", "", fmt.Errorf("%q: unknown kind %q (want %s)", ref, kind, strings.Join(kinds, ", "))
	}
	slug := repo.Slugify(name)
	if slug == "" {
		return "", "", fmt.Errorf("%q has no slug in it", ref)
	}
	return kind, slug, nil
}

// MoveState is what the planner needs to decide each entry's fate, without
// this package having to know how a caller stores things.
type MoveState struct {
	// HasFrom and HasTo report whether a live row carries that slug.
	HasFrom, HasTo bool
	// Declared reports whether the file declares the target. A move whose
	// target is not declared is a rename followed by a delete in disguise.
	Declared bool
}

// PlanMove decides one entry: a rename change, a no-op warning, or an error.
// Exactly one of the three is non-empty.
func PlanMove(m Move, st MoveState) (change *Change, warning, err string) {
	if !st.Declared {
		return nil, "", fmt.Sprintf("moved: %s.%s is not declared in this file; a move to something the file does not declare is a rename and a delete, say which you mean",
			m.Kind, m.To)
	}
	switch {
	case st.HasFrom && st.HasTo:
		// The marker claims `to` is the old `from`, but a real `from` still
		// lives. Applying it would merge two rows.
		return nil, "", fmt.Sprintf("moved: both %s.%s and %s.%s exist; the move cannot say which is which",
			m.Kind, m.From, m.Kind, m.To)
	case st.HasFrom:
		return &Change{Kind: "move", Env: moveEnvLabel(m), Tile: m.From, Field: "slug",
			Old: m.From, New: m.To,
			Note: "renames it in place: volumes, variables and history ride the row, nothing is destroyed",
		}, "", ""
	case st.HasTo:
		return nil, fmt.Sprintf("moved: %s.%s -> %s.%s is already applied; the entry can be removed",
			m.Kind, m.From, m.Kind, m.To), ""
	}
	// Neither side exists: the target is declared, so the create row already
	// covers it and the marker is simply stale.
	return nil, fmt.Sprintf("moved: neither %s.%s nor %s.%s exists; the entry has nothing to move",
		m.Kind, m.From, m.Kind, m.To), ""
}

// moveEnvLabel is the plan row's Env column. A tile move applies in every env
// in scope that holds the slug, so it is not one env's row.
func moveEnvLabel(_ Move) string { return "stack" }

// planMoves adds one row per entry and reports the moves the apply should
// actually perform. State matching is by slug, per env for tiles.
//
// Scoped by opts, the same as every other walk: an env-branch plan that saw
// the whole stack reported "both exist" for a move it had already applied in
// its own env, and the stack was then stuck with an erroring plan.
func planMoves(r *Resolved, s State, p *Plan, opts DiffOpts) []Move {
	var todo []Move
	for _, m := range r.Moves {
		st := MoveState{}
		switch m.Kind {
		case "env":
			if opts.OutOfScope(m.From) || opts.OutOfScope(m.To) {
				continue
			}
			_, st.HasFrom = s.Envs[m.From]
			_, st.HasTo = s.Envs[m.To]
			_, st.Declared = r.Envs[m.To]
		default:
			// State matching is scoped: a tile move applies in every env in
			// scope where the slug exists. Declaredness is not, it is a
			// property of the file and the same in every plan.
			for slug, es := range s.Envs {
				if opts.OutOfScope(slug) {
					continue
				}
				if _, ok := es.Tiles[m.From]; ok {
					st.HasFrom = true
				}
				if _, ok := es.Tiles[m.To]; ok {
					st.HasTo = true
				}
			}
			for _, re := range r.Envs {
				if _, ok := re.Tiles[m.To]; ok {
					st.Declared = true
				}
			}
			// Neither side inside a narrowed scope: the entry is somebody
			// else's plan to report, not a stale marker to warn about.
			if !st.HasFrom && !st.HasTo && (opts.OnlyEnv != "" || len(opts.SkipEnvs) > 0) {
				continue
			}
		}
		ch, warn, err := PlanMove(m, st)
		switch {
		case err != "":
			p.Errors = append(p.Errors, err)
		case warn != "":
			p.Warnings = append(p.Warnings, warn)
		case ch != nil:
			p.Changes = append(p.Changes, *ch)
			todo = append(todo, m)
		}
	}
	return todo
}

// renameState re-keys live state so the differ sees the move as one object
// under a new name rather than a delete and a create. The State is the
// planner's own copy, so this never touches what the caller holds.
func renameState(s State, moves []Move) State {
	if len(moves) == 0 {
		return s
	}
	envs := make(map[string]EnvState, len(s.Envs))
	for slug, es := range s.Envs {
		envs[slug] = es
	}
	for _, m := range moves {
		if m.Kind == "env" {
			if es, ok := envs[m.From]; ok {
				delete(envs, m.From)
				envs[m.To] = es
			}
			continue
		}
		for slug, es := range envs {
			ts, ok := es.Tiles[m.From]
			if !ok {
				continue
			}
			tiles := make(map[string]TileState, len(es.Tiles))
			for k, v := range es.Tiles {
				tiles[k] = v
			}
			delete(tiles, m.From)
			tiles[m.To] = ts
			es.Tiles = tiles
			envs[slug] = es
		}
	}
	s.Envs = envs
	return s
}

// applyMoves performs the renames: the row keeps its id, so volumes, variables
// and history ride along, and everything derived from the slug is rebuilt.
//
// The old service is torn down first. Container and service names are derived
// from the slug (envnet.ServiceName), so a renamed tile would otherwise deploy
// alongside its own former self instead of replacing it. Swarm cannot rename a
// service in place, so tear down and redeploy is the only shape there is; the
// redeploy is what makes the docs' promise that a move does not take the tile
// down true. Volumes are id-keyed and the proxy file is rewritten on deploy,
// so nothing is lost in between.
//
// Scoped by opts, like every other walk in an apply: without it an env-branch
// plan renamed the row in every environment, including the ones held back for
// a person.
func (a Applier) applyMoves(ctx context.Context, stack *repo.Stack, r *Resolved, opts DiffOpts) error {
	if len(r.Moves) == 0 {
		return nil
	}
	store := a.Planner.Store
	envs, err := store.ListEnvironmentsByStack(ctx, stack.ID)
	if err != nil {
		return err
	}
	for _, m := range r.Moves {
		if m.Kind == "env" {
			if opts.OutOfScope(m.From) || opts.OutOfScope(m.To) {
				continue
			}
			env, err := store.GetEnvironmentBySlug(ctx, stack.ID, m.From)
			if err != nil || env == nil {
				continue // already applied, or nothing to move; the plan said so
			}
			if other, _ := store.GetEnvironmentBySlug(ctx, stack.ID, m.To); other != nil {
				continue // both exist: a plan error, refused before this ran
			}
			// Every tile in it is torn down: the env slug is part of every
			// container and network name under it.
			tiles, _ := store.ListTilesByEnv(ctx, env.ID)
			for i := range tiles {
				envnet.TearDown(ctx, store, a.Ops.Cluster, &tiles[i])
			}
			env.Name, env.Slug = strings.ToUpper(m.To[:1])+m.To[1:], m.To
			if err := store.RenameEnvironment(ctx, env.ID, env.Name, env.Slug); err != nil {
				return err
			}
			// Back up under the new names. Same selection as a stack rename:
			// what was running comes back, what was not stays as it was.
			for i := range tiles {
				a.restartTile(ctx, &tiles[i])
			}
			continue
		}
		// A tile move applies in every env in scope that holds the slug:
		// state matching is per env by slug.
		for i := range envs {
			if opts.OutOfScope(envs[i].Slug) {
				continue
			}
			t, err := store.GetTileBySlug(ctx, envs[i].ID, m.From)
			if err != nil || t == nil {
				continue
			}
			if other, _ := store.GetTileBySlug(ctx, envs[i].ID, m.To); other != nil {
				continue // both exist: a plan error, refused before this ran
			}
			// Teardown, rename, route rewrite — the service owns the order,
			// and this path owns the redeploy because the applier reports
			// what it deployed.
			if err := a.Ops.Tiles.Rename(ctx, t, m.To, m.To); err != nil {
				return err
			}
			a.restartTile(ctx, t)
		}
	}
	return nil
}

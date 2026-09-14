package stackconf

import (
	"context"
	"encoding/json"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// StagingOpts is the diff context for UI staging: no bound git repo (tiles
// carry their own source via the serializer), plus the domain-claim context
// so auto/apex entries resolve. Scoped to one env.
func (pl Planner) StagingOpts(ctx context.Context, stack *repo.Stack, envSlug string) DiffOpts {
	opts := DiffOpts{OnlyEnv: envSlug}
	pl.loadDomainContext(ctx, stack, &opts)
	return opts
}

// StagedResolved builds a stack's desired config: the committed tiles
// (StateToResolved) with every staged change patched on top, in order.
func (a Applier) StagedResolved(ctx context.Context, stack *repo.Stack) (*Resolved, State, error) {
	state, err := a.Planner.Snapshot(ctx, stack)
	if err != nil {
		return nil, state, err
	}
	r := StateToResolved(stack.Name, state)

	changes, err := a.Planner.Store.ListStagedByStack(ctx, stack.ID)
	if err != nil {
		return nil, state, err
	}
	envs, err := a.Planner.Store.ListEnvironmentsByStack(ctx, stack.ID)
	if err != nil {
		return nil, state, err
	}
	slugByEnv := make(map[string]string, len(envs))
	for _, e := range envs {
		slugByEnv[e.ID] = e.Slug
	}
	// created_at order (ListStagedByStack) makes last-write-win per field, and
	// makes a later delete supersede an earlier update; an update on an
	// already-deleted tile is a no-op, so a delete wins regardless of order.
	for _, ch := range changes {
		re, ok := r.Envs[slugByEnv[ch.EnvID]]
		if !ok {
			continue
		}
		applyStagedPatch(re, ch)
	}
	return r, state, nil
}

// stagedPayload is a staged change's JSON blob: an op plus a sparse patch.
// The op lives in the blob (not a column) so migration 014 stays as-is.
type stagedPayload struct {
	Op    string          `json:"op"`    // update (default) | create | delete
	Patch json.RawMessage `json:"patch"` // sparse TileConf (update) or full (create)
}

// applyStagedPatch overlays one staged change onto its env's tile set.
//   - update: the patch's fields replace those on the existing tile.
//   - create: the patch is a full TileConf, added as a new tile.
//   - delete: the tile is dropped so Diff emits a strict-mode delete.
func applyStagedPatch(re ResolvedEnv, ch repo.StagedChange) {
	var p stagedPayload
	if json.Unmarshal([]byte(ch.Payload), &p) != nil {
		return
	}
	switch p.Op {
	case "delete":
		delete(re.Tiles, ch.TileSlug)
	case "create":
		var tc TileConf
		if json.Unmarshal(p.Patch, &tc) == nil {
			if tc.Type == "" {
				tc.Type = "service"
			}
			re.Tiles[ch.TileSlug] = tc
		}
	default: // update
		tc, ok := re.Tiles[ch.TileSlug]
		if !ok {
			return // tile vanished under the staged edit, drop it
		}
		mergePatch(&tc, p.Patch)
		re.Tiles[ch.TileSlug] = tc
	}
}

// mergePatch applies a sparse JSON patch onto tc with replace semantics.
// json.Unmarshal overwrites scalars and replaces slices, but MERGES into a
// non-nil map, so Env is nulled first when the patch sets it, keeping "set
// env to exactly this" honest (a removed var actually disappears).
func mergePatch(tc *TileConf, patch json.RawMessage) {
	var probe struct {
		Env *EnvMap `json:"env"`
	}
	_ = json.Unmarshal(patch, &probe)
	if probe.Env != nil {
		tc.Env = nil
	}
	_ = json.Unmarshal(patch, tc)
}

// StagedPlan diffs a stack's staged desired config against current state,
// scoped to one environment.
func (a Applier) StagedPlan(ctx context.Context, stack *repo.Stack, envSlug string) (*Plan, error) {
	r, state, err := a.StagedResolved(ctx, stack)
	if err != nil {
		return nil, err
	}
	return Diff(r, state, a.Planner.StagingOpts(ctx, stack, envSlug)), nil
}

// ApplyStaged reconciles one env's staged config and clears that env's staged
// changes on success. force=true is human approval (allows deletes / ignores
// per-env policy).
func (a Applier) ApplyStaged(ctx context.Context, stack *repo.Stack, env *repo.Environment, force bool) (bool, error) {
	r, _, err := a.StagedResolved(ctx, stack)
	if err != nil {
		return false, err
	}
	ok, err := a.ApplyResolved(ctx, stack, r, a.Planner.StagingOpts(ctx, stack, env.Slug), force)
	if err != nil || !ok {
		return ok, err
	}
	return true, a.Planner.Store.DeleteStagedByEnv(ctx, env.ID)
}

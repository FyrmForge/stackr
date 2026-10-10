package service

import (
	"context"
	"strings"

	"github.com/FyrmForge/stackr/internal/authz"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/slug"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
)

type EnvSpec = environment.Spec

// Envs is the stack's environments; Ladder only the promote rungs, in order.
func (o *Orchestrator) Envs(ctx context.Context, stackID string) ([]Environment, error) {
	return o.envs.List(ctx, stackID)
}

func (o *Orchestrator) Ladder(ctx context.Context, stackID string) ([]Environment, error) {
	return o.envs.Ladder(ctx, stackID)
}

// EnvHues is the hue each env of a list (Envs' order) is drawn in, by id:
// its stored colour, else v0's default for its slug or place.
func EnvHues(envs []Environment) map[string]string {
	return environment.Hues(envs)
}

// JoinsLockedTier refuses a name whose slug is a locked tier of the stack's
// org unless the caller holds tier.write. Every surface that creates or
// renames a static env calls it first; cur is the env's slug on a rename
// (staying in its tier is no join), "" on a create.
func (o *Orchestrator) JoinsLockedTier(ctx context.Context, by *Principal, stackID, name, cur string) error {
	sl := slug.Make(strings.TrimSpace(name))
	if sl == "" || sl == cur {
		return nil
	}
	st, err := o.stacks.Get(ctx, stackID)
	if err != nil {
		return err
	}
	t, in, err := o.tiers.Of(ctx, st.OrgID, sl)
	if err != nil || !in || !t.Locked {
		return err
	}
	if by == nil || authz.Can(by.Access, "tier.write", authz.Resource{OrgID: st.OrgID, StackID: stackID}) != nil {
		return errs.Refusedf("%s is a locked tier; an org owner adds envs to it", sl)
	}
	return nil
}

func (o *Orchestrator) CreateEnv(ctx context.Context, stackID, name string, s EnvSpec) (Environment, error) {
	return o.envs.Create(ctx, stackID, name, s)
}

// RenameEnv moves name and slug, and the auto domains under the env with
// them.
func (o *Orchestrator) RenameEnv(ctx context.Context, id, name string) (Environment, error) {
	// [old] and [new] refs from other envs, and its own view if it joins or
	// leaves a tier
	sc := ParamScope{Kind: "env", ID: id}
	var was bool
	var old []Tile
	e, err := o.onEnv(ctx, id, func(e Environment) (Environment, error) {
		var err error
		if _, was, err = o.envTier(ctx, e); err != nil {
			return e, err
		}
		if old, err = o.readersOf(ctx, sc, true); err != nil {
			return e, err
		}
		return o.envs.Rename(ctx, e, name)
	})
	if err != nil {
		return e, err
	}
	ts, err := o.tiles.List(ctx, id)
	if err != nil {
		return e, err
	}
	if err := o.refreshAutoHosts(ctx, ts); err != nil {
		return e, err
	}
	now, err := o.readersOf(ctx, sc, true)
	if err != nil {
		return e, err
	}
	if _, in, err := o.envTier(ctx, e); err != nil {
		return e, err
	} else if in != was {
		now = append(now, ts...)
	}
	return e, o.redeployUnion(ctx, old, now)
}

// SetEnvFrom sets where the env's releases come from: a branch (auto or
// not) or promotion from the rung below.
func (o *Orchestrator) SetEnvFrom(ctx context.Context, id, kind, branch string, auto bool) (Environment, error) {
	return o.onEnv(ctx, id,
		func(e Environment) (Environment, error) { return o.envs.SetFrom(ctx, e, kind, branch, auto) })
}

func (o *Orchestrator) SetEnvColor(ctx context.Context, id, color string) (Environment, error) {
	return o.onEnv(ctx, id, func(e Environment) (Environment, error) { return o.envs.SetColor(ctx, e, color) })
}

// SetEnvSettings writes the env's settings blob and redeploys its running
// tiles (B34).
func (o *Orchestrator) SetEnvSettings(ctx context.Context, id, blob string) (Environment, error) {
	if err := o.checkBlobLimits(ctx, blob); err != nil {
		return Environment{}, err
	}
	e, err := o.onEnv(ctx, id, func(e Environment) (Environment, error) { return o.envs.SetSettings(ctx, e, blob) })
	if err != nil {
		return e, err
	}
	ts, err := o.tiles.List(ctx, id)
	if err != nil {
		return e, err
	}
	return e, o.redeployRunning(ctx, ts)
}

// ReorderEnvs sets the ladder order, bottom rung first.
func (o *Orchestrator) ReorderEnvs(ctx context.Context, stackID string, ids []string) error {
	return o.envs.Reorder(ctx, stackID, ids)
}

// DeleteEnv refuses while the env has tiles.
func (o *Orchestrator) DeleteEnv(ctx context.Context, id string) error {
	e, err := o.envs.Get(ctx, id)
	if err != nil {
		return err
	}
	ts, err := o.tiles.List(ctx, id)
	if err != nil {
		return err
	}
	if err := o.envs.Delete(ctx, e, len(ts)); err != nil {
		return err
	}
	o.cancelWaitingFor(ctx, "env:"+id)
	return nil
}

func (o *Orchestrator) onEnv(
	ctx context.Context,
	id string,
	f func(Environment) (Environment, error),
) (Environment, error) {
	e, err := o.envs.Get(ctx, id)
	if err != nil {
		return e, err
	}
	return f(e)
}

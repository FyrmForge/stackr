package service

import (
	"context"
	"errors"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tier"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Tier is one rung of an org's env ladder. A stack env is in the tier whose
// slug equals its own and takes the tier's lock.
type Tier = store.Tier

// Tiers is the org's tiers, bottom first.
func (o *Orchestrator) Tiers(ctx context.Context, orgID string) ([]Tier, error) {
	return o.tiers.List(ctx, orgID)
}

// CreateTier puts a locked tier on top of the ladder.
func (o *Orchestrator) CreateTier(ctx context.Context, orgID, slug string) (Tier, error) {
	// the org's first tier switches its readers from org to tier blocks
	have, err := o.tiers.List(ctx, orgID)
	if err != nil {
		return Tier{}, err
	}
	var pre []Tile
	if len(have) == 0 {
		if pre, err = o.readersOf(ctx, ParamScope{Kind: "org", ID: orgID}, false); err != nil {
			return Tier{}, err
		}
	}
	t, err := o.tiers.Create(ctx, orgID, slug)
	if err != nil {
		return t, err
	}
	// a live static env with the slug joins the tier and gains its block
	live, err := o.slugTiles(ctx, orgID, t.Slug)
	if err != nil {
		return t, err
	}
	return t, o.redeployUnion(ctx, pre, live)
}

// slugTiles is the tiles of the org's static envs named slug.
func (o *Orchestrator) slugTiles(ctx context.Context, orgID, slug string) ([]Tile, error) {
	sts, err := o.stacks.List(ctx, orgID)
	if err != nil {
		return nil, err
	}
	var ts []Tile
	for _, st := range sts {
		e, err := o.envs.GetBySlug(ctx, st.ID, slug)
		if errors.Is(err, errs.ErrNotFound) || (err == nil && e.Type != environment.Static) {
			continue
		}
		if err != nil {
			return nil, err
		}
		more, err := o.tiles.List(ctx, e.ID)
		if err != nil {
			return nil, err
		}
		ts = append(ts, more...)
	}
	return ts, nil
}

// RenameTier moves the slug. Stack envs keep theirs, so they leave the tier.
func (o *Orchestrator) RenameTier(ctx context.Context, orgID, slug, newSlug string) (Tier, error) {
	t, err := o.tiers.GetBySlug(ctx, orgID, slug)
	if err != nil {
		return t, err
	}
	sts, err := o.stacks.List(ctx, orgID)
	if err != nil {
		return t, err
	}
	for _, st := range sts {
		es, err := o.envs.List(ctx, st.ID)
		if err != nil {
			return t, err
		}
		for _, e := range es {
			if e.Type == environment.Static && e.Slug == slug {
				return t, errs.Conflictf("%s/%s is still in tier %s; rename the env first", st.Slug, e.Slug, slug)
			}
		}
	}
	// [old] refs and the envs of the old slug lose the block; [new] refs and
	// an env already named new gain it
	sc := ParamScope{Kind: "tier", ID: t.ID}
	old, err := o.readersOf(ctx, sc, false)
	if err != nil {
		return t, err
	}
	if t, err = o.tiers.Rename(ctx, t, newSlug); err != nil {
		return t, err
	}
	now, err := o.readersOf(ctx, sc, false)
	if err != nil {
		return t, err
	}
	return t, o.redeployUnion(ctx, old, now)
}

// ReorderTiers sets the ladder, bottom first: every tier once.
func (o *Orchestrator) ReorderTiers(ctx context.Context, orgID string, slugs []string) error {
	return o.store.Tx(ctx, func(tx store.Tx) error { return tier.New(tx.Tiers).Reorder(ctx, orgID, slugs) })
}

// DeleteTier refuses while a stack env still has the tier's slug.
func (o *Orchestrator) DeleteTier(ctx context.Context, orgID, slug string) error {
	t, err := o.tiers.GetBySlug(ctx, orgID, slug)
	if err != nil {
		return err
	}
	sts, err := o.stacks.List(ctx, orgID)
	if err != nil {
		return err
	}
	for _, st := range sts {
		es, err := o.envs.List(ctx, st.ID)
		if err != nil {
			return err
		}
		for _, e := range es {
			if e.Type == environment.Static && e.Slug == slug {
				return errs.Conflictf("%s/%s is still in tier %s", st.Slug, e.Slug, slug)
			}
		}
	}
	// [slug] refs elsewhere lose the block (no env is in it: refused above)
	refs, err := o.readersOf(ctx, ParamScope{Kind: "tier", ID: t.ID}, true)
	if err != nil {
		return err
	}
	if err := o.tiers.Delete(ctx, t.ID); err != nil {
		return err
	}
	// the last tier gone: the org's readers go back to org blocks
	left, err := o.tiers.List(ctx, orgID)
	if err != nil {
		return err
	}
	var org []Tile
	if len(left) == 0 {
		if org, err = o.readersOf(ctx, ParamScope{Kind: "org", ID: orgID}, false); err != nil {
			return err
		}
	}
	return o.redeployUnion(ctx, refs, org)
}

// SetTierLock locks or unlocks the tier for every env in it.
func (o *Orchestrator) SetTierLock(ctx context.Context, orgID, slug string, locked bool) (Tier, error) {
	t, err := o.tiers.GetBySlug(ctx, orgID, slug)
	if err != nil {
		return t, err
	}
	was := t.Locked
	if t, err = o.tiers.SetLocked(ctx, t, locked); err != nil || was == locked {
		return t, err
	}
	ts, err := o.readersOf(ctx, ParamScope{Kind: "tier", ID: t.ID}, true)
	if err != nil {
		return t, err
	}
	return t, o.redeployRunning(ctx, ts)
}

// SetEnvLock locks or unlocks an off-tier env. A tiered env's lock is its
// tier's.
func (o *Orchestrator) SetEnvLock(ctx context.Context, envID string, locked bool) (Environment, error) {
	was := false
	e, err := o.onEnv(ctx, envID, func(e Environment) (Environment, error) {
		was = e.Locked
		if e.Type == environment.Ephemeral {
			return e, errs.Conflictf("PR envs have no lock")
		}
		if e.Type == environment.Static {
			st, err := o.stacks.Get(ctx, e.StackID)
			if err != nil {
				return e, err
			}
			if _, in, err := o.tiers.Of(ctx, st.OrgID, e.Slug); err != nil {
				return e, err
			} else if in {
				return e, errs.Conflictf("%s's lock is the org's tier lock", e.Slug)
			}
		}
		return o.envs.SetLocked(ctx, e, locked)
	})
	if err != nil || was == locked {
		return e, err
	}
	ts, err := o.readersOf(ctx, ParamScope{Kind: "env", ID: e.ID}, true)
	if err != nil {
		return e, err
	}
	return e, o.redeployRunning(ctx, ts)
}

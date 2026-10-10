// Package tier owns an org's tiers: the ordered ladder of env names (dev,
// staging, prod) and each one's lock. A stack env is in the tier whose slug
// equals its own; nothing links the two rows, so a rename moves the tier and
// leaves envs behind (the service refuses a delete a stack env still names).
package tier

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/slug"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// PR is the slug of the PR block ("params.c[pr]"): never a tier.
const PR = "pr"

type Leaf struct{ tiers store.TierStore }

// New takes the table; build it on a store.Tx's table to make Reorder atomic.
func New(tiers store.TierStore) *Leaf { return &Leaf{tiers: tiers} }

// List is the org's tiers, bottom first.
func (l *Leaf) List(ctx context.Context, orgID string) ([]store.Tier, error) {
	return l.tiers.ListByOrg(ctx, orgID)
}

func (l *Leaf) Get(ctx context.Context, id string) (store.Tier, error) {
	return l.tiers.Get(ctx, id)
}

func (l *Leaf) GetBySlug(ctx context.Context, orgID, slug string) (store.Tier, error) {
	return l.tiers.GetBySlug(ctx, orgID, slug)
}

// Of is the tier an env slug is in. A PR env is never in one: the caller
// checks the env's type first.
func (l *Leaf) Of(ctx context.Context, orgID, envSlug string) (store.Tier, bool, error) {
	t, err := l.tiers.GetBySlug(ctx, orgID, envSlug)
	if errors.Is(err, errs.ErrNotFound) {
		return store.Tier{}, false, nil
	}
	return t, err == nil, err
}

// Create puts a locked tier on top of the ladder.
func (l *Leaf) Create(ctx context.Context, orgID, sl string) (store.Tier, error) {
	t := store.Tier{ID: uuid.NewString(), OrgID: orgID, Locked: true, CreatedAt: time.Now().UTC()}
	if err := l.check(ctx, &t, sl); err != nil {
		return t, err
	}
	all, err := l.tiers.ListByOrg(ctx, orgID)
	if err != nil {
		return t, err
	}
	if len(all) > 0 {
		t.Position = all[len(all)-1].Position + 1
	}
	return t, l.tiers.Create(ctx, t)
}

// Rename moves the slug; the position and lock stay.
func (l *Leaf) Rename(ctx context.Context, t store.Tier, sl string) (store.Tier, error) {
	if err := l.check(ctx, &t, sl); err != nil {
		return t, err
	}
	return t, l.tiers.Update(ctx, t)
}

func (l *Leaf) check(ctx context.Context, t *store.Tier, sl string) error {
	switch {
	case !slug.Valid(sl):
		return errs.Invalidf("slug", "%q is not a valid tier name.", sl)
	case sl == PR || sl == "order" || slug.Reserved(sl):
		return errs.Invalidf("slug", "%q is reserved.", sl)
	}
	if other, err := l.tiers.GetBySlug(ctx, t.OrgID, sl); err == nil && other.ID != t.ID {
		return errs.Invalidf("slug", "Another tier already uses that name.")
	} else if err != nil && !errors.Is(err, errs.ErrNotFound) {
		return err
	}
	t.Slug = sl
	return nil
}

// SetLocked writes the tier's lock.
func (l *Leaf) SetLocked(ctx context.Context, t store.Tier, locked bool) (store.Tier, error) {
	t.Locked = locked
	return t, l.tiers.Update(ctx, t)
}

// Reorder sets the ladder to slugs, bottom first: exactly the org's tiers,
// each once.
func (l *Leaf) Reorder(ctx context.Context, orgID string, slugs []string) error {
	all, err := l.tiers.ListByOrg(ctx, orgID)
	if err != nil {
		return err
	}
	byslug := map[string]string{}
	for _, t := range all {
		byslug[t.Slug] = t.ID
	}
	ids := make([]string, 0, len(slugs))
	for _, s := range slugs {
		id, ok := byslug[s]
		if !ok || slices.Contains(ids, id) {
			return errs.Invalidf("order", "Name each tier once, and only the org's tiers.")
		}
		ids = append(ids, id)
	}
	if len(ids) != len(all) {
		return errs.Invalidf("order", "Name each tier once, and only the org's tiers.")
	}
	return l.tiers.Reorder(ctx, ids)
}

// Delete removes the row; its params go with it. Whether a stack env still
// names it is the service's check.
func (l *Leaf) Delete(ctx context.Context, id string) error { return l.tiers.Delete(ctx, id) }

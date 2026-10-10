package store

import (
	"context"
	"time"
)

// Tier is a row of tiers: one rung of an org's env ladder. A stack env whose
// slug equals Slug is in the tier and takes its Locked.
type Tier struct {
	ID        string    `db:"id" json:"id"`
	OrgID     string    `db:"org_id" json:"org_id"`
	Slug      string    `db:"slug" json:"slug"`
	Position  int       `db:"position" json:"position"`
	Locked    bool      `db:"locked" json:"locked"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
}

type TierStore interface {
	Create(ctx context.Context, t Tier) error
	Get(ctx context.Context, id string) (Tier, error)
	GetBySlug(ctx context.Context, orgID, slug string) (Tier, error)
	// ListByOrg is ordered by position, bottom first.
	ListByOrg(ctx context.Context, orgID string) ([]Tier, error)
	Update(ctx context.Context, t Tier) error
	Delete(ctx context.Context, id string) error
	// Reorder sets position to the index of each id in ids.
	Reorder(ctx context.Context, ids []string) error
}

var tiersT = newTable[Tier]("tiers", nil)

type tiers struct{ crud[Tier] }

func (s tiers) GetBySlug(ctx context.Context, orgID, slug string) (Tier, error) {
	return s.one(ctx, "org_id = ? AND slug = ?", orgID, slug)
}

func (s tiers) ListByOrg(ctx context.Context, orgID string) ([]Tier, error) {
	return s.many(ctx, "org_id = ? ORDER BY position, slug", orgID)
}

func (s tiers) Reorder(ctx context.Context, ids []string) error {
	for i, id := range ids {
		res, err := s.q.ExecContext(ctx, "UPDATE tiers SET position = ? WHERE id = ?", i, id)
		if err := affected(res, err); err != nil {
			return err
		}
	}
	return nil
}

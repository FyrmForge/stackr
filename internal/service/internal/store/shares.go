package store

import (
	"context"
	"time"
)

// Share is a row of shares: an org's NFS or SMB export. User and PasswordRef
// are ${{ org.params }} refs, never values.
type Share struct {
	ID          string    `db:"id" json:"id"`
	OrgID       string    `db:"org_id" json:"org_id"`
	Slug        string    `db:"slug" json:"slug"`
	Kind        string    `db:"kind" json:"kind"` // nfs | smb
	Source      string    `db:"source" json:"source"`
	Options     string    `db:"options" json:"options"`
	User        string    `db:"user" json:"user"`
	PasswordRef string    `db:"password_ref" json:"password_ref"`
	CreatedAt   time.Time `db:"created_at" json:"created_at"`
}

type ShareStore interface {
	Create(ctx context.Context, s Share) error
	Get(ctx context.Context, id string) (Share, error)
	GetBySlug(ctx context.Context, orgID, slug string) (Share, error)
	ListByOrg(ctx context.Context, orgID string) ([]Share, error)
	Update(ctx context.Context, s Share) error
	Delete(ctx context.Context, id string) error
}

var sharesT = newTable[Share]("shares", nil)

type shares struct{ crud[Share] }

func (s shares) GetBySlug(ctx context.Context, orgID, slug string) (Share, error) {
	return s.one(ctx, "org_id = ? AND slug = ?", orgID, slug)
}

func (s shares) ListByOrg(ctx context.Context, orgID string) ([]Share, error) {
	return s.many(ctx, "org_id = ? ORDER BY slug", orgID)
}

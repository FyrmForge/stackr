package store

import (
	"context"
	"time"
)

// Org is a row of orgs.
type Org struct {
	ID                string     `db:"id" json:"id"`
	Name              string     `db:"name" json:"name"`
	Slug              string     `db:"slug" json:"slug"`
	AvatarPath        string     `db:"avatar_path" json:"avatar_path"`
	EnvColors         string     `db:"env_colors" json:"env_colors"`
	Settings          string     `db:"settings" json:"settings"`
	SetupDoneAt       *time.Time `db:"setup_done_at" json:"setup_done_at"`
	SetupMode         string     `db:"setup_mode" json:"setup_mode"` // the wizard's branch: "config" or "ui"
	CreatedAt         time.Time  `db:"created_at" json:"created_at"`
	ConfigConnectorID string     `db:"config_connector_id" json:"config_connector_id"`
	ConfigRepo        string     `db:"config_repo" json:"config_repo"`
	ConfigBranch      string     `db:"config_branch" json:"config_branch"`
	ConfigPath        string     `db:"config_path" json:"config_path"`
	ConfigAuto        bool       `db:"config_auto" json:"config_auto"`
}

type OrgStore interface {
	Create(ctx context.Context, o Org) error
	Get(ctx context.Context, id string) (Org, error)
	GetBySlug(ctx context.Context, slug string) (Org, error)
	List(ctx context.Context) ([]Org, error)
	Update(ctx context.Context, o Org) error
	Delete(ctx context.Context, id string) error
}

var orgsT = newTable[Org]("orgs", nil)

type orgs struct{ crud[Org] }

func (s orgs) GetBySlug(ctx context.Context, slug string) (Org, error) {
	return s.one(ctx, "slug = ?", slug)
}

func (s orgs) List(ctx context.Context) ([]Org, error) { return s.many(ctx, "1 = 1") }

// OrgMember is a row of org_members.
type OrgMember struct {
	ID        string    `db:"id" json:"id"`
	OrgID     string    `db:"org_id" json:"org_id"`
	UserID    string    `db:"user_id" json:"user_id"`
	Role      string    `db:"role" json:"role"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
}

type OrgMemberStore interface {
	Create(ctx context.Context, m OrgMember) error
	Get(ctx context.Context, id string) (OrgMember, error)
	GetByOrgUser(ctx context.Context, orgID, userID string) (OrgMember, error)
	ListByOrg(ctx context.Context, orgID string) ([]OrgMember, error)
	ListByUser(ctx context.Context, userID string) ([]OrgMember, error)
	Update(ctx context.Context, m OrgMember) error
	Delete(ctx context.Context, id string) error
}

var orgMembersT = newTable[OrgMember]("org_members", nil)

type orgMembers struct{ crud[OrgMember] }

func (s orgMembers) GetByOrgUser(ctx context.Context, orgID, userID string) (OrgMember, error) {
	return s.one(ctx, "org_id = ? AND user_id = ?", orgID, userID)
}

func (s orgMembers) ListByOrg(ctx context.Context, orgID string) ([]OrgMember, error) {
	return s.many(ctx, "org_id = ?", orgID)
}

func (s orgMembers) ListByUser(ctx context.Context, userID string) ([]OrgMember, error) {
	return s.many(ctx, "user_id = ?", userID)
}

// Invite is a row of invites. ID doubles as the link token.
type Invite struct {
	ID        string     `db:"id" json:"id"`
	OrgID     string     `db:"org_id" json:"org_id"`
	Email     string     `db:"email" json:"email"`
	Role      string     `db:"role" json:"role"`
	CreatedBy string     `db:"created_by" json:"created_by"`
	CreatedAt time.Time  `db:"created_at" json:"created_at"`
	ExpiresAt time.Time  `db:"expires_at" json:"expires_at"`
	UsedAt    *time.Time `db:"used_at" json:"used_at"`
}

type InviteStore interface {
	Create(ctx context.Context, i Invite) error
	Get(ctx context.Context, id string) (Invite, error)
	ListByOrg(ctx context.Context, orgID string) ([]Invite, error)
	Update(ctx context.Context, i Invite) error
	Delete(ctx context.Context, id string) error
	// Burn marks an unused invite used in one statement; ErrNotFound when it
	// is missing or already used, so two clicks cannot both win.
	Burn(ctx context.Context, id string, at time.Time) error
}

var invitesT = newTable[Invite]("invites", nil)

type invites struct{ crud[Invite] }

func (s invites) Burn(ctx context.Context, id string, at time.Time) error {
	res, err := s.q.ExecContext(ctx, `UPDATE invites SET used_at = ? WHERE id = ? AND used_at IS NULL`, at, id)
	return affected(res, err)
}

func (s invites) ListByOrg(ctx context.Context, orgID string) ([]Invite, error) {
	return s.many(ctx, "org_id = ?", orgID)
}

package sqlite

import (
	"context"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

func (s *Store) ListOrgsForUser(ctx context.Context, userID string) ([]repo.Org, error) {
	return list[repo.Org](ctx, s,
		`SELECT o.* FROM orgs o JOIN org_members m ON m.org_id = o.id
		 WHERE m.user_id = ? ORDER BY o.name`, userID)
}

func (s *Store) ListOrgMembers(ctx context.Context, orgID string) ([]repo.OrgMember, error) {
	return list[repo.OrgMember](ctx, s,
		`SELECT m.org_id, m.user_id, m.role, m.created_at, u.email, u.name, u.avatar_path
		 FROM org_members m JOIN users u ON u.id = m.user_id
		 WHERE m.org_id = ? ORDER BY u.name`, orgID)
}

func (s *Store) GetOrgMember(ctx context.Context, orgID, userID string) (*repo.OrgMember, error) {
	return get[repo.OrgMember](ctx, s,
		`SELECT org_id, user_id, role, created_at, '' AS email, '' AS name, '' AS avatar_path
		 FROM org_members WHERE org_id = ? AND user_id = ?`, orgID, userID)
}

func (s *Store) UpsertOrgMember(ctx context.Context, m *repo.OrgMember) error {
	_, err := s.db.NamedExecContext(ctx,
		`INSERT INTO org_members (org_id, user_id, role, created_at)
		 VALUES (:org_id, :user_id, :role, :created_at)
		 ON CONFLICT (org_id, user_id) DO UPDATE SET role = excluded.role`, m)
	return err
}

func (s *Store) DeleteOrgMember(ctx context.Context, orgID, userID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM org_members WHERE org_id = ? AND user_id = ?`, orgID, userID)
	return err
}

func (s *Store) CreateInvite(ctx context.Context, i *repo.Invite) error {
	_, err := s.db.NamedExecContext(ctx,
		`INSERT INTO invites (id, org_id, email, role, created_by, created_at, expires_at, used_at)
		 VALUES (:id, :org_id, :email, :role, :created_by, :created_at, :expires_at, :used_at)`, i)
	return err
}

func (s *Store) GetInvite(ctx context.Context, id string) (*repo.Invite, error) {
	return get[repo.Invite](ctx, s, `SELECT * FROM invites WHERE id = ?`, id)
}

// rowid rather than created_at, see ListDeploymentsByTile / stacks.go.
func (s *Store) ListInvitesByOrg(ctx context.Context, orgID string) ([]repo.Invite, error) {
	return list[repo.Invite](ctx, s,
		`SELECT * FROM invites WHERE org_id = ? AND used_at IS NULL ORDER BY rowid DESC`, orgID)
}

func (s *Store) MarkInviteUsed(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE invites SET used_at = CURRENT_TIMESTAMP WHERE id = ?`, id)
	return err
}

func (s *Store) DeleteInvite(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM invites WHERE id = ?`, id)
	return err
}

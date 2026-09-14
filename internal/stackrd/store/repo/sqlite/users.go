package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// userCols is the explicit select list, shared so a new column can never be
// added to one lookup and forgotten in the other.
const userCols = `id, email, password_hash, name, role, active, notify_prefs, graph_prefs, theme, avatar_path, created_at, updated_at`

func (s *Store) GetUserByID(ctx context.Context, id string) (*repo.User, error) {
	var u repo.User
	err := s.db.GetContext(ctx, &u, `SELECT `+userCols+` FROM users WHERE id = ?`, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &u, nil
}

func (s *Store) GetUserByEmail(ctx context.Context, email string) (*repo.User, error) {
	var u repo.User
	err := s.db.GetContext(ctx, &u, `SELECT `+userCols+` FROM users WHERE email = ?`, email)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &u, nil
}

func (s *Store) CreateUser(ctx context.Context, user *repo.User) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO users (id, email, password_hash, name, role, active, notify_prefs, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		user.ID, user.Email, user.PasswordHash, user.Name, user.Role,
		user.Active, user.NotifyPrefs, user.CreatedAt, user.UpdatedAt,
	)
	return err
}

// UpdateUser also writes email and notification preferences, the account
// screen edits both, and leaving them out silently discarded the change.
func (s *Store) UpdateUser(ctx context.Context, user *repo.User) error {
	_, err := s.db.NamedExecContext(ctx,
		`UPDATE users SET email = :email, name = :name, role = :role, active = :active,
		 password_hash = :password_hash, notify_prefs = :notify_prefs,
		 graph_prefs = :graph_prefs,
		 theme = :theme,
		 avatar_path = :avatar_path,
		 updated_at = :updated_at WHERE id = :id`, user)
	return err
}

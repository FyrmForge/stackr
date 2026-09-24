package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/FyrmForge/stackr/internal/repo"
)

func (s *Store) GetUserByID(ctx context.Context, id string) (*repo.User, error) {
	var u repo.User
	err := s.db.GetContext(ctx, &u,
		`SELECT id, email, password_hash, name, role, active, created_at, updated_at
		 FROM users WHERE id = ?`, id)
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
	err := s.db.GetContext(ctx, &u,
		`SELECT id, email, password_hash, name, role, active, created_at, updated_at
		 FROM users WHERE email = ?`, email)
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
		`INSERT INTO users (id, email, password_hash, name, role, active, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		user.ID, user.Email, user.PasswordHash, user.Name, user.Role,
		user.Active, user.CreatedAt, user.UpdatedAt,
	)
	return err
}

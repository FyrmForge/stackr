package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/FyrmForge/hamr/pkg/auth"
	"github.com/jmoiron/sqlx"
)

// Store implements repo.Store using SQLite.
type Store struct {
	db *sqlx.DB
}

// NewStore creates a new SQLite-backed store.
func NewStore(db *sqlx.DB) *Store {
	return &Store{db: db}
}

// Health checks the database connection.
func (s *Store) Health(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

// DB returns the underlying database connection for use in queries.
func (s *Store) DB() *sqlx.DB {
	return s.db
}

// ---------------------------------------------------------------------------
// auth.SessionStore implementation
// ---------------------------------------------------------------------------

// Create inserts a new session row.
// To store extra metadata, add your own columns to the sessions table and
// populate them here, or store a JSON blob if you prefer.
func (s *Store) Create(ctx context.Context, session *auth.Session) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (id, subject_id, token, expires_at, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		session.ID, session.SubjectID, session.Token,
		session.ExpiresAt, session.CreatedAt,
	)
	return err
}

func (s *Store) GetByToken(ctx context.Context, token string) (*auth.Session, error) {
	var (
		id        string
		subjectID sql.NullString
		tok       string
		expiresAt time.Time
		createdAt time.Time
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, subject_id, token, expires_at, created_at
		 FROM sessions WHERE token = ?`, token,
	).Scan(&id, &subjectID, &tok, &expiresAt, &createdAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &auth.Session{
		ID:        id,
		SubjectID: subjectID.String,
		Token:     tok,
		ExpiresAt: expiresAt,
		CreatedAt: createdAt,
	}, nil
}

func (s *Store) Delete(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
	return err
}

func (s *Store) DeleteBySubjectID(ctx context.Context, subjectID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE subject_id = ?`, subjectID)
	return err
}

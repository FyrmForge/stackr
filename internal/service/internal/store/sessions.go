package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/FyrmForge/hamr/pkg/auth"
)

// SessionStore is hamr's contract; its session manager is the only writer
// besides leaf/user closing a user's sessions.
type SessionStore = auth.SessionStore

type sessions struct{ q querier }

func (s sessions) Create(ctx context.Context, se *auth.Session) error {
	_, err := s.q.ExecContext(ctx,
		`INSERT INTO sessions (id, subject_id, token, expires_at, created_at) VALUES (?, ?, ?, ?, ?)`,
		se.ID, se.SubjectID, se.Token, se.ExpiresAt, se.CreatedAt)
	return mapErr(err)
}

// GetByToken answers (nil, nil) for an unknown token: hamr's contract.
func (s sessions) GetByToken(ctx context.Context, token string) (*auth.Session, error) {
	var (
		se  auth.Session
		sub sql.NullString
	)
	err := s.q.QueryRowxContext(ctx,
		`SELECT id, subject_id, token, expires_at, created_at FROM sessions WHERE token = ?`, token,
	).Scan(&se.ID, &sub, &se.Token, &se.ExpiresAt, &se.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	se.SubjectID = sub.String
	return &se, nil
}

func (s sessions) Delete(ctx context.Context, id string) error {
	_, err := s.q.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
	return err
}

func (s sessions) DeleteBySubjectID(ctx context.Context, subjectID string) error {
	_, err := s.q.ExecContext(ctx, `DELETE FROM sessions WHERE subject_id = ?`, subjectID)
	return err
}

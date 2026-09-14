package sqlite

import (
	"context"
	"database/sql"

	"github.com/FyrmForge/stackr/internal/stackrd/config/secrets"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

func (s *Store) CreateSecretLink(ctx context.Context, l *repo.SecretLink) error {
	_, err := s.db.NamedExecContext(ctx,
		`INSERT INTO secret_links (id, kind, owner_kind, owner_id, label, token_hash, pass_hash,
		   fields, state, attempts, window_minutes, expires_at, opened_at, created_by, created_at)
		 VALUES (:id, :kind, :owner_kind, :owner_id, :label, :token_hash, :pass_hash,
		   :fields, :state, :attempts, :window_minutes, :expires_at, :opened_at, :created_by, :created_at)`, l)
	return err
}

func (s *Store) GetSecretLinkByHash(ctx context.Context, tokenHash string) (*repo.SecretLink, error) {
	return get[repo.SecretLink](ctx, s, `SELECT * FROM secret_links WHERE token_hash = ?`, tokenHash)
}

// ListSecretLinks is the operator's view of one scope, newest first. Dead rows
// are kept: the row names fields but never holds values, so it is the audit
// line for the exchange.
func (s *Store) ListSecretLinks(ctx context.Context, ownerKind, ownerID string) ([]repo.SecretLink, error) {
	return list[repo.SecretLink](ctx, s,
		`SELECT * FROM secret_links WHERE owner_kind = ? AND owner_id = ? ORDER BY created_at DESC`,
		ownerKind, ownerID)
}

// ClaimSecretLink flips an open link to state and reports whether this caller
// won the race. Callers must act only when it returns true, checking the
// state and then writing would let two concurrent submits both through.
func (s *Store) ClaimSecretLink(ctx context.Context, id, state string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE secret_links SET state = ? WHERE id = ? AND state = ?`, state, id, repo.LinkOpen)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// BurnDropLink is ClaimSecretLink(burned) plus the variable writes in one
// transaction: a failed write rolls the burn back too, so the link stays open
// and the sender can resubmit, never a burned link with half the values.
func (s *Store) BurnDropLink(ctx context.Context, id string, vars []repo.Variable) (bool, error) {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	res, err := tx.ExecContext(ctx,
		`UPDATE secret_links SET state = ? WHERE id = ? AND state = ?`, repo.LinkBurned, id, repo.LinkOpen)
	if err != nil {
		return false, err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return false, err
	}
	for i := range vars {
		enc := vars[i]
		enc.Value = secrets.Encrypt(enc.Value)
		if _, err := tx.NamedExecContext(ctx,
			`INSERT INTO variables (owner_kind, owner_id, name, value, secret, created_at, updated_at)
			 VALUES (:owner_kind, :owner_id, :name, :value, :secret, :created_at, :updated_at)
			 ON CONFLICT (owner_kind, owner_id, name) DO UPDATE SET
			   value = excluded.value, secret = excluded.secret, updated_at = excluded.updated_at`, &enc); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

// TouchSecretLink writes back the two fields that move without burning the
// link: the failed-passphrase count and the first-access stamp. Callers pass
// the values they read, so "stamp only on first access" is decided in the
// service, not here.
func (s *Store) TouchSecretLink(ctx context.Context, id string, attempts int, openedAt sql.NullTime) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE secret_links SET attempts = ?, opened_at = ? WHERE id = ?`, attempts, openedAt, id)
	return err
}

func (s *Store) DeleteSecretLink(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM secret_links WHERE id = ?`, id)
	return err
}

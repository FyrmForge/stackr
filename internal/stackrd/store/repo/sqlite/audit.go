package sqlite

import (
	"context"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Audit events record every read and write of a secret variable's value.
// Writes are fire-and-forget from the caller's point of view, an audit
// insert failing must never block the action it describes, so callers log
// errors instead of returning them.

func (s *Store) AddAuditEvent(ctx context.Context, e *repo.AuditEvent) error {
	_, err := s.db.NamedExecContext(ctx,
		`INSERT INTO audit_events (actor, action, owner_kind, owner_id, name)
		 VALUES (:actor, :action, :owner_kind, :owner_id, :name)`, e)
	return err
}

// rowid rather than created_at, see ListDeploymentsByTile / stacks.go.
func (s *Store) ListAuditEvents(ctx context.Context, ownerKind, ownerID string, limit int) ([]repo.AuditEvent, error) {
	return list[repo.AuditEvent](ctx, s,
		`SELECT * FROM audit_events WHERE owner_kind = ? AND owner_id = ? ORDER BY rowid DESC LIMIT ?`,
		ownerKind, ownerID, limit)
}

// ListAllAuditEvents is the admin view: every secret read and write on the
// server, newest first, optionally narrowed to one actor or one action.
//
// Server-wide and so admin-only: the per-owner listing above is what a tenant
// sees, and it is already scoped by the owner they are looking at.
func (s *Store) ListAllAuditEvents(ctx context.Context, actor, action string, limit int) ([]repo.AuditEvent, error) {
	q := `SELECT * FROM audit_events WHERE 1 = 1`
	var args []any
	if actor != "" {
		q += ` AND actor = ?`
		args = append(args, actor)
	}
	if action != "" {
		q += ` AND action = ?`
		args = append(args, action)
	}
	q += ` ORDER BY rowid DESC LIMIT ?`
	args = append(args, limit)
	return list[repo.AuditEvent](ctx, s, q, args...)
}

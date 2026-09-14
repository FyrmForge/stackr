package sqlite

import (
	"context"
	"time"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Notifications are per-recipient: every query is scoped by user_id, so one
// person reading or clearing their centre never touches anyone else's.

func (s *Store) CreateNotification(ctx context.Context, n *repo.Notification) error {
	_, err := s.db.NamedExecContext(ctx,
		`INSERT INTO notifications (id, user_id, kind, title, body, link, read, created_at)
		 VALUES (:id, :user_id, :kind, :title, :body, :link, :read, :created_at)`, n)
	return err
}

// rowid rather than created_at, see ListDeploymentsByTile / stacks.go.
func (s *Store) ListNotifications(ctx context.Context, userID string, limit int) ([]repo.Notification, error) {
	return list[repo.Notification](ctx, s,
		`SELECT * FROM notifications WHERE user_id = ? ORDER BY rowid DESC LIMIT ?`, userID, limit)
}

func (s *Store) CountUnreadNotifications(ctx context.Context, userID string) (int, error) {
	var n int
	err := s.db.GetContext(ctx, &n,
		`SELECT COUNT(*) FROM notifications WHERE user_id = ? AND read = 0`, userID)
	return n, err
}

func (s *Store) MarkAllNotificationsRead(ctx context.Context, userID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE notifications SET read = 1 WHERE user_id = ? AND read = 0`, userID)
	return err
}

func (s *Store) DeleteAllNotifications(ctx context.Context, userID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM notifications WHERE user_id = ?`, userID)
	return err
}

func (s *Store) PruneNotifications(ctx context.Context, before time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM notifications WHERE created_at < ?`, before.UTC())
	return err
}

package service

import (
	"context"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// NotificationService reads and clears a person's notification list.
//
// Writing one is notify.Notifier's: a notification is raised by whatever
// happened, and the fan-out to the websocket is part of raising it. What was
// missing was a home for the other half — the bell count, the list, and the
// two clears — which three handlers read from the store directly.
//
// Every row belongs to exactly one recipient, so every method here takes the
// user id and none of them takes an org: there is no such thing as somebody
// else's notification to read.
type NotificationService struct {
	store repo.Store
}

func NewNotificationService(store repo.Store) *NotificationService {
	return &NotificationService{store: store}
}

// List is a person's most recent notifications.
func (s *NotificationService) List(ctx context.Context, userID string, limit int) ([]repo.Notification, error) {
	return s.store.ListNotifications(ctx, userID, limit)
}

// Unread is the bell count.
func (s *NotificationService) Unread(ctx context.Context, userID string) (int, error) {
	return s.store.CountUnreadNotifications(ctx, userID)
}

// MarkAllRead clears the bell without losing the list.
func (s *NotificationService) MarkAllRead(ctx context.Context, userID string) error {
	return s.store.MarkAllNotificationsRead(ctx, userID)
}

// DeleteAll empties the list.
func (s *NotificationService) DeleteAll(ctx context.Context, userID string) error {
	return s.store.DeleteAllNotifications(ctx, userID)
}

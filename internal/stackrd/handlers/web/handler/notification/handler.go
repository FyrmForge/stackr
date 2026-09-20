// Package notification renders the in-app notification center and the
// live-updating rail bell badge. Everything here is scoped to the signed-in
// user, the centre used to be global, so one person opening it marked it read
// for everybody.
package notification

import (
	"net/http"

	"github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/service"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

type handler struct {
	store         repo.Store
	notifications *service.NotificationService
}

// NewHandler creates a new notification handler.
func NewHandler(store repo.Store) *handler {
	return &handler{store: store}
}

// GET /notifications, the center. Viewing marks everything read (the list
// still highlights what was unread when you opened it).
func (h *handler) Page(c echo.Context) error {
	ctx := c.Request().Context()
	me := middleware.GetSubjectID(c)
	ns, err := h.notifications.List(ctx, me, 100)
	if err != nil {
		return err
	}
	_ = h.notifications.MarkAllRead(ctx, me)
	return respond.HTML(c, http.StatusOK, notificationsPage(c, ns))
}

// POST /notifications/read, explicit mark-all-read (viewing already does
// this; the button exists for peace of mind and updates the badge instantly).
func (h *handler) MarkAllRead(c echo.Context) error {
	if err := h.notifications.MarkAllRead(c.Request().Context(), middleware.GetSubjectID(c)); err != nil {
		return err
	}
	return respond.Redirect(c, "/notifications")
}

// POST /notifications/clear, delete everything of mine.
func (h *handler) Clear(c echo.Context) error {
	if err := h.notifications.DeleteAll(c.Request().Context(), middleware.GetSubjectID(c)); err != nil {
		return err
	}
	return respond.Redirect(c, "/notifications")
}

// GET /notifications/badge, the rail bell fragment with unread count.
func (h *handler) Badge(c echo.Context) error {
	n, _ := h.notifications.Unread(c.Request().Context(), middleware.GetSubjectID(c))
	return respond.HTML(c, http.StatusOK, Bell(c, n))
}

// WithNotifications gives the page the notification list.
func (h *handler) WithNotifications(v *service.NotificationService) *handler {
	h.notifications = v
	return h
}

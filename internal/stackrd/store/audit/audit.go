// Package audit records reads and writes of secret variable values. An event
// is best-effort by design: losing one audit row is better than blocking the
// action it describes, so Record logs failures instead of returning them.
package audit

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v4"

	stackrmw "github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Actor names the logged-in user for the web surfaces; API and share-link
// callers build their own actor strings ("api:<key>", "share-link:<id>").
func Actor(c echo.Context) string {
	if u := stackrmw.CurrentUser(c); u != nil {
		return u.Email
	}
	return "?"
}

// Actions. "Read" actions mean the plaintext left the server.
const (
	Reveal = "reveal" // shown unmasked in the UI
	Copy   = "copy"   // fetched for the clipboard
	Edit   = "edit"   // pre-filled into an edit form
	Read   = "read"   // listed unmasked over the API
	Share  = "share"  // revealed through a one-time share link
	Set    = "set"    // value written
	Delete = "delete" // variable removed
)

// PanelViews logs the panel renders that put a secret's plaintext in the
// markup, an explicit ?reveal= or an ?edit= pre-fill. Copy never touches the
// DOM; it fetches from ServeValue, which logs itself.
func PanelViews(c echo.Context, s repo.Store, vars []repo.Variable, ownerKind, ownerID string) {
	ctx := c.Request().Context()
	for _, v := range vars {
		if !v.Secret || v.Value == "" {
			continue
		}
		if v.Name == c.QueryParam("reveal") {
			Record(ctx, s, Actor(c), Reveal, ownerKind, ownerID, v.Name)
		}
		if v.Name == c.QueryParam("edit") {
			Record(ctx, s, Actor(c), Edit, ownerKind, ownerID, v.Name)
		}
	}
}

// ServeValue finds ?name= in the scope's variables, records the read under
// the action the caller's button performs, and returns the plaintext. Copy
// and the tile tab's Reveal share this endpoint but are not the same act.
func ServeValue(c echo.Context, s repo.Store, vars []repo.Variable, ownerKind, ownerID, action string) error {
	name := c.QueryParam("name")
	for _, v := range vars {
		if v.Name == name {
			Record(c.Request().Context(), s, Actor(c), action, ownerKind, ownerID, name)
			return c.String(http.StatusOK, v.Value)
		}
	}
	return echo.NewHTTPError(http.StatusNotFound, "no such variable")
}

func Record(ctx context.Context, s repo.Store, actor, action, ownerKind, ownerID, name string) {
	e := &repo.AuditEvent{Actor: actor, Action: action, OwnerKind: ownerKind, OwnerID: ownerID, Name: name}
	if err := s.AddAuditEvent(ctx, e); err != nil {
		slog.Error("audit event dropped", "action", action, "owner", ownerKind+":"+ownerID, "name", name, "err", err)
	}
}

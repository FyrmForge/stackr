package middleware

// The echo-aware half of the audit trail. It lives here rather than in
// store/audit because that package was importing handlers/middleware to find
// the current user, which pointed a store-level package at the HTTP layer and
// closed a cycle the moment the service layer needed to record an event.
// store/audit is now pure: a Record and a vocabulary.

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/store/audit"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// AuditActor names the logged-in user for the web surfaces; API and
// share-link callers build their own actor strings ("api:<key>",
// "share-link:<id>").
func AuditActor(c echo.Context) string {
	if u := CurrentUser(c); u != nil {
		return u.Email
	}
	return "?"
}

// AuditPanelViews logs the panel renders that put a secret's plaintext in the
// markup, an explicit ?reveal= or an ?edit= pre-fill. Copy never touches the
// DOM; it fetches from AuditServeValue, which logs itself.
func AuditPanelViews(c echo.Context, s repo.Store, vars []repo.Variable, ownerKind, ownerID string) {
	ctx := c.Request().Context()
	for _, v := range vars {
		if !v.Secret || v.Value == "" {
			continue
		}
		if v.Name == c.QueryParam("reveal") {
			audit.Record(ctx, s, AuditActor(c), audit.Reveal, ownerKind, ownerID, v.Name)
		}
		if v.Name == c.QueryParam("edit") {
			audit.Record(ctx, s, AuditActor(c), audit.Edit, ownerKind, ownerID, v.Name)
		}
	}
}

// AuditServeValue finds ?name= in the scope's variables, records the read
// under the action the caller's button performs, and returns the plaintext.
// Copy and the tile tab's Reveal share this endpoint but are not the same act.
func AuditServeValue(c echo.Context, s repo.Store, vars []repo.Variable, ownerKind, ownerID, action string) error {
	name := c.QueryParam("name")
	for _, v := range vars {
		if v.Name == name {
			audit.Record(c.Request().Context(), s, AuditActor(c), action, ownerKind, ownerID, name)
			return c.String(http.StatusOK, v.Value)
		}
	}
	return echo.NewHTTPError(http.StatusNotFound, "no such variable")
}

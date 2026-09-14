package web

import (
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/hamr/pkg/respond"

	"github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
	"github.com/FyrmForge/stackr/internal/stackrd/handlers/web/components"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// setupPending answers the middleware.SetupPending that the access checks
// raise for an org still in the onboarding wizard.
//
// The owner goes to the wizard's summary, the same place the settings gate has
// always sent them, and the only screen that can finish the org. Everyone else
// gets a holding page rather than the 404 tenancy would give: a member was
// invited here and knows the org exists, so not-found just reads as broken.
//
// Registered inside htmxErrors so it sees the error first, and answering with
// respond.Redirect means an htmx post gets HX-Redirect instead of the wizard
// swapped into a form's target.
func setupPending(store repo.Store) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			err := next(c)
			var sp middleware.SetupPending
			if !errors.As(err, &sp) || c.Response().Committed {
				return err
			}
			if setupOwner(c, store, sp.OrgID) {
				// No flash: the summary's own heading is "Finish setting up
				// <org>", so a toast saying the same thing is the sentence
				// twice.
				return respond.Redirect(c, "/orgs/"+sp.Slug+"/setup/done")
			}
			name := sp.Name
			if name == "" {
				name = sp.Slug
			}
			return respond.HTML(c, http.StatusOK, components.SetupHoldingPage(c, name))
		}
	}
}

// setupOwner is the role check in the org the request addressed, which is not
// necessarily the active one OrgRole answers for.
func setupOwner(c echo.Context, store repo.Store, orgID string) bool {
	if middleware.IsAdmin(c) {
		return true
	}
	u := middleware.CurrentUser(c)
	if u == nil {
		return false
	}
	m, err := store.GetOrgMember(c.Request().Context(), orgID, u.ID)
	return err == nil && m != nil && m.Role == "owner"
}

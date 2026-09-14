package middleware

import (
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/hamr/pkg/respond"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// RequireSetupDone closes an org's settings until its owner has finished the
// onboarding wizard. Without it the wizard is a suggestion: every step links to
// a settings tab, so one click ejects you from the flow and the steps behind
// you are never marked done.
//
// The owner goes to the wizard's summary rather than to a guessed "first
// unfinished" step, every step is skippable, so guessing would drop people
// back onto steps they skipped on purpose. A member gets SetupPending, which
// the web renders as the holding page, so settings and the canvas give them
// the same answer; someone who is in neither still gets not-found.
//
// This group is no longer the only gate: RequireOrgSetup below runs from the
// access checks, so the canvas, the plans pages and every stack under the org
// are closed too (plan 29). It stays because the settings routes are a group
// and a group is the cheapest place to stop them.
func RequireSetupDone(store repo.Store) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			key := c.Param("slug")
			if key == "" {
				key = c.Param("id")
			}
			ctx := c.Request().Context()
			o, err := store.GetOrgBySlug(ctx, key)
			if err != nil {
				return err
			}
			if o == nil {
				if o, err = store.GetOrg(ctx, key); err != nil {
					return err
				}
			}
			if o == nil || o.SetupDoneAt != nil {
				return next(c) // unknown org: let the handler produce the 404
			}
			u := CurrentUser(c)
			if u == nil {
				return next(c) // not logged in: auth answers first
			}
			if !IsAdmin(c) {
				m, err := store.GetOrgMember(ctx, o.ID, u.ID)
				if err != nil {
					return err
				}
				if m == nil {
					return echo.NewHTTPError(http.StatusNotFound, "org not found")
				}
				if m.Role != "owner" {
					// A member gets the same holding page the rest of the org gives
					// them, not a 404 here and "still being set up" one click away.
					// They were invited; not-found only looks broken.
					return SetupPending{OrgID: o.ID, Slug: o.Slug, Name: o.Name}
				}
			}
			// respond.Redirect, not c.Redirect: a settings form posted by htmx
			// needs the HX-Redirect header or the wizard lands in a swap target.
			return respond.Redirect(c, "/orgs/"+o.Slug+"/setup/done")
		}
	}
}

// SetupPending is what an access check returns when the org that owns the
// resource has not been through the onboarding wizard. It is an error and not
// a redirect written on the spot for two reasons: the checks it comes out of
// return errors, and the web and the API answer it differently, the web sends
// the owner back to the wizard, the API refuses with a 409, because a 303 to an
// HTML page is a parse error to the CLI.
type SetupPending struct {
	OrgID string
	Slug  string
	Name  string
}

func (e SetupPending) Error() string { return "organization setup is not finished" }

// setupOpenRoutes are the routes the wizard needs while the org it is
// configuring is, by definition, unfinished. Matched on the registered route
// pattern rather than the request path, so no slug can ever look like one.
//
// These two are the config step's repo picker: it renders inside the wizard but
// posts to the settings handlers, which is why they sit outside the settings
// group in the route table.
var setupOpenRoutes = map[string]bool{
	"/orgs/:slug/settings/connectors/:connectorID/branches": true,
	"/orgs/:slug/settings/connectors/:connectorID/file":     true,
	// Discard, on the wizard's summary. Without it the only way out of a draft
	// is to finish it: every other org route bounces the owner back here.
	"/orgs/:slug/delete": true,
}

func setupOpen(c echo.Context) bool {
	p := c.Path()
	return strings.HasPrefix(p, "/orgs/:slug/setup") || setupOpenRoutes[p]
}

// RequireOrgSetup refuses a request addressed at an org whose wizard is still
// open. Called from the access checks every org- and stack-addressed route
// already goes through, so gating is one condition rather than a route group
// the id-keyed half of the route table could never have joined.
func RequireOrgSetup(c echo.Context, o repo.Org) error {
	if o.SetupDoneAt != nil || setupOpen(c) {
		return nil
	}
	return SetupPending{OrgID: o.ID, Slug: o.Slug, Name: o.Name}
}

package middleware

import (
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	hamrmw "github.com/FyrmForge/hamr/pkg/middleware"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Context keys set by OrgContext for every site request.
const (
	CtxOrgs = "orgs"     // []repo.Org, the user's organizations
	CtxOrg  = "org"      // repo.Org, the active one
	CtxRole = "org_role" // string, the user's role in the active org
	// CtxWriteHere is set by the access checks below: the user's write right in
	// the org that owns the resource this request addresses, which is not
	// necessarily the active one.
	CtxWriteHere = "org_write_here"
)

// OrgCookie stores the active organization's id.
const OrgCookie = "stackr_org"

// SetupCookie marks a GitHub connector round trip that started in the
// onboarding wizard, so the callback can return there instead of to settings.
// It lives here because both ends of that trip are in different handler
// packages.
const SetupCookie = "stackr_setup"

// CurrentUser pulls the authenticated user loaded by BrowserAuth (nil when
// logged out).
func CurrentUser(c echo.Context) *repo.User {
	u, _ := hamrmw.GetSubject(c).(*repo.User)
	return u
}

func currentUser(c echo.Context) *repo.User { return CurrentUser(c) }

// IsAdmin reports whether the request's user is a server admin.
func IsAdmin(c echo.Context) bool {
	u := currentUser(c)
	return u != nil && u.Role == "admin"
}

// SecureCookie follows the request scheme for non-session browser cookies.
// TLS terminates at Traefik in production, so X-Forwarded-Proto is the usual
// signal; direct TLS remains useful in tests and alternate deployments.
func SecureCookie(c echo.Context) bool {
	return c.Request().TLS != nil || strings.EqualFold(c.Request().Header.Get("X-Forwarded-Proto"), "https")
}

// OrgContext resolves the user's organizations (admins see all), the active
// one (cookie, else first), and the user's role in it.
func OrgContext(store repo.Store) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			u := currentUser(c)
			if u == nil {
				return next(c)
			}
			ctx := c.Request().Context()
			var orgs []repo.Org
			var err error
			if u.Role == "admin" {
				orgs, err = store.ListOrgs(ctx)
			} else {
				orgs, err = store.ListOrgsForUser(ctx, u.ID)
			}
			if err != nil || len(orgs) == 0 {
				return next(c)
			}
			// An unfinished draft is gated everywhere but its own wizard, so
			// landing on one as the active org means every page bounces. Prefer
			// a finished org; fall back to the first when they are all drafts,
			// which is a fresh install mid-wizard.
			active := orgs[0]
			for _, o := range orgs {
				if o.SetupDoneAt != nil {
					active = o
					break
				}
			}
			if ck, err := c.Cookie(OrgCookie); err == nil {
				for _, o := range orgs {
					if o.ID == ck.Value {
						active = o
						break
					}
				}
			}
			role := "viewer"
			if u.Role == "admin" {
				role = "owner" // admins act as owner everywhere
			} else if m, err := store.GetOrgMember(ctx, active.ID, u.ID); err == nil && m != nil {
				role = m.Role
			}
			c.Set(CtxOrgs, orgs)
			c.Set(CtxOrg, active)
			c.Set(CtxRole, role)
			return next(c)
		}
	}
}

// ActiveOrg returns the active organization, ok=false outside OrgContext.
func ActiveOrg(c echo.Context) (repo.Org, bool) {
	o, ok := c.Get(CtxOrg).(repo.Org)
	return o, ok
}

// Orgs returns the user's organizations loaded by OrgContext.
func Orgs(c echo.Context) []repo.Org {
	os, _ := c.Get(CtxOrgs).([]repo.Org)
	return os
}

// OrgRole returns the user's role in the active org ("" outside OrgContext).
func OrgRole(c echo.Context) string {
	r, _ := c.Get(CtxRole).(string)
	return r
}

// IsOwner reports owner rights on the active org (admins included).
func IsOwner(c echo.Context) bool { return OrgRole(c) == "owner" }

// CanWrite reports content-write rights on the active org.
//
// A server admin always may. CanWriteOrg has always said so; this said the
// opposite, and the gap showed on the server-admin screens, which are not
// org-addressed at all: on a fresh install nobody has an org yet, so the
// active role is "" and every POST under /servers, add a node, drain one,
// save a volume, came back "read-only access" to the one account that owns
// the machine.
func CanWrite(c echo.Context) bool {
	if IsAdmin(c) {
		return true
	}
	r := OrgRole(c)
	return r == "owner" || r == "member"
}

// InOrg reports whether the user belongs to (or, for admins, may act on) the
// org, the tenancy check for id-addressed resources.
func InOrg(c echo.Context, orgID string) bool {
	if IsAdmin(c) {
		return true
	}
	for _, o := range Orgs(c) {
		if o.ID == orgID {
			return true
		}
	}
	return false
}

// RequireOrgAccess is InOrg as an error: 404 (not 403) so resource ids don't
// leak across tenants. It is also where an unfinished org stops answering:
// every org- and stack-addressed route in the panel comes through here, which
// the settings route group it used to be gated by never could.
//
// Setup state is read off the list OrgContext already loaded (both queries
// select the whole row), so the gate costs no query. An org missing from that
// list means the load failed, which only an admin survives, and the handler
// behind this is about to fail on the same database.
func RequireOrgAccess(c echo.Context, orgID string) error {
	if !InOrg(c, orgID) {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	for _, o := range Orgs(c) {
		if o.ID == orgID {
			return RequireOrgSetup(c, o)
		}
	}
	return nil
}

// mutating reports whether a request changes anything.
func mutating(c echo.Context) bool {
	switch c.Request().Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return true
}

// CanWriteOrg reports content-write rights in one specific org.
//
// CanWrite answers for the *active* org (the one the cookie selects) which
// is the right question for the page the user is looking at and the wrong one
// for a resource addressed by id. Someone who owns org A and only views org B
// carries role "owner" while acting on B's tiles.
func CanWriteOrg(c echo.Context, store repo.Store, orgID string) bool {
	if IsAdmin(c) {
		return true
	}
	u := currentUser(c)
	if u == nil {
		return false
	}
	m, err := store.GetOrgMember(c.Request().Context(), orgID, u.ID)
	return err == nil && m != nil && (m.Role == "owner" || m.Role == "member")
}

// RequireOrgWrite is membership plus write rights in that same org, for
// org-addressed mutations.
func RequireOrgWrite(c echo.Context, store repo.Store, orgID string) error {
	if err := RequireOrgAccess(c, orgID); err != nil {
		return err
	}
	write := CanWriteOrg(c, store, orgID)
	c.Set(CtxWriteHere, write)
	if mutating(c) && !write {
		return echo.NewHTTPError(http.StatusForbidden, "read-only access to this organization")
	}
	return nil
}

// CanWriteHere answers "may this user write to the thing this request is
// about". It reads the right the access check already resolved for the
// resource's own org; outside one it falls back to the active org, which is
// the best a page with no resource can do.
//
// Templates used to ask CanWrite directly and so offered upload and delete
// buttons to a viewer whose cookie pointed at an org they can write.
func CanWriteHere(c echo.Context) bool {
	if w, ok := c.Get(CtxWriteHere).(bool); ok {
		return w
	}
	return CanWrite(c)
}

// RequireStackAccess verifies a stack (by id) belongs to one of the user's
// orgs, the tenancy check for tile-, env-, and deployment-addressed routes.
//
// On a mutating request it also checks write rights in *that stack's* org.
// ReadOnlyGuard cannot: it only knows the active org, so a viewer in the org
// that owns the tile passes it whenever their cookie points somewhere they can
// write. That gap reaches every id-addressed write, volume files, table rows,
// deploys, so the check belongs here, where all of them already come through.
// Admins used to short-circuit before the stack was even loaded. They no
// longer can: the setup gate is keyed on the stack's org, so the org has to be
// known first. RequireOrgWrite waves an admin through everything else it
// checks, including the write right this used to record by hand.
func RequireStackAccess(c echo.Context, store repo.Store, stackID string) error {
	s, err := store.GetStack(c.Request().Context(), stackID)
	if err != nil || s == nil {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	return RequireOrgWrite(c, store, s.OrgID)
}

// ReadOnlyGuard blocks mutating requests from viewers. Session actions that
// belong to the user (logout, org switch) stay allowed.
func ReadOnlyGuard() echo.MiddlewareFunc {
	// Creating an org belongs with the session-level actions, not with content
	// writes: it happens outside any org, and CanWrite reads the role in the
	// active one. On a fresh install nobody has an org yet, so without this the
	// guard blocks the only thing there is to do.
	allowed := map[string]bool{"/logout": true, "/orgs/switch": true, "/orgs": true}
	// The user's own row is not org content. A viewer still has to change their
	// own password, mint their own keys and clear their own notifications, and
	// which org their cookie points at has no bearing on any of it. Prefixes
	// because several of these routes carry an id. Write scopes on an API key
	// are the one thing under /account that needs a right, and CreateAPIKey
	// checks that itself through CanGrantWrite.
	self := func(p string) bool {
		return strings.HasPrefix(p, "/account/") || strings.HasPrefix(p, "/notifications/")
	}
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			m := c.Request().Method
			if m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions {
				return next(c)
			}
			p := c.Request().URL.Path
			if currentUser(c) == nil || allowed[p] || self(p) {
				return next(c)
			}
			if !CanWrite(c) {
				return echo.NewHTTPError(http.StatusForbidden, "read-only access")
			}
			return next(c)
		}
	}
}

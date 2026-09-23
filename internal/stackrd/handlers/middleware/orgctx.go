package middleware

import (
	"errors"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	hamrmw "github.com/FyrmForge/hamr/pkg/middleware"

	"github.com/FyrmForge/stackr/internal/stackrd/service"
	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
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
					if o.ID != ck.Value {
						continue
					}
					// Creating a draft makes it the active org, and this cookie
					// outlives the session: a draft abandoned mid-wizard was
					// still active after a fresh login, with every page in it
					// bouncing to the wizard. A draft only wins while it is the
					// only thing there is; the wizard itself is addressed by
					// slug, not by the active org.
					if o.SetupDoneAt == nil && active.SetupDoneAt != nil {
						break
					}
					active = o
					break
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

// IsOwnerOf reports owner rights in one specific org, the owner-level twin of
// CanWriteOrg and the same distinction: IsOwner answers for the active cookie
// org, this one for the org the page is about.
//
// For deciding whether a page offers a control, not for authorizing the write
// behind it — that is the route's verb.
func IsOwnerOf(c echo.Context, store repo.Store, orgID string) bool {
	if IsAdmin(c) {
		return true
	}
	u := currentUser(c)
	if u == nil {
		return false
	}
	m, err := store.GetOrgMember(c.Request().Context(), orgID, u.ID)
	return err == nil && m != nil && m.Role == "owner"
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

// RequireVerb gates a route on AccessService's level for one verb, resolving
// the org from the :slug or :id param the route carries.
//
// It is the panel's adapter onto the one access table: the level lives in
// service.Verbs, not in a closure here, so a verb both surfaces reach cannot
// end up with two levels again.
func RequireVerb(store repo.Store, access *service.AccessService, v service.Verb) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			u := currentUser(c)
			if u == nil {
				return echo.NewHTTPError(http.StatusNotFound, "not found")
			}
			o, err := orgFromRoute(c, store)
			if err != nil {
				return err
			}
			p, err := access.Principal(c.Request().Context(), u, nil, false)
			if err != nil {
				return err
			}
			if err := access.Require(p, v, o.ID); err != nil {
				return HTTP(err)
			}
			return next(c)
		}
	}
}

// orgFromRoute reads the org a route addresses, by slug or by id.
func orgFromRoute(c echo.Context, store repo.Store) (*repo.Org, error) {
	ctx := c.Request().Context()
	key := c.Param("slug")
	if key == "" {
		key = c.Param("id")
	}
	o, err := store.GetOrgBySlug(ctx, key)
	if err != nil || o == nil {
		o, err = store.GetOrg(ctx, key)
	}
	if err != nil {
		return nil, err
	}
	if o == nil {
		return nil, echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	return o, nil
}

// Gate is point 18's one authorization path for the panel.
//
// It replaces the ~35 hand-written gate helpers with the same three steps for
// every route: resolve who is asking, resolve which org the route is about,
// ask AccessService whether that verb is allowed there. The level lives in
// service.verbLevels and nowhere else, so a verb both surfaces reach cannot
// end up with two levels again.
//
// v and k are declared at the route, not in the handler body, because the
// body is where forgetting happens. param names which path parameter carries
// the reference k resolves — ":id" on most routes, ":slug" on an org, and
// ":planID" where a stack route addresses one of its plans.
//
// Refusals keep the panel's wording: a caller outside the org gets 404, not
// 403, because a 403 confirms the id exists to someone who should not know
// it. That is the surface's decision and it is deliberately not the API's.
func Gate(store repo.Store, access *service.AccessService, v service.Verb, k service.Kind, param string) echo.MiddlewareFunc {
	notFound := echo.NewHTTPError(http.StatusNotFound, "not found")
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			u := currentUser(c)
			if u == nil {
				return notFound
			}
			ctx := c.Request().Context()
			p, err := access.Principal(ctx, u, nil, false)
			if err != nil {
				return err
			}
			// KindDeferred: the org is not addressable yet — it arrives in
			// the body, or it is what the request is asking to resolve. The
			// handler makes the check; see service.KindDeferred for the list.
			if k == service.KindDeferred {
				return next(c)
			}
			var orgID string
			if k != service.KindNone {
				orgID, err = access.TenancyOf(ctx, k, c.Param(param))
				switch {
				case errors.Is(err, service.ErrServerOwned):
					// Exists, belongs to the installation rather than an org
					// (the panel's own backup). Admin-only, and there is no
					// org for the verb to be checked against.
					if !p.Admin {
						return notFound
					}
					return next(c)
				case err != nil:
					return err
				case orgID == "":
					return notFound // no such thing, or not one this route can address
				}
			}
			// An org whose wizard is still open takes no writes. This is
			// not a level, so it has no place in the verb table — but it is
			// what RequireOrgAccess carried alongside membership, and the
			// gate helpers are what point 18 removes. Without it here, an
			// org mid-wizard starts accepting writes the moment its helper
			// goes. The setup routes themselves are exempt (setupOpen).
			if orgID != "" {
				o, err := store.GetOrg(ctx, orgID)
				if err != nil {
					return err
				}
				if o != nil {
					if err := RequireOrgSetup(c, *o); err != nil {
						return err
					}
				}
			}
			if err := access.Require(p, v, orgID); err != nil {
				// A page nobody below this level may see does not announce
				// itself. The panel answers 404 for tenancy and 403 for
				// level, and these two read verbs are where the two meet:
				// the helpers they replace answered "not found" on purpose.
				// ownedSettingsOrg 404'd a member, because the invite list,
				// the org's plans and the wizard are not things a member
				// should learn exist; adminOnly 404'd a non-admin for the
				// same reason, and it still sits behind this gate.
				//
				// Writes keep the 403 — the caller already knows the org and
				// asked to act in it, and so do the write-level reads (the
				// var-value endpoints), which answered 403 before. The API
				// keeps 403 for everything; that split is deliberate and is
				// not to be flattened.
				switch v {
				case service.VerbOrgOwnerRead, service.VerbAdminRead:
					if errors.Is(err, svcerr.ErrForbidden) {
						return notFound
					}
				}
				return HTTP(err)
			}
			// Templates ask CanWriteHere rather than CanWrite so a viewer in
			// the resource's org is not offered buttons because their cookie
			// points somewhere they can write. The gate helpers recorded this;
			// it has to keep being recorded once they are gone.
			if orgID != "" {
				if lvl, ok := p.Level(orgID); ok {
					c.Set(CtxWriteHere, lvl >= service.LevelWrite)
				}
			}
			return next(c)
		}
	}
}

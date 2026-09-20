package v1

import (
	"context"
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"

	stackrmw "github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
	"github.com/FyrmForge/stackr/internal/stackrd/service"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Capability scopes. A key is granted a subset of these at creation (bounded by
// the creator's own access); each route declares the one it requires via op().
const (
	ScopeStacksRead     = "stacks:read"
	ScopeStacksWrite    = "stacks:write"
	ScopeAppsRead       = "apps:read"
	ScopeAppsWrite      = "apps:write"
	ScopeAppsDeploy     = "apps:deploy"
	ScopeVarsRead       = "vars:read"
	ScopeVarsWrite      = "vars:write"
	ScopeSecretsRead    = "secrets:read"
	ScopeLogsRead       = "logs:read"
	ScopeDBsRead        = "dbs:read"
	ScopeDBsWrite       = "dbs:write"
	ScopeDBsFork        = "dbs:fork"
	ScopeDomainsWrite   = "domains:write"
	ScopeEnvsWrite      = "envs:write"
	ScopeConfigRead     = "config:read"
	ScopeConfigApply    = "config:apply"
	ScopeTilesForward   = "tiles:forward"
	ScopeBackupsRead    = "backups:read"
	ScopeBackupsWrite   = "backups:write"
	ScopeBackupsRestore = "backups:restore"
	ScopeStorageRead    = "storage:read"
	ScopeStorageWrite   = "storage:write"
)

// Scope groups, in display order. A group is only a UI heading, nothing is
// enforced at group level, and keys still store the explicit scope list.
const (
	GroupStructure = "Stacks & environments"
	GroupApps      = "Apps & deployments"
	GroupConfig    = "Configuration"
	GroupData      = "Data"
	GroupAccess    = "Access"
)

// ScopeGroups is the order the UI renders headings in. A group with no scopes
// simply doesn't appear.
var ScopeGroups = []string{GroupStructure, GroupApps, GroupConfig, GroupData, GroupAccess}

// ScopeInfo describes a capability for the key-creation UI.
type ScopeInfo struct {
	Name  string
	Desc  string
	Write bool // requires content-write access to grant
	Group string
}

// Scopes is the full capability catalog, in display order.
var Scopes = []ScopeInfo{
	{ScopeStacksRead, "List and read stacks", false, GroupStructure},
	{ScopeStacksWrite, "Create and delete stacks", true, GroupStructure},
	{ScopeEnvsWrite, "Create environments", true, GroupStructure},
	{ScopeAppsRead, "List and read apps", false, GroupApps},
	{ScopeAppsWrite, "Create, update, and delete apps", true, GroupApps},
	{ScopeAppsDeploy, "Trigger deployments", true, GroupApps},
	{ScopeLogsRead, "Read logs", false, GroupApps},
	{ScopeVarsRead, "Read environment variables", false, GroupConfig},
	{ScopeVarsWrite, "Replace environment variables", true, GroupConfig},
	{ScopeSecretsRead, "Reveal secret values and resolve references", true, GroupConfig},
	{ScopeDomainsWrite, "Add and remove domains", true, GroupConfig},
	{ScopeConfigRead, "Read config-as-code plans", false, GroupConfig},
	// Separate from every other write scope: approving a plan applies whatever
	// the config file says, deletes included, on an environment whose apply
	// policy is deliberately manual.
	{ScopeConfigApply, "Re-plan, approve, and reject config-as-code plans", true, GroupConfig},
	{ScopeDBsRead, "List and read databases", false, GroupData},
	{ScopeDBsWrite, "Create and delete databases", true, GroupData},
	// Separate from write: a fork reads every byte of a live database and
	// writes a second copy of it, which creating an empty one does not.
	{ScopeDBsFork, "Fork a database slice, copying its data", true, GroupData},
	{ScopeBackupsRead, "List backup schedules and run history", false, GroupData},
	{ScopeBackupsWrite, "Configure backups and trigger runs", true, GroupData},
	// Separate from write: a restore overwrites live data with an archive, and
	// nothing about it is undoable from the panel.
	{ScopeBackupsRestore, "Restore data from a backup (destructive)", true, GroupData},
	{ScopeStorageRead, "List storage shares/pools and their sub-paths", false, GroupData},
	// Server-wide: creating storage carries share credentials and host paths.
	{ScopeStorageWrite, "Create and delete storage shares/pools and sub-paths", true, GroupData},
	// Write, not read: a tunnel is unrestricted network access to the
	// container, bypassing every proxy-level guard.
	{ScopeTilesForward, "Port-forward a tile to a local machine", true, GroupAccess},
}

// ScopesIn returns the catalog entries in one group, preserving Scopes' order.
func ScopesIn(group string) []ScopeInfo {
	var out []ScopeInfo
	for _, s := range Scopes {
		if s.Group == group {
			out = append(out, s)
		}
	}
	return out
}

// ValidScope reports whether s is a known capability.
func ValidScope(s string) bool {
	for _, sc := range Scopes {
		if sc.Name == s {
			return true
		}
	}
	return false
}

// WriteScope reports whether granting s requires content-write access.
func WriteScope(s string) bool {
	for _, sc := range Scopes {
		if sc.Name == s {
			return sc.Write
		}
	}
	return true // unknown → treat as privileged
}

// echo context keys set by KeyAuth.
const (
	ctxKey    = "apikey"
	ctxUser   = "apiuser"
	ctxOrgIDs = "apiorgs" // map[string]bool of the user's org ids (nil = admin)
	// ctxSetupDone caches org id -> "wizard finished", filled once per request.
	// orgAllowed is called per row by every listing, so a lookup per call was a
	// second query per app, stack, database and destination returned.
	ctxSetupDone = "apisetupdone"
	// ctxPrincipal caches the resolved principal: its roles are a query per
	// org, and a request makes several access checks.
	ctxPrincipal = "apiprincipal"
)

// HashKey returns the stored hash for a raw API token. One implementation,
// in the service that mints them: the authenticator has to hash the header
// with exactly the function that wrote the row.
func HashKey(raw string) string { return service.HashAPIKey(raw) }

// GrantableScopes filters a requested scope list down to what this minter may
// actually hand out. A key must never grant more than the person minting it
// already has, so a write scope needs content-write in the active org (or
// server admin). Unknown scopes are dropped rather than refused: the form
// posts whatever the page rendered, and a stale checkbox is not an error.
//
// It lives beside the catalog it filters against, and is shared by the
// account page and both halves of the CLI login.
func GrantableScopes(requested []string, canWrite bool) []string {
	out := make([]string, 0, len(requested))
	for _, s := range requested {
		if !ValidScope(s) || (WriteScope(s) && !canWrite) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// KeyAuth authenticates via x-api-key and loads the key's user + org access
// into the request context, so scope checks and org tenancy both apply.
func (a *API) KeyAuth(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		raw := c.Request().Header.Get("x-api-key")
		if raw == "" {
			return echo.NewHTTPError(http.StatusUnauthorized, "missing x-api-key")
		}
		ctx := c.Request().Context()
		k, err := a.store.GetAPIKeyByHash(ctx, HashKey(raw))
		if err != nil {
			return err
		}
		if k == nil {
			return echo.NewHTTPError(http.StatusUnauthorized, "invalid api key")
		}
		user, err := a.auth.User(ctx, k.UserID)
		if err != nil || user == nil {
			return echo.NewHTTPError(http.StatusUnauthorized, "api key user no longer exists")
		}
		// Active, not just present. Deactivating a user closed nothing:
		// this checked only that the row existed, so every key they had
		// minted kept working indefinitely.
		if !user.Active {
			return echo.NewHTTPError(http.StatusUnauthorized, "api key user is deactivated")
		}
		c.Set(ctxKey, k)
		c.Set(ctxUser, user)
		if user.Role != "admin" { // admins see all orgs → leave orgIDs nil
			orgs, err := a.orgs.ListForUser(ctx, user.ID)
			if err != nil {
				return err
			}
			ids := make(map[string]bool, len(orgs))
			for _, o := range orgs {
				ids[o.ID] = true
			}
			c.Set(ctxOrgIDs, ids)
		}
		return next(c)
	}
}

// requireScope wraps a handler so it runs only when the key carries the scope.
func (a *API) requireScope(scope string, h echo.HandlerFunc) echo.HandlerFunc {
	if scope == "" {
		return h
	}
	return func(c echo.Context) error {
		k, _ := c.Get(ctxKey).(*repo.APIKey)
		if k == nil || !k.HasScope(scope) {
			return echo.NewHTTPError(http.StatusForbidden, "api key missing scope: "+scope)
		}
		return h(c)
	}
}

func (a *API) user(c echo.Context) *repo.User { u, _ := c.Get(ctxUser).(*repo.User); return u }

func (a *API) isAdmin(c echo.Context) bool {
	u := a.user(c)
	return u != nil && u.Role == "admin"
}

// viewer is who is asking, in the form the service layer takes: the admin
// flag and the orgs this key may act in, with the wizard filter already
// applied.
func (a *API) viewer(c echo.Context) service.Viewer {
	v := service.Viewer{Admin: a.isAdmin(c), Orgs: map[string]bool{}}
	ids, _ := c.Get(ctxOrgIDs).(map[string]bool)
	done := a.setupDone(c)
	for id := range ids {
		if done[id] {
			v.Orgs[id] = true
		}
	}
	return v
}

// adminOnly refuses non-admin keys. Storage and proxy config are panel-wide,
// not org-scoped, so scope alone is not enough.
func (a *API) adminOnly(h echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if !a.isAdmin(c) {
			return echo.NewHTTPError(http.StatusForbidden, "admin only")
		}
		return h(c)
	}
}

// orgMember reports whether the key's user belongs to an org, tenancy alone.
func (a *API) orgMember(c echo.Context, orgID string) bool {
	if a.isAdmin(c) {
		return true
	}
	ids, _ := c.Get(ctxOrgIDs).(map[string]bool)
	return ids[orgID]
}

// orgAllowed is tenancy plus "this org is open for business". An org whose
// onboarding wizard is still running answers nothing but its own wizard, and
// the API is how the CLI would otherwise drive it around the outside. The
// listing endpoints all filter through here, so they drop it silently; the
// single-resource paths call orgReady instead and say why.
func (a *API) orgAllowed(c echo.Context, orgID string) bool {
	if !a.orgMember(c, orgID) {
		return false
	}
	return a.setupDone(c)[orgID]
}

// setupDone is every org's wizard state, resolved once and kept on the request.
func (a *API) setupDone(c echo.Context) map[string]bool {
	if m, ok := c.Get(ctxSetupDone).(map[string]bool); ok {
		return m
	}
	m := map[string]bool{}
	orgs, err := a.orgs.ListAll(c.Request().Context())
	if err == nil {
		for _, o := range orgs {
			m[o.ID] = o.SetupDoneAt != nil
		}
	}
	c.Set(ctxSetupDone, m)
	return m
}

// orgReady refuses an unfinished org with a 409 naming the screen that
// finishes it. Not a redirect: the CLI would render the wizard's HTML as a
// parse error, and not a 404 either, the caller has access, the org is just
// not built yet.
func orgReady(o *repo.Org) error {
	if o == nil || o.SetupDoneAt != nil {
		return nil
	}
	return echo.NewHTTPError(http.StatusConflict,
		"organization setup is not finished: finish it at /orgs/"+o.Slug+"/setup/done in the panel")
}

// userOrgIDs returns the key's user's org ids (nil for admin, means "all").
func (a *API) userOrgIDs(c echo.Context) map[string]bool {
	if a.isAdmin(c) {
		return nil
	}
	ids, _ := c.Get(ctxOrgIDs).(map[string]bool)
	return ids
}

// requireStackAccess loads a stack and 404s unless the key's user's org owns it
// (404 not 403 so ids don't leak across tenants).
func (a *API) requireStackAccess(c echo.Context, stackID string) (*repo.Stack, error) {
	ctx := c.Request().Context()
	s, err := a.stacks.Get(ctx, stackID)
	if err != nil {
		s, err = a.stackByPath(ctx, stackID) // org:stack, see slugpath.go
	}
	if err != nil || s == nil {
		return nil, echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	if !a.orgMember(c, s.OrgID) {
		return nil, echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	o, err := a.orgs.Get(ctx, s.OrgID)
	if err != nil {
		return nil, stackrmw.HTTP(err)
	}
	if err := orgReady(o); err != nil {
		return nil, err
	}
	return s, nil
}

// requireOrgWrite verifies the key's user has content-write (owner/member) in
// the org, the live role check that bounds write scopes.
func (a *API) requireOrgWrite(ctx context.Context, c echo.Context, orgID string) error {
	return a.requireVerb(ctx, c, service.VerbStackWrite, orgID)
}

// requireVerb is the API's adapter onto AccessService: the level a verb needs
// comes from the one table, not from a closure here.
//
// This is the call that makes a key's write scopes mean something. Scopes are
// granted against whichever org the minting browser's cookie pointed at, and a
// key carries no org of its own, so the live role check in the *target* org is
// the only thing standing between a key minted on org A and a write in org B
// where its user is a viewer. Every write route already makes this call; what
// changed is that the level it asks for is shared with the panel.
func (a *API) requireVerb(ctx context.Context, c echo.Context, v service.Verb, orgID string) error {
	if err := a.keyOrgAllows(c, orgID); err != nil {
		return err
	}
	p, err := a.principal(ctx, c)
	if err != nil {
		return err
	}
	return stackrmw.HTTP(a.access.Require(p, v, orgID))
}

// keyOrgAllows refuses a bound key acting outside the org it was minted for.
//
// This is the other half of the comment above: the live role check stops a key
// whose user is a viewer in the target org, and this stops one whose user is a
// member everywhere — the scopes on the key were granted on the strength of
// one org's role and do not travel out of it.
//
// A key with no org is unbound and unaffected, which is every key minted
// before the column existed. 404, not 403, for the same reason the tenancy
// refusals are: a key that may not act here should not learn the org exists.
//
// Two call sites cover every write: here, which the route gate and every
// org-in-the-body create route funnel through, and orgForCreate, which picks
// an org by role instead of asking.
func (a *API) keyOrgAllows(c echo.Context, orgID string) error {
	if orgID == "" {
		return nil // server-wide route, nothing to bind against
	}
	if k, _ := c.Get(ctxKey).(*repo.APIKey); k != nil && k.OrgID.Valid && k.OrgID.String != orgID {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	return nil
}

// principal resolves who is asking, once per request.
func (a *API) principal(ctx context.Context, c echo.Context) (service.Principal, error) {
	if p, ok := c.Get(ctxPrincipal).(service.Principal); ok {
		return p, nil
	}
	var scopes []string
	keyed := false
	if k, _ := c.Get(ctxKey).(*repo.APIKey); k != nil {
		scopes, keyed = k.ScopeList(), true
	}
	p, err := a.access.Principal(ctx, a.user(c), scopes, keyed)
	if err != nil {
		return service.Principal{}, err
	}
	c.Set(ctxPrincipal, p)
	return p, nil
}

// requireTile loads a tile and checks org access; write=true also checks the
// user's live write role in the tile's org.
func (a *API) requireTile(c echo.Context, tileID string, write bool) (*repo.Tile, error) {
	ctx := c.Request().Context()
	t, err := a.tiles.Get(ctx, tileID)
	if err != nil {
		t, err = a.tileByPath(ctx, tileID) // org:stack:env:tile
	}
	if err != nil || t == nil {
		return nil, echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	s, err := a.stack(c, t.StackID)
	if err != nil {
		return nil, err
	}
	if write {
		if err := a.requireOrgWrite(ctx, c, s.OrgID); err != nil {
			return nil, err
		}
	}
	return t, nil
}

// gate is point 18's one authorization path for the API: resolve who is
// asking, resolve which org the route is about, ask AccessService whether the
// verb is allowed there. The level comes from service.verbLevels, shared with
// the panel, which is what stops the same operation having two levels.
//
// Refusals keep the split AccessService already makes: not-a-member is 404
// on both surfaces, because a 403 there confirms the id to someone who
// should not have it, while in-the-org-but-not-at-this-level is 403 here and
// a 404 in the panel, which hides the resource entirely. That is the
// behaviour the gate helpers had; the mapping is stackrmw.HTTP's.
func (a *API) gate(v service.Verb, k service.Kind, param string, h echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		ctx := c.Request().Context()
		p, err := a.principal(ctx, c)
		if err != nil {
			return err
		}
		// KindDeferred: the org arrives in the body, or is what the request
		// asks to resolve, so there is nothing to check before the handler.
		if k == service.KindDeferred {
			return h(c)
		}
		var orgID string
		if k != service.KindNone {
			orgID, err = a.access.TenancyOf(ctx, k, c.Param(param))
			switch {
			case errors.Is(err, service.ErrServerOwned):
				// The panel's own backup: a real row owned by the
				// installation, not by an org. Admin-only, and nothing for
				// the verb to be checked against.
				if !p.Admin {
					return echo.NewHTTPError(http.StatusNotFound, "not found")
				}
				return h(c)
			case err != nil:
				return err
			case orgID == "":
				return echo.NewHTTPError(http.StatusNotFound, "not found")
			}
			// An org whose wizard is unfinished takes no writes. The gate
			// helpers checked this on every org- and stack-addressed route
			// (orgReady in requireStackAccess); it is not a level, so it does
			// not live in the verb table.
			o, err := a.orgs.Get(ctx, orgID)
			if err != nil {
				return stackrmw.HTTP(err)
			}
			if err := orgReady(o); err != nil {
				return err
			}
		}
		if err := a.keyOrgAllows(c, orgID); err != nil {
			return err
		}
		if err := a.access.Require(p, v, orgID); err != nil {
			return stackrmw.HTTP(err)
		}
		return h(c)
	}
}

// Loaders. Point 18 moved authorization to the route, so a handler behind a
// mutating route needs the row and nothing else — these are requireTile and
// friends with the role check taken out, not new lookups.
//
// The GET handlers still call the require* forms: 192 read routes carry no
// verb yet, so for them the body check is still the only one. That split is
// deliberate and is why both shapes exist (see 06-points-18-20.md).

func (a *API) tile(c echo.Context, ref string) (*repo.Tile, error) {
	t, err := a.access.ResolveTile(c.Request().Context(), ref)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	return t, nil
}

func (a *API) stack(c echo.Context, ref string) (*repo.Stack, error) {
	s, err := a.access.ResolveStack(c.Request().Context(), ref)
	if err != nil {
		return nil, err
	}
	if s == nil {
		return nil, echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	return s, nil
}

func (a *API) env(c echo.Context, ref string) (*repo.Environment, error) {
	e, err := a.access.ResolveEnv(c.Request().Context(), ref)
	if err != nil {
		return nil, err
	}
	if e == nil {
		return nil, echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	return e, nil
}

func (a *API) org(c echo.Context, ref string) (*repo.Org, error) {
	o, err := a.access.ResolveOrg(c.Request().Context(), ref)
	if err != nil {
		return nil, err
	}
	if o == nil {
		return nil, echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	return o, nil
}

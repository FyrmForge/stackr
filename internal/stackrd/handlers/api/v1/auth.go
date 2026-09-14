package v1

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"

	"github.com/labstack/echo/v4"

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
)

// HashKey returns the stored hash for a raw API token.
func HashKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
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
		user, err := a.store.GetUserByID(ctx, k.UserID)
		if err != nil || user == nil {
			return echo.NewHTTPError(http.StatusUnauthorized, "api key user no longer exists")
		}
		c.Set(ctxKey, k)
		c.Set(ctxUser, user)
		if user.Role != "admin" { // admins see all orgs → leave orgIDs nil
			orgs, err := a.store.ListOrgsForUser(ctx, user.ID)
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
	orgs, err := a.store.ListOrgs(c.Request().Context())
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
	s, err := a.store.GetStack(ctx, stackID)
	if err != nil || s == nil {
		s, err = a.stackByPath(ctx, stackID) // org:stack, see slugpath.go
	}
	if err != nil || s == nil {
		return nil, echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	if !a.orgMember(c, s.OrgID) {
		return nil, echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	o, err := a.store.GetOrg(ctx, s.OrgID)
	if err != nil {
		return nil, err
	}
	if err := orgReady(o); err != nil {
		return nil, err
	}
	return s, nil
}

// requireOrgWrite verifies the key's user has content-write (owner/member) in
// the org, the live role check that bounds write scopes.
func (a *API) requireOrgWrite(ctx context.Context, c echo.Context, orgID string) error {
	if a.isAdmin(c) {
		return nil
	}
	m, err := a.store.GetOrgMember(ctx, orgID, a.user(c).ID)
	if err != nil || m == nil || (m.Role != "owner" && m.Role != "member") {
		return echo.NewHTTPError(http.StatusForbidden, "no write access to this organization")
	}
	return nil
}

// requireTile loads a tile and checks org access; write=true also checks the
// user's live write role in the tile's org.
func (a *API) requireTile(c echo.Context, tileID string, write bool) (*repo.Tile, error) {
	ctx := c.Request().Context()
	t, err := a.store.GetTile(ctx, tileID)
	if err != nil || t == nil {
		t, err = a.tileByPath(ctx, tileID) // org:stack:env:tile
	}
	if err != nil || t == nil {
		return nil, echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	s, err := a.requireStackAccess(c, t.StackID)
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

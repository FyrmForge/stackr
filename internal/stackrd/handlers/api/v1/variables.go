package v1

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/config/secrets"
	"github.com/FyrmForge/stackr/internal/stackrd/config/varref"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/deploy"
	"github.com/FyrmForge/stackr/internal/stackrd/store/audit"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Variables are structured rows, not a KEY=VALUE blob: a value carries whether
// it is secret, and secret values only leave the server for a key holding
// secrets:read. Values may be ${{ ... }} references, stored as written and
// resolved at deploy time, never at write time.

const maskedValue = "•••"

// canReadSecrets reports whether this key may see secret values.
func (a *API) canReadSecrets(c echo.Context) bool {
	k, _ := c.Get(ctxKey).(*repo.APIKey)
	return k != nil && k.HasScope(ScopeSecretsRead)
}

// auditActor names the API key for the audit trail.
func auditActor(c echo.Context) string {
	if k, _ := c.Get(ctxKey).(*repo.APIKey); k != nil {
		return "api:" + k.Name
	}
	return "api:?"
}

// auditSecretReads logs each secret whose plaintext a secrets:read response
// carries. Callers only invoke it when the values go out unmasked.
func auditSecretReads(c echo.Context, s repo.Store, vars []repo.Variable, ownerKind, ownerID string) {
	ctx := c.Request().Context()
	for _, v := range vars {
		if v.Secret {
			audit.Record(ctx, s, auditActor(c), audit.Read, ownerKind, ownerID, v.Name)
		}
	}
}

// toVarEntries renders stored rows for a response, masking secret values the
// caller may not read. Masking rather than omitting: the name is not the
// secret, and a caller needs to know the variable exists.
func (a *API) toVarEntries(vars []repo.Variable, reveal bool) []varEntry {
	out := make([]varEntry, 0, len(vars))
	for _, v := range vars {
		e := varEntry{Name: v.Name, Value: v.Value, Secret: v.Secret}
		if v.Secret && !reveal {
			e.Value = maskedValue
		}
		out = append(out, e)
	}
	return out
}

// --- app (tile) variables ---

func (a *API) getVars(c echo.Context) error {
	t, err := a.requireTile(c, c.Param("id"), false)
	if err != nil {
		return err
	}
	vars, err := a.store.ListVariables(c.Request().Context(), repo.OwnerTile, t.ID)
	if err != nil {
		return err
	}
	reveal := a.canReadSecrets(c)
	if reveal {
		auditSecretReads(c, a.store, vars, repo.OwnerTile, t.ID)
	}
	return c.JSON(http.StatusOK, varsOut{Vars: a.toVarEntries(vars, reveal)})
}

// putVars replaces the app's whole variable set.
func (a *API) putVars(c echo.Context) error {
	t, err := a.requireTile(c, c.Param("id"), true)
	if err != nil {
		return err
	}
	// B1: on a config-managed stack the file owns these, a write here would be
	// reverted by the next plan, which is the silent-drift bug.
	if err := a.rejectManaged(c.Request().Context(), t.StackID); err != nil {
		return err
	}
	var in varsIn
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad body")
	}
	if err := a.writeVars(c, repo.OwnerTile, t.ID, in.Vars, t); err != nil {
		return err
	}
	vars, err := a.store.ListVariables(c.Request().Context(), repo.OwnerTile, t.ID)
	if err != nil {
		return err
	}
	reveal := a.canReadSecrets(c)
	if reveal {
		auditSecretReads(c, a.store, vars, repo.OwnerTile, t.ID)
	}
	return c.JSON(http.StatusOK, varsOut{Vars: a.toVarEntries(vars, reveal)}) // deploy to apply
}

// writeVars replaces an owner's variable set. tile is non-nil for tile owners:
// its env blob is kept in step so the console and config plan still see the
// non-secret variables. Secrets stay out of the blob, it is rendered in plain
// UI surfaces and diffed into config plans.
func (a *API) writeVars(c echo.Context, ownerKind, ownerID string, entries []varEntry, tile *repo.Tile) error {
	ctx := c.Request().Context()
	seen := map[string]bool{}
	for _, e := range entries {
		if e.Name == "" {
			return echo.NewHTTPError(http.StatusBadRequest, "variable name required")
		}
		if e.Value == maskedValue {
			return echo.NewHTTPError(http.StatusBadRequest,
				"variable "+e.Name+" was sent back masked; read it with secrets:read or omit it to keep the stored value")
		}
		seen[e.Name] = true
	}
	if tile != nil {
		var lines []string
		for _, e := range entries {
			if !e.Secret {
				lines = append(lines, e.Name+"="+e.Value)
			}
		}
		sort.Strings(lines)
		tile.Env = strings.Join(lines, "\n")
		if err := a.store.UpdateTile(ctx, tile); err != nil {
			return err
		}
	}
	// Existing values, so a generate request can leave a live secret alone.
	existing := map[string]bool{}
	if cur, err := a.store.ListVariables(ctx, ownerKind, ownerID); err == nil {
		for _, v := range cur {
			existing[v.Name] = v.Value != ""
		}
	}
	now := time.Now().UTC()
	for _, e := range entries {
		value := e.Value
		if e.Generate {
			if existing[e.Name] {
				// Generating over a live secret would break everything reading
				// it, and the caller asked for a value, not for a rotation.
				continue
			}
			length := e.Length
			if length <= 0 {
				length = 32
			}
			// The same generator the config file's `default: generated` uses,
			// so a secret minted here and one minted by an apply are the same
			// kind of thing.
			value = secrets.Generate(length, true, false)
		}
		if err := a.store.UpsertVariable(ctx, &repo.Variable{OwnerKind: ownerKind, OwnerID: ownerID,
			Name: e.Name, Value: value, Secret: e.Secret, CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		audit.Record(ctx, a.store, auditActor(c), audit.Set, ownerKind, ownerID, e.Name)
		// Same release the web forms do: a tile parked on this name has its
		// reason gone, and would otherwise read "Waiting for X" forever.
		switch ownerKind {
		case repo.OwnerEnv:
			deploy.ClearWaitingEnv(ctx, a.store, ownerID, e.Name)
		case repo.OwnerStack:
			deploy.ClearWaiting(ctx, a.store, e.Name, ownerID)
		case repo.OwnerOrg:
			deploy.ClearWaitingOrg(ctx, a.store, ownerID, e.Name)
		}
	}
	cur, err := a.store.ListVariables(ctx, ownerKind, ownerID)
	if err != nil {
		return err
	}
	for _, v := range cur {
		if seen[v.Name] {
			continue
		}
		if err := a.store.DeleteVariable(ctx, ownerKind, ownerID, v.Name); err != nil {
			return err
		}
		audit.Record(ctx, a.store, auditActor(c), audit.Delete, ownerKind, ownerID, v.Name)
	}
	return nil
}

// resolvedVars returns the environment a deploy would produce. Scoped mode
// unless the key holds secrets:read, and Scoped fails on a secret rather than
// omitting it, so a caller can never mistake a partial environment for a
// complete one.
func (a *API) resolvedVars(c echo.Context) error {
	t, err := a.requireTile(c, c.Param("id"), false)
	if err != nil {
		return err
	}
	mode := varref.Scoped
	if a.canReadSecrets(c) {
		mode = varref.System
		// The resolved environment folds in secrets from every scope; "*" marks
		// a whole-environment read rather than naming each one.
		audit.Record(c.Request().Context(), a.store, auditActor(c), audit.Read, repo.OwnerTile, t.ID, "*")
	}
	res, err := varref.New(a.store).Resolve(c.Request().Context(), t.ID, mode)
	if err != nil {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, err.Error())
	}
	out := make([]varEntry, 0, len(res.Vars))
	for name, val := range res.Vars {
		out = append(out, varEntry{Name: name, Value: val})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return c.JSON(http.StatusOK, varsOut{Vars: out})
}

// --- stack and org variables ---

func (a *API) getStackVars(c echo.Context) error {
	s, err := a.requireStackAccess(c, c.Param("id"))
	if err != nil {
		return err
	}
	vars, err := a.store.ListVariables(c.Request().Context(), repo.OwnerStack, s.ID)
	if err != nil {
		return err
	}
	reveal := a.canReadSecrets(c)
	if reveal {
		auditSecretReads(c, a.store, vars, repo.OwnerStack, s.ID)
	}
	return c.JSON(http.StatusOK, varsOut{Vars: a.toVarEntries(vars, reveal)})
}

func (a *API) putStackVars(c echo.Context) error {
	s, err := a.requireStackAccess(c, c.Param("id"))
	if err != nil {
		return err
	}
	if err := a.requireOrgWrite(c.Request().Context(), c, s.OrgID); err != nil {
		return err
	}
	var in varsIn
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad body")
	}
	// Stack/org values are server-managed by design, they are outside the repo
	// config, so a config-managed stack doesn't lock them.
	if err := a.writeVars(c, repo.OwnerStack, s.ID, in.Vars, nil); err != nil {
		return err
	}
	return a.getStackVars(c)
}

// requireEnv resolves an environment through its stack's tenancy check (404,
// not 403, so ids don't leak across tenants). The stack comes back too, it
// carries the org id a write check needs.
func (a *API) requireEnv(c echo.Context, envID string) (*repo.Environment, *repo.Stack, error) {
	env, err := a.store.GetEnvironment(c.Request().Context(), envID)
	if err != nil || env == nil {
		return nil, nil, echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	s, err := a.requireStackAccess(c, env.StackID)
	if err != nil {
		return nil, nil, err
	}
	return env, s, nil
}

func (a *API) getEnvVars(c echo.Context) error {
	env, _, err := a.requireEnv(c, c.Param("id"))
	if err != nil {
		return err
	}
	vars, err := a.store.ListVariables(c.Request().Context(), repo.OwnerEnv, env.ID)
	if err != nil {
		return err
	}
	reveal := a.canReadSecrets(c)
	if reveal {
		auditSecretReads(c, a.store, vars, repo.OwnerEnv, env.ID)
	}
	return c.JSON(http.StatusOK, varsOut{Vars: a.toVarEntries(vars, reveal)})
}

func (a *API) putEnvVars(c echo.Context) error {
	env, s, err := a.requireEnv(c, c.Param("id"))
	if err != nil {
		return err
	}
	if err := a.requireOrgWrite(c.Request().Context(), c, s.OrgID); err != nil {
		return err
	}
	var in varsIn
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad body")
	}
	// Env values are the per-environment side of stack-declared secrets, like
	// stack/org values they live outside the repo config, so a config-managed
	// stack doesn't lock them.
	if err := a.writeVars(c, repo.OwnerEnv, env.ID, in.Vars, nil); err != nil {
		return err
	}
	return a.getEnvVars(c)
}

// requireOrg resolves an org the key may act in (404, not 403, so ids don't
// leak across tenants).
func (a *API) requireOrg(c echo.Context, orgID string) (*repo.Org, error) {
	ctx := c.Request().Context()
	o, err := a.store.GetOrg(ctx, orgID)
	if err != nil || o == nil {
		o, err = a.orgByPath(ctx, orgID) // a bare org slug, see slugpath.go
	}
	if err != nil || o == nil || !a.orgMember(c, o.ID) {
		return nil, echo.NewHTTPError(http.StatusNotFound, "not found")
	}
	if err := orgReady(o); err != nil {
		return nil, err
	}
	return o, nil
}

func (a *API) getOrgVars(c echo.Context) error {
	o, err := a.requireOrg(c, c.Param("id"))
	if err != nil {
		return err
	}
	vars, err := a.store.ListVariables(c.Request().Context(), repo.OwnerOrg, o.ID)
	if err != nil {
		return err
	}
	reveal := a.canReadSecrets(c)
	if reveal {
		auditSecretReads(c, a.store, vars, repo.OwnerOrg, o.ID)
	}
	return c.JSON(http.StatusOK, varsOut{Vars: a.toVarEntries(vars, reveal)})
}

func (a *API) putOrgVars(c echo.Context) error {
	o, err := a.requireOrg(c, c.Param("id"))
	if err != nil {
		return err
	}
	if err := a.requireOrgWrite(c.Request().Context(), c, o.ID); err != nil {
		return err
	}
	var in varsIn
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad body")
	}
	if err := a.writeVars(c, repo.OwnerOrg, o.ID, in.Vars, nil); err != nil {
		return err
	}
	return a.getOrgVars(c)
}

// --- reference catalogue ---

// referenceCatalogue lists what this app may reference and under which names.
// Metadata only: it never carries values, secret or otherwise, so it is safe
// for autocomplete without secrets:read.
func (a *API) referenceCatalogue(c echo.Context) error {
	t, err := a.requireTile(c, c.Param("id"), false)
	if err != nil {
		return err
	}
	sources, err := varref.Catalogue(c.Request().Context(), a.store, t.ID)
	if err != nil {
		return err
	}
	out := make([]refSourceOut, 0, len(sources))
	for _, s := range sources {
		outs := make([]refOutputOut, 0, len(s.Outputs))
		for _, o := range s.Outputs {
			outs = append(outs, refOutputOut{Name: o.Name, Kind: o.Kind, Secret: o.Secret})
		}
		out = append(out, refSourceOut{Scope: s.Scope, Slug: s.Slug, Kind: s.Kind, Outputs: outs})
	}
	return c.JSON(http.StatusOK, out)
}

// --- managed resources ---

// listAppResources reports the resources this app is bound to and the names
// they publish, identity and output names, never values.
func (a *API) listAppResources(c echo.Context) error {
	t, err := a.requireTile(c, c.Param("id"), false)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	binds, err := a.store.BindingsForConsumer(ctx, t.ID)
	if err != nil {
		return err
	}
	out := []resourceOut{}
	for _, b := range binds {
		res, err := a.store.GetResource(ctx, b.ResourceID)
		if err != nil || res == nil {
			continue
		}
		outs, err := a.store.ListOutputs(ctx, res.ID)
		if err != nil {
			return err
		}
		names := make([]string, 0, len(outs))
		for _, o := range outs {
			names = append(names, o.Name)
		}
		out = append(out, resourceOut{ID: res.ID, Slug: res.Slug, Name: res.Name,
			Kind: res.Kind, Status: res.Status, Outputs: names})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return c.JSON(http.StatusOK, out)
}

package v1

import (
	"net/http"
	"sort"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/config/varref"
	stackrmw "github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
	"github.com/FyrmForge/stackr/internal/stackrd/service"
	"github.com/FyrmForge/stackr/internal/stackrd/store/audit"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Variables are structured rows, not a KEY=VALUE blob: a value carries whether
// it is secret, and secret values only leave the server for a key holding
// secrets:read. Values may be ${{ ... }} references, stored as written and
// resolved at deploy time, never at write time.

// maskedValue is the service's, so the mask a listing renders and the mask a
// write refuses are the same string.
const maskedValue = service.Masked

// canReadSecrets reports whether this caller may see secret VALUES, as opposed
// to the names and the fact that a value is secret.
//
// Two conditions, not one. The scope is what the key was granted; the level is
// what its user has in the org right now. secrets:read is a write-grade scope,
// so it can only be minted by somebody with content-write — but a key outlives
// the role that minted it, and the route itself is read-level (a viewer may
// list variables). Without the live check, a member who minted a key and was
// then demoted to viewer kept reading every secret in the org, which is the
// exact shape point 7 found on the deploy routes.
//
// It matches the panel, where the reveal and the Copy endpoint both ask for
// write in the resource's own org. Decided when the reads were gated; see
// docs/plans/service-extraction/06-points-18-20.md.
func (a *API) canReadSecrets(c echo.Context, k service.Kind, ref string) bool {
	key, _ := c.Get(ctxKey).(*repo.APIKey)
	if key != nil && !key.HasScope(ScopeSecretsRead) {
		return false
	}
	orgID, err := a.access.TenancyOf(c.Request().Context(), k, ref)
	if err != nil || orgID == "" {
		return false
	}
	return a.requireVerb(c.Request().Context(), c, service.VerbVariableWrite, orgID) == nil
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
	t, err := a.tile(c, c.Param("id"))
	if err != nil {
		return err
	}
	vars, err := a.vars.List(c.Request().Context(), service.TileVars(t.ID))
	if err != nil {
		return err
	}
	reveal := a.canReadSecrets(c, service.KindTile, t.ID)
	if reveal {
		auditSecretReads(c, a.store, vars, repo.OwnerTile, t.ID)
	}
	return c.JSON(http.StatusOK, varsOut{Vars: a.toVarEntries(vars, reveal)})
}

// putVars replaces the app's whole variable set.
func (a *API) putVars(c echo.Context) error {
	t, err := a.tile(c, c.Param("id"))
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
		return stackrmw.HTTP(err)
	}
	vars, err := a.vars.List(c.Request().Context(), service.TileVars(t.ID))
	if err != nil {
		return err
	}
	reveal := a.canReadSecrets(c, service.KindTile, t.ID)
	if reveal {
		auditSecretReads(c, a.store, vars, repo.OwnerTile, t.ID)
	}
	return c.JSON(http.StatusOK, varsOut{Vars: a.toVarEntries(vars, reveal)}) // deploy to apply
}

// writeVars replaces an owner's variable set through the service, so an API
// write earns the same side effects a panel write does: the audit row, the
// release of any deploy parked on the name, a replan when the stack is
// config-managed, and a redeploy of a running consumer.
//
// None of those happened here before. `stackr vars set` on a running tile
// wrote a row and changed nothing a user could see.
func (a *API) writeVars(c echo.Context, ownerKind, ownerID string, entries []varEntry, tile *repo.Tile) error {
	writes := make([]service.VarWrite, 0, len(entries))
	for _, e := range entries {
		writes = append(writes, service.VarWrite{Name: e.Name, Value: e.Value,
			Secret: e.Secret, Generate: e.Generate, Length: e.Length})
	}
	return a.vars.Replace(c.Request().Context(),
		service.VarOwner{Kind: ownerKind, ID: ownerID}, writes, a.actor(c))
}

// resolvedVars returns the environment a deploy would produce. Scoped mode
// unless the key holds secrets:read, and Scoped fails on a secret rather than
// omitting it, so a caller can never mistake a partial environment for a
// complete one.
func (a *API) resolvedVars(c echo.Context) error {
	t, err := a.tile(c, c.Param("id"))
	if err != nil {
		return err
	}
	mode := varref.Scoped
	if a.canReadSecrets(c, service.KindTile, t.ID) {
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
	s, err := a.stack(c, c.Param("id"))
	if err != nil {
		return err
	}
	vars, err := a.vars.List(c.Request().Context(), service.StackVars(s.ID))
	if err != nil {
		return err
	}
	reveal := a.canReadSecrets(c, service.KindStack, s.ID)
	if reveal {
		auditSecretReads(c, a.store, vars, repo.OwnerStack, s.ID)
	}
	return c.JSON(http.StatusOK, varsOut{Vars: a.toVarEntries(vars, reveal)})
}

func (a *API) putStackVars(c echo.Context) error {
	s, err := a.stack(c, c.Param("id"))
	if err != nil {
		return err
	}
	var in varsIn
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad body")
	}
	// Stack/org values are server-managed by design, they are outside the repo
	// config, so a config-managed stack doesn't lock them.
	if err := a.writeVars(c, repo.OwnerStack, s.ID, in.Vars, nil); err != nil {
		return stackrmw.HTTP(err)
	}
	a.stackChanged(s.ID)
	return a.getStackVars(c)
}

// requireEnv resolves an environment through its stack's tenancy check (404,
// not 403, so ids don't leak across tenants). The stack comes back too, it
// carries the org id a write check needs.
func (a *API) requireEnv(c echo.Context, envID string) (*repo.Environment, *repo.Stack, error) {
	env, err := a.envs.Get(c.Request().Context(), envID)
	if err != nil {
		return nil, nil, stackrmw.HTTP(err)
	}
	s, err := a.stack(c, env.StackID)
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
	vars, err := a.vars.List(c.Request().Context(), service.EnvVars(env.ID))
	if err != nil {
		return err
	}
	reveal := a.canReadSecrets(c, service.KindEnv, env.ID)
	if reveal {
		auditSecretReads(c, a.store, vars, repo.OwnerEnv, env.ID)
	}
	return c.JSON(http.StatusOK, varsOut{Vars: a.toVarEntries(vars, reveal)})
}

func (a *API) putEnvVars(c echo.Context) error {
	env, _, err := a.requireEnv(c, c.Param("id"))
	if err != nil {
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
		return stackrmw.HTTP(err)
	}
	a.stackChanged(env.StackID)
	return a.getEnvVars(c)
}

// requireOrg resolves an org the key may act in (404, not 403, so ids don't
// leak across tenants).
func (a *API) requireOrg(c echo.Context, orgID string) (*repo.Org, error) {
	ctx := c.Request().Context()
	o, err := a.orgs.Get(ctx, orgID)
	if err != nil {
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
	o, err := a.org(c, c.Param("id"))
	if err != nil {
		return err
	}
	vars, err := a.vars.List(c.Request().Context(), service.OrgVars(o.ID))
	if err != nil {
		return err
	}
	reveal := a.canReadSecrets(c, service.KindOrg, o.ID)
	if reveal {
		auditSecretReads(c, a.store, vars, repo.OwnerOrg, o.ID)
	}
	return c.JSON(http.StatusOK, varsOut{Vars: a.toVarEntries(vars, reveal)})
}

func (a *API) putOrgVars(c echo.Context) error {
	o, err := a.org(c, c.Param("id"))
	if err != nil {
		return err
	}
	var in varsIn
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad body")
	}
	if err := a.writeVars(c, repo.OwnerOrg, o.ID, in.Vars, nil); err != nil {
		return stackrmw.HTTP(err)
	}
	return a.getOrgVars(c)
}

// --- reference catalogue ---

// referenceCatalogue lists what this app may reference and under which names.
// Metadata only: it never carries values, secret or otherwise, so it is safe
// for autocomplete without secrets:read.
func (a *API) referenceCatalogue(c echo.Context) error {
	t, err := a.tile(c, c.Param("id"))
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
	t, err := a.tile(c, c.Param("id"))
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	binds, err := a.instances.Bindings(ctx, t.ID)
	if err != nil {
		return err
	}
	out := []resourceOut{}
	for _, b := range binds {
		res, err := a.instances.Resource(ctx, b.ResourceID)
		if err != nil {
			continue
		}
		outs, err := a.instances.Outputs(ctx, res.ID)
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

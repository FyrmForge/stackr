package v1

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/config/envutil"
	"github.com/FyrmForge/stackr/internal/stackrd/service"
	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// stackOrg returns a stack's org id ("" when the stack is gone), cache-free.
func (a *API) stackOrg(c echo.Context, stackID string) string {
	s, err := a.stacks.Get(c.Request().Context(), stackID)
	if err != nil {
		return ""
	}
	return s.OrgID
}

// orgForCreate resolves the org a new stack lands in: an explicit org_id
// (must be writable), else the caller's single writable org.
func (a *API) orgForCreate(c echo.Context, explicit string) (string, error) {
	ctx := c.Request().Context()
	cand := map[string]bool{}
	if a.isAdmin(c) {
		orgs, _ := a.orgs.ListAll(ctx)
		for _, o := range orgs {
			cand[o.ID] = true
		}
	} else {
		for id := range a.userOrgIDs(c) {
			if role, err := a.members.RoleOf(ctx, id, a.user(c).ID); err == nil && (role == "owner" || role == "member") {
				cand[id] = true
			}
		}
	}
	// A bound key may only create in the org it was minted for. This path
	// picks an org by role rather than resolving one from the request, so it
	// is the one write that never reaches requireVerb — see keyOrgAllows.
	for id := range cand {
		if a.keyOrgAllows(c, id) != nil {
			delete(cand, id)
		}
	}
	// A create names its org instead of inheriting one from a resource, so it
	// is the one path that misses requireStackAccess and requireOrg. Without
	// this the CLI could still build inside an org whose wizard is open.
	//
	// Dropped from the candidates rather than rejected after the pick: a user
	// with one working org and one half-set-up one would otherwise be told to
	// name an org_id because "you belong to multiple organizations", when only
	// one of them can take a stack at all. An org named explicitly still gets
	// the 409 that says why.
	pending := map[string]*repo.Org{}
	for id := range cand {
		o, err := a.orgs.Get(ctx, id)
		if errors.Is(err, svcerr.ErrNotFound) {
			delete(cand, id) // vanished under us: not a candidate, not a 409
			continue
		}
		if err != nil {
			return "", err
		}
		if o.SetupDoneAt == nil {
			pending[id] = o
			delete(cand, id)
		}
	}
	if explicit != "" {
		if o, ok := pending[explicit]; ok {
			return "", orgReady(o)
		}
		if !cand[explicit] {
			return "", echo.NewHTTPError(http.StatusForbidden, "no write access to that organization")
		}
		return explicit, nil
	}
	if len(cand) == 1 {
		for id := range cand {
			return id, nil
		}
	}
	if len(cand) == 0 {
		// The whole point of the 409 is the brand-new user whose only org is
		// still in the wizard. Dropping unfinished orgs from cand hid exactly
		// that case behind "no write access", which is not true, they own it.
		for _, o := range pending {
			return "", orgReady(o)
		}
		return "", echo.NewHTTPError(http.StatusForbidden, "no organization with write access")
	}
	return "", echo.NewHTTPError(http.StatusBadRequest, "org_id required (you belong to multiple organizations)")
}

// resolveEnv finds an env by slug within a stack (default: the first env).
func (a *API) resolveEnv(ctx context.Context, stackID, slug string) (*repo.Environment, error) {
	envs, err := a.envs.ListForStack(ctx, stackID)
	if err != nil || len(envs) == 0 {
		return nil, echo.NewHTTPError(http.StatusBadRequest, "stack has no environments")
	}
	if slug == "" {
		return &envs[0], nil
	}
	for i := range envs {
		if envs[i].Slug == slug {
			return &envs[i], nil
		}
	}
	return nil, echo.NewHTTPError(http.StatusNotFound, "environment not found")
}

// checkConnector validates a connector id against the stack's org. A connector
// carries git credentials, so accepting one from another tenant would let a
// caller borrow its access; the web form silently drops an invalid id, but an
// API caller asked for something specific and should be told it was refused
// rather than get a tile that quietly cannot clone.
func (a *API) checkConnector(ctx context.Context, s *repo.Stack, id string) error {
	if id == "" {
		return nil
	}
	cn, err := a.connectors.Get(ctx, id)
	if err != nil && !errors.Is(err, svcerr.ErrNotFound) {
		return err
	}
	if cn == nil || cn.OrgID != s.OrgID {
		return echo.NewHTTPError(http.StatusBadRequest, "connector not found in this organization")
	}
	return nil
}

// teardownTile stops a tile's containers, removes its proxy route, orphans
// the shared-db provisions it consumed, drops the row and re-registers the
// two schedule tables its rows cascaded out of.
//
// The last two halves are new here: this used to be three lines that skipped
// both, so a tile deleted over the API left provision rows pointing at an id
// that no longer resolved and left its cron_jobs and backups entries ticking.
// It stays as a helper because the volume and managed-database delete routes
// call it too, and their gates are points 9 and 5.
func (a *API) teardownTile(ctx context.Context, t *repo.Tile) error {
	return a.tiles.TearDown(ctx, t)
}

// actor names the API key that made the call. There is no user row behind an
// API key, so the id is empty and the key's name is what signs a staged
// change or a run.
func (a *API) actor(c echo.Context) service.Actor {
	act := service.Actor{Via: "api"}
	if k, _ := c.Get(ctxKey).(*repo.APIKey); k != nil {
		act.Name = k.Name
	}
	return act
}

func envToMap(raw string) map[string]string {
	m := map[string]string{}
	for _, v := range envutil.Parse(raw) {
		m[v.Key] = v.Value
	}
	return m
}

func mapToEnv(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(m[k])
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

// stackChanged nudges the open canvases after something in a stack changed
// that is not a tile's runtime state: variables, a slice's visibility, a new
// managed database. Same omission as statusChanged — the panel nudged, the
// API did not.
func (a *API) stackChanged(stackID string) {
	if a.notify == nil || stackID == "" {
		return
	}
	a.notify.Project(stackID)
}

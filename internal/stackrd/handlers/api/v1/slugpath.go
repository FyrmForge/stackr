package v1

// Slug paths. Every {id} in the API also accepts the object's slug path:
// `acme`, `acme:shop`, `acme:shop:prod`, `acme:shop:prod:api`. That is what
// lets a CI script address a stack by the name a human wrote in the config file
// instead of carrying a uuid around.
//
// The separator is a colon, not a slash: a {id} is one URL path segment, and a
// slash inside it would be routed as another segment before any handler saw it.
// Colon is also what the infra paths the CLI already speaks use
// (infra/managedtiles/resolve.go). A slash is accepted too, for the places a
// path arrives in a body or a flag rather than a URL.
//
// A bare stack, env or tile slug is not accepted. Slugs repeat across orgs, so
// a one-segment reference below org level would have to guess a tenant, and
// guessing is how one org's script quietly edits another's stack.
//
// Resolution lives inside the four access gates (requireOrg,
// requireStackAccess, requireEnvAccess, requireTile), so every handler gets it
// without a second addressing path to keep in sync. An id is still an id: the
// lookup by id runs first, and a path is tried only when that misses. The
// authorization checks are unchanged and run on whatever the path resolved to,
// so a path into another tenant 404s exactly like its id would.

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Path depths, one per addressable kind.
const (
	refOrg   = 1
	refStack = 2
	refEnv   = 3
	refTile  = 4
)

// pathParts splits a slug path and reports whether it has exactly the depth the
// caller wants. Anything else is not a path for that kind.
func pathParts(ref string, want int) ([]string, bool) {
	parts := strings.FieldsFunc(ref, func(r rune) bool { return r == ':' || r == '/' })
	if len(parts) != want {
		return nil, false
	}
	for _, p := range parts {
		if p == "" {
			return nil, false
		}
	}
	return parts, true
}

// orgByPath resolves a bare org slug.
func (a *API) orgByPath(ctx context.Context, ref string) (*repo.Org, error) {
	parts, ok := pathParts(ref, refOrg)
	if !ok {
		return nil, nil
	}
	org, err := a.orgs.BySlug(ctx, parts[0])
	if errors.Is(err, svcerr.ErrNotFound) {
		return nil, nil // a path miss is the caller's to interpret
	}
	return org, err
}

// stackByPath resolves org/stack.
func (a *API) stackByPath(ctx context.Context, ref string) (*repo.Stack, error) {
	parts, ok := pathParts(ref, refStack)
	if !ok {
		return nil, nil
	}
	org, err := a.orgByPath(ctx, parts[0])
	if err != nil || org == nil {
		return nil, err
	}
	st, err := a.stacks.BySlug(ctx, org.ID, parts[1])
	if errors.Is(err, svcerr.ErrNotFound) {
		return nil, nil // see envByPath: a path miss is the caller's to interpret
	}
	return st, err
}

// envByPath resolves org/stack/env.
func (a *API) envByPath(ctx context.Context, ref string) (*repo.Environment, error) {
	parts, ok := pathParts(ref, refEnv)
	if !ok {
		return nil, nil
	}
	st, err := a.stackByPath(ctx, parts[0]+":"+parts[1])
	if err != nil || st == nil {
		return nil, err
	}
	env, err := a.envs.BySlug(ctx, st.ID, parts[2])
	if errors.Is(err, svcerr.ErrNotFound) {
		// A path that resolves to nothing is not an error here: every caller
		// of this resolver decides for itself what a miss means.
		return nil, nil
	}
	return env, err
}

// tileByPath resolves org/stack/env/tile.
func (a *API) tileByPath(ctx context.Context, ref string) (*repo.Tile, error) {
	parts, ok := pathParts(ref, refTile)
	if !ok {
		return nil, nil
	}
	env, err := a.envByPath(ctx, strings.Join(parts[:3], ":"))
	if err != nil || env == nil {
		return nil, err
	}
	t, err := a.tiles.BySlug(ctx, env.ID, parts[3])
	if errors.Is(err, svcerr.ErrNotFound) {
		return nil, nil // a path miss is the caller's to interpret
	}
	return t, err
}

// listOrgs is the entry point for slug addressing: without it a caller holding
// only an API key has no way to learn the org slug every other path starts
// with. Admins see every org; everyone else sees their own memberships.
func (a *API) listOrgs(c echo.Context) error {
	ctx := c.Request().Context()
	var orgs []repo.Org
	var err error
	if a.isAdmin(c) {
		orgs, err = a.orgs.ListAll(ctx)
	} else {
		orgs, err = a.orgs.ListForUser(ctx, a.user(c).ID)
	}
	if err != nil {
		return err
	}
	out := make([]orgOut, 0, len(orgs))
	for i := range orgs {
		o := &orgs[i]
		role := "admin" // an admin is not a member row; say so rather than blank
		if r, err := a.members.RoleOf(ctx, o.ID, a.user(c).ID); err == nil && r != "" {
			role = r
		}
		out = append(out, orgOut{ID: o.ID, Slug: o.Slug, Name: o.Name, Role: role})
	}
	return c.JSON(http.StatusOK, out)
}

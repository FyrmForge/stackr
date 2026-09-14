package components

import (
	"context"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/stackrd/envcolor"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/envnet"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// PanelHead is what a drawer's header shows. A struct rather than a parameter
// list because every tile kind fills the same slots and half of them are
// optional, a positional call would be six strings in a row and easy to
// transpose.
type PanelHead struct {
	Title string
	// Subtitle is what the tile is made of: an image, a source, an engine.
	Subtitle string
	// Location is where it lives, "<stack> / <env>". The full page has a
	// breadcrumb for this; the drawer has none, so a shared instance opened
	// from the org canvas would otherwise not say which environment it is in.
	Location string
	// LocationColor is the env's colour (envcolor CSS value) for the dot
	// before Location, "" for none.
	LocationColor string
	// Scope is how far the tile is shared ("org-scoped"). Separate from
	// Location because they answer different questions: an org-scoped
	// instance lives in one environment but serves every stack.
	Scope  string
	Status string
	// InDrawer adds the close button, the full page navigates instead.
	InDrawer bool
}

// CtxTileLocation is where a handler leaves "<stack> / <env>" for the panel
// header to pick up.
//
// A context stash rather than another parameter: every panel path already
// funnels through its handler's load(), and the app panel component takes
// thirteen arguments as it is. A path that skips load() simply renders no
// location, which is a missing line rather than a wrong one.
const CtxTileLocation = "tileLocation"

// CtxTileLocationColor is the env colour beside it, stashed by the same call.
const CtxTileLocationColor = "tileLocationColor"

// TileLocationColor reads the env colour a handler stashed, "" if none.
func TileLocationColor(c echo.Context) string {
	s, _ := c.Get(CtxTileLocationColor).(string)
	return s
}

// TileLocation reads the location a handler stashed, "" if none.
func TileLocation(c echo.Context) string {
	s, _ := c.Get(CtxTileLocation).(string)
	return s
}

// CtxUpperEnv marks a tile living above the default env: the ladder's upper
// rungs get images by Promote on the releases page, so the tile's own Deploy
// button (a build from branch head) is hidden there.
const CtxUpperEnv = "upperEnv"

// UpperEnv reads the flag a handler stashed.
func UpperEnv(c echo.Context) bool {
	b, _ := c.Get(CtxUpperEnv).(bool)
	return b
}

// StashUpperEnv sets the flag for the tile.
func StashUpperEnv(c echo.Context, store repo.Store, t *repo.Tile) {
	c.Set(CtxUpperEnv, envnet.UpperEnv(c.Request().Context(), store, t))
}

// StashTileLocation resolves "<stack> / <env>" for a tile and stashes it.
// Best-effort: a lookup failure leaves the header without the line.
func StashTileLocation(c echo.Context, store repo.Store, t *repo.Tile) {
	sc, err := envnet.Resolve(c.Request().Context(), store, t)
	if err != nil {
		return
	}
	loc := sc.StackSlug + " / " + sc.EnvSlug
	if sc.EnvSlug == repo.HomeSlug {
		loc = sc.StackSlug // shared tile: the stack, no env
	}
	c.Set(CtxTileLocation, loc)
	c.Set(CtxTileLocationColor, EnvColor(c.Request().Context(), store, t.StackID, t.EnvironmentID))
}

// EnvColor resolves one env's colour through the stack's ladder and org.
// Best-effort: "" on any lookup failure.
func EnvColor(ctx context.Context, store repo.Store, stackID, envID string) string {
	st, err := store.GetStack(ctx, stackID)
	if err != nil || st == nil {
		return ""
	}
	envs, err := store.ListEnvironmentsByStack(ctx, stackID)
	if err != nil {
		return ""
	}
	org, _ := store.GetOrg(ctx, st.OrgID)
	return envcolor.Map(envs, org, st.ConfigManaged())[envID].CSS
}

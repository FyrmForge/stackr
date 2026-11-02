// Package scope is the page behind /:org/:stack/:env/:tile: it sends the
// viewer to the env canvas with the tile's drawer open, so crumbs and shared
// links land on something.
package scope

import (
	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
)

type handler struct{}

func NewHandler() *handler { return &handler{} }

// GET /:org/:stack/:env/:tile
func (h *handler) Page(c echo.Context) error {
	s := middleware.ScopeOf(c)
	return respond.Redirect(c, "/"+s.Org.Slug+"/"+s.Stack.Slug+"/"+s.Env.Slug+"?drawer=tile:"+s.Tile.ID)
}

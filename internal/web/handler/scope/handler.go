// Package scope is the placeholder behind /:org/:stack/:env/:tile, so the
// access middleware is mounted on every level and the layout's crumbs have
// a page to follow. ponytail: the org, stack, env and tile pages replace it.
package scope

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/web/render"
)

type handler struct{}

func NewHandler() *handler { return &handler{} }

// GET /:org, /:org/:stack, /:org/:stack/:env, /:org/:stack/:env/:tile
func (h *handler) Page(c echo.Context) error {
	return render.Page(c, http.StatusOK, "Overview", placeholder())
}

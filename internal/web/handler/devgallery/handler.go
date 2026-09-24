// Package devgallery renders every shared component with sample views, so
// a component change is seen in one place. Mounted in dev mode only.
package devgallery

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/web/render"
)

type handler struct{}

func NewHandler() *handler { return &handler{} }

// GET /dev/components
func (h *handler) Page(c echo.Context) error {
	return render.Page(c, http.StatusOK, "Components", gallery())
}

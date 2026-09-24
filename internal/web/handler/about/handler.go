package about

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/web/render"
)

// Handler handles about page requests.
type handler struct{}

// NewHandler creates a new about handler.
func NewHandler() *handler {
	return &handler{}
}

// GET /about
func (h *handler) About(c echo.Context) error {
	return render.Page(c, http.StatusOK, "About", aboutPage())
}

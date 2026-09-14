package about

import (
	"net/http"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"
)

// Handler handles about page requests.
type handler struct{}

// NewHandler creates a new about handler.
func NewHandler() *handler {
	return &handler{}
}

// GET /about
func (h *handler) About(c echo.Context) error {
	return respond.HTML(c, http.StatusOK, aboutPage(c))
}

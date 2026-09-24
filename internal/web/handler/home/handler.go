package home

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/web/render"
)

// Handler handles home page requests.
type handler struct{}

// NewHandler creates a new home handler.
func NewHandler() *handler {
	return &handler{}
}

// GET /
func (h *handler) Index(c echo.Context) error {
	return render.Page(c, http.StatusOK, "Home", homePage())
}

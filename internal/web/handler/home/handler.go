package home

import (
	"net/http"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"
)

// Handler handles home page requests.
type handler struct{}

// NewHandler creates a new home handler.
func NewHandler() *handler {
	return &handler{}
}

// GET /
func (h *handler) Index(c echo.Context) error {
	return respond.HTML(c, http.StatusOK, homePage(c))
}

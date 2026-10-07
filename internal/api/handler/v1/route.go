package v1

import (
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/service"
)

type (
	// RouteIn is a new route: a host that goes to an address outside stackr.
	RouteIn struct {
		Host     string `json:"host"`
		Mode     string `json:"mode"` // passthrough | http | https
		Target   string `json:"target"`
		Insecure bool   `json:"insecure"`
	}
	// RouteOut is a route row.
	RouteOut = service.ExternalRoute
)

// Routes is every external route (admin).
func (h *H) Routes() Endpoint {
	return Get(func(c echo.Context) ([]RouteOut, error) { return list(h.Orch.ExternalRoutes(rc(c))) })
}

func (h *H) CreateRoute() Endpoint {
	return JSON(201, func(c echo.Context, in RouteIn) (RouteOut, error) {
		return h.Orch.CreateExternalRoute(rc(c), service.ExternalRouteSpec(in))
	})
}

func (h *H) DeleteRoute() Endpoint {
	return Done(func(c echo.Context, _ None) error { return h.Orch.DeleteExternalRoute(rc(c), c.Param("route")) })
}

package v1

import (
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/service"
)

func (h *H) GetOrg() Endpoint {
	return Get(func(c echo.Context) (service.Org, error) { return *scope(c).Org, nil })
}

func (h *H) Orgs() Endpoint {
	return Get(func(c echo.Context) ([]service.Org, error) { return list(h.S.Orgs(rc(c), who(c))) })
}

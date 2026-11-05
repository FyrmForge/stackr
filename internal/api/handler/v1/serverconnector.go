package v1

import (
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
)

type ConnectorShareIn struct {
	OrgIDs []string `json:"org_ids"` // the orgs it is shared with by name
	All    bool     `json:"all"`     // every org; wins over OrgIDs
}

func (h *H) ServerConnectors() Endpoint {
	return Get(func(c echo.Context) ([]service.ServerConnector, error) {
		return list(h.Orch.ServerConnectors(rc(c)))
	})
}

func (h *H) UpdateServerConnector() Endpoint {
	return JSON(200, func(c echo.Context, in ConnectorPatch) (service.ServerConnector, error) {
		return h.Orch.RenameServerConnector(rc(c), c.Param("connector"), in.Name)
	})
}

func (h *H) DeleteServerConnector() Endpoint {
	return Done(func(c echo.Context, _ None) error {
		return h.Orch.DeleteServerConnector(rc(c), c.Param("connector"))
	})
}

func (h *H) ShareConnector() Endpoint {
	return JSON(200, func(c echo.Context, in ConnectorShareIn) (service.ServerConnector, error) {
		return h.Orch.ShareConnector(rc(c), middleware.Principal(c), c.Param("connector"), in.OrgIDs, in.All)
	})
}

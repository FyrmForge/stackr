package v1

import (
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
)

// Pins, ingress and exits work the same at every scope, so each handler
// takes the At that names the scope (org, stack, env or tile).

// AtTile names the tile in the path.
func AtTile(c echo.Context) (string, string) { return "tile", tileID(c) }

// Pins, ingress picks and exit lists name placements, never nodes.

// PlacementRef names a placement; "" clears the pick.
type PlacementRef struct {
	Placement string `json:"placement"`
}

// PlacementsIn is an ordered list of placements (or "own"); empty clears the
// list.
type PlacementsIn struct {
	Placements []string `json:"placements"`
}

// Pin answers where new envs of an org or stack go.
func (h *H) Pin(at At) Endpoint {
	return Get(func(c echo.Context) (service.PinOut, error) {
		k, id := at(c)
		return h.Orch.PinOf(rc(c), k, id)
	})
}

// SetPin pins an org or stack to a placement (open to the org); an empty
// placement clears it. A pin that changes where an existing env runs moves it
// (SetEnvPin).
func (h *H) SetPin(at At) Endpoint {
	return JSON(200, func(c echo.Context, in PlacementRef) (service.PinOut, error) {
		k, id := at(c)
		return h.Orch.PinNode(rc(c), middleware.Principal(c), k, id, in.Placement)
	})
}

// Ingress answers the placement public traffic of a scope enters through.
func (h *H) Ingress(at At) Endpoint {
	return Get(func(c echo.Context) (service.IngressOut, error) {
		k, id := at(c)
		return h.Orch.IngressGet(rc(c), k, id)
	})
}

// SetIngress picks the ingress placement of a scope; empty clears it.
func (h *H) SetIngress(at At) Endpoint {
	return JSON(200, func(c echo.Context, in PlacementRef) (service.IngressOut, error) {
		k, id := at(c)
		return h.Orch.IngressSet(rc(c), middleware.Principal(c), k, id, in.Placement)
	})
}

// Exits answers the exit list in force for a scope.
func (h *H) Exits(at At) Endpoint {
	return Get(func(c echo.Context) (service.ExitOut, error) {
		k, id := at(c)
		return h.Orch.ExitGet(rc(c), k, id)
	})
}

// SetExits replaces the scope's exit list; an empty list goes back to the
// env's own placement.
func (h *H) SetExits(at At) Endpoint {
	return JSON(200, func(c echo.Context, in PlacementsIn) (service.ExitOut, error) {
		k, id := at(c)
		return h.Orch.ExitSet(rc(c), middleware.Principal(c), k, id, in.Placements)
	})
}

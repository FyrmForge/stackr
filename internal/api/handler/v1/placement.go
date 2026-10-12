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

// NodeIn names a node; "" clears the pick.
type NodeIn struct {
	Node string `json:"node"`
}

// NodesIn is an ordered list of nodes; empty clears the list.
type NodesIn struct {
	Nodes []string `json:"nodes"`
}

// Pin answers where new envs of an org or stack go.
func (h *H) Pin(at At) Endpoint {
	return Get(func(c echo.Context) (service.PinOut, error) {
		k, id := at(c)
		return h.Orch.PinOf(rc(c), k, id)
	})
}

// SetPin pins an org or stack to a node (the node must be open to the org);
// an empty node clears it. Envs already made stay.
func (h *H) SetPin(at At) Endpoint {
	return JSON(200, func(c echo.Context, in NodeIn) (service.PinOut, error) {
		k, id := at(c)
		return h.Orch.PinNode(rc(c), middleware.Principal(c), k, id, in.Node)
	})
}

// Ingress answers the node public traffic of a scope enters through.
func (h *H) Ingress(at At) Endpoint {
	return Get(func(c echo.Context) (service.IngressOut, error) {
		k, id := at(c)
		return h.Orch.IngressGet(rc(c), k, id)
	})
}

// SetIngress picks the ingress node of a scope; an empty node clears it.
func (h *H) SetIngress(at At) Endpoint {
	return JSON(200, func(c echo.Context, in NodeIn) (service.IngressOut, error) {
		k, id := at(c)
		return h.Orch.IngressSet(rc(c), middleware.Principal(c), k, id, in.Node)
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
// env's own node.
func (h *H) SetExits(at At) Endpoint {
	return JSON(200, func(c echo.Context, in NodesIn) (service.ExitOut, error) {
		k, id := at(c)
		return h.Orch.ExitSet(rc(c), middleware.Principal(c), k, id, in.Nodes)
	})
}

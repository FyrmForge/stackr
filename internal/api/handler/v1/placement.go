package v1

import (
	"errors"
	"net/http"

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

// PinIn pins an org or stack to a placement ("" clears it). When that moves
// envs to another node, Confirm must be the org's or stack's slug.
type PinIn struct {
	Placement string `json:"placement"`
	Confirm   string `json:"confirm,omitempty"`
}

// PinSetOut is the scope's pin after the call; Jobs are the env moves the
// pin started, one per env.
type PinSetOut struct {
	Scope     string        `json:"scope"`
	Placement string        `json:"placement"`
	Jobs      []service.Job `json:"jobs,omitempty"`
}

// SetPin pins an org or stack to a placement (open to the org); an empty
// placement clears it. A pin that moves existing envs to another node (D4)
// answers 409 with one move plan per env, until the body's confirm is the
// scope's slug; then the moves start.
func (h *H) SetPin(at At) Endpoint {
	e := JSON(http.StatusOK, func(echo.Context, PinIn) (PinSetOut, error) { return PinSetOut{}, nil })
	e.Handle = func(c echo.Context) error {
		var in PinIn
		if err := decode(c, &in); err != nil {
			return err
		}
		k, id := at(c)
		jobs, err := h.Orch.PinScope(rc(c), middleware.Principal(c), k, id, in.Placement, in.Confirm)
		var mn *service.MoveNeeded
		if errors.As(err, &mn) {
			return c.JSON(http.StatusConflict, movePlansOut(mn.Plans))
		}
		if err != nil {
			return err
		}
		pin, err := h.Orch.PinOf(rc(c), k, id)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, PinSetOut{Scope: pin.Scope, Placement: pin.Placement, Jobs: jobs})
	}
	return e
}

// PinPlan is the moves pinning the org or stack to ?to=<placement> would
// cause: one plan per env that changes node; none when nothing moves.
func (h *H) PinPlan(at At) Endpoint {
	return Get(func(c echo.Context) ([]MovePlanOut, error) {
		k, id := at(c)
		ps, err := h.Orch.PlanPin(rc(c), middleware.Principal(c), k, id, c.QueryParam("to"))
		return movePlansOut(ps), err
	}).Q("to")
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

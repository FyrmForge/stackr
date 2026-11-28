package v1

import (
	"strconv"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
)

// ObserveMovePlanOut is what moving logs and metrics onto the controller's
// disk (or back to the system disk) does.
type ObserveMovePlanOut struct {
	NodeID    string `json:"node_id"`
	Back      bool   `json:"back"`
	SizeBytes int64  `json:"size_bytes"`
	Cap       uint64 `json:"cap"`
	Need      int64  `json:"need"`
	Free      uint64 `json:"free"`
	Fits      bool   `json:"fits"`
	Busy      bool   `json:"busy"`
}

// backParam is the ?back=true of both observe-move verbs.
func backParam(c echo.Context) bool {
	b, _ := strconv.ParseBool(c.QueryParam("back"))
	return b
}

// PlanMoveObserve (server admin) is the size, the disk's cap and the room check.
func (h *H) PlanMoveObserve() Endpoint {
	return Get(func(c echo.Context) (ObserveMovePlanOut, error) {
		id, err := h.nodeID(c)
		if err != nil {
			return ObserveMovePlanOut{}, err
		}
		p, err := h.Orch.PlanMoveObserve(rc(c), middleware.Principal(c), id, backParam(c))
		return ObserveMovePlanOut(p), err
	})
}

// MoveObserve (server admin) queues the move of logs and metrics; observe
// stops while it copies.
func (h *H) MoveObserve() Endpoint {
	return Job(func(c echo.Context, _ None) (service.Job, error) {
		id, err := h.nodeID(c)
		if err != nil {
			return service.Job{}, err
		}
		return h.Orch.MoveObserve(rc(c), middleware.Principal(c), id, backParam(c))
	})
}

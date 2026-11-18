// Package waiting is the Waiting on you page: what needs the signed-in
// user's action, kept current by its own poll.
package waiting

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/ui/components"
	"github.com/FyrmForge/stackr/internal/web/render"
)

type handler struct{ orch *service.Orchestrator }

func NewHandler(orch *service.Orchestrator) *handler { return &handler{orch: orch} }

// GET /-/waiting
func (h *handler) Page(c echo.Context) error {
	w, err := h.orch.WaitingOnYou(c.Request().Context(), middleware.Principal(c))
	if err != nil {
		return middleware.HTTPError(err)
	}
	v := components.WaitingView{Count: w.Count(), Live: "/-/waiting"}
	for _, g := range w.Groups {
		gv := components.WaitGroupView{Title: g.Title}
		for _, it := range g.Items {
			gv.Rows = append(gv.Rows, components.WaitRowView(it))
		}
		v.Groups = append(v.Groups, gv)
	}
	for _, it := range w.Others {
		v.Others = append(v.Others, components.WaitRowView(it))
	}
	return render.Page(c, http.StatusOK, "Waiting on you", components.Waiting(v))
}

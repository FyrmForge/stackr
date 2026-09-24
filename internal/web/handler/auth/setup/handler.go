// Package setup is the first-run page after register: the first admin names
// the first org, anyone else is told to ask for an invite.
package setup

import (
	"net/http"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/ui/dialog"
	"github.com/FyrmForge/stackr/internal/web/render"
)

type handler struct{ orch *service.Orchestrator }

func NewHandler(orch *service.Orchestrator) *handler { return &handler{orch: orch} }

// GET /setup. A user who is in an org already has nothing to set up. The
// form posts to the home canvas's create route, which org.create gates.
func (h *handler) Page(c echo.Context) error {
	p := middleware.Principal(c)
	orgs, err := h.orch.Orgs(c.Request().Context(), p.User.ID)
	if err != nil {
		return middleware.HTTPError(err)
	}
	if len(orgs) > 0 {
		return respond.Redirect(c, "/"+orgs[0].Slug)
	}
	var v View
	if p.Access.Admin {
		v.Create = &dialog.CreateLevelView{Kind: "org", Action: "/-/new-org"}
	}
	return render.Page(c, http.StatusOK, "Set up", page(v))
}

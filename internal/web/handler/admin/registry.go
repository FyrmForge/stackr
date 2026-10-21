package admin

import (
	"strconv"
	"strings"

	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	comp "github.com/FyrmForge/stackr/internal/ui/components"
	ui "github.com/FyrmForge/stackr/internal/ui/drawer/admin"
	"github.com/FyrmForge/stackr/internal/web/render"
)

func (h *handler) mountRegistry(g *echo.Group, a *middleware.Access) {
	g.POST(ui.Base+"/registry/clean", h.act("registry", func(c echo.Context) (string, *service.Job, error) {
		j, err := h.orch.CleanRegistryNow(c.Request().Context(), middleware.Principal(c).User.ID)
		return "Cleanup queued.", &j, err
	}), a.Require("container.admin"))
}

// registryTab is what every org holds in the registry and the last cleanup.
// The registry being down is the tab's text.
func (h *handler) registryTab(c echo.Context, job *comp.JobStatusView) (templ.Component, error) {
	v := ui.RegistryView{Job: job, Clean: comp.ConfirmView{
		Button:  "Clean up now",
		Title:   "Clean up the registry now?",
		Warning: "Pushes pause for a few seconds.",
		Action:  ui.Base + "/registry/clean",
		Target:  "#" + comp.DrawerRoot,
		Primary: true,
	}}
	o, err := h.orch.RegistryOverview(c.Request().Context())
	if err != nil {
		v.Err = err.Error()
		return ui.Registry(v), nil
	}
	v.Total, v.Versions = render.Size(o.Bytes), strconv.Itoa(o.Versions)
	for _, u := range o.Orgs {
		v.Orgs = append(v.Orgs, ui.RegistryOrg{Org: u.Org, Versions: strconv.Itoa(u.Versions), Size: render.Size(u.Bytes)})
	}
	if l := o.Last; l != nil {
		v.Last, v.LastAt = strconv.Itoa(l.Deleted)+" versions deleted, "+render.Size(l.Freed)+" freed.", l.At
		for _, e := range l.Errors {
			v.LastErrors = append(v.LastErrors, strings.TrimSpace(e))
		}
	}
	return ui.Registry(v), nil
}

package admin

import (
	"net/http"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	comp "github.com/FyrmForge/stackr/internal/ui/components"
	ui "github.com/FyrmForge/stackr/internal/ui/drawer/admin"
	"github.com/FyrmForge/stackr/internal/ui/drawer/vars"
	"github.com/FyrmForge/stackr/internal/web/render"
)

// mountParams is the Params tab's two writes: the same editor and form as an
// org's, on the server scope. Only the server file reads these.
func (h *handler) mountParams(g *echo.Group, a *middleware.Access) {
	w := a.Require("serverparams.write")
	g.POST(ui.Base+"/params", h.SetParams, w)
	g.POST(ui.Base+"/params/delete", h.DeleteParam, w)
}

// params is the server scope's editor. A secret's value never reaches the view.
func (h *handler) params(c echo.Context, errMsg, note string) (templ.Component, error) {
	rows, err := h.orch.Params(c.Request().Context(), service.ServerParamScope, true)
	if err != nil {
		return nil, err
	}
	v := vars.View{
		Scope:  "the server",
		Lead:   "Only stackr-server.yml reads these.",
		Error:  errMsg,
		Note:   note,
		Editor: comp.ParamEditorView{Action: ui.Base + "/params", DeleteAction: ui.Base + "/params/delete"},
	}
	for _, p := range rows {
		if p.Kind == "secret" {
			v.Editor.Secrets = append(v.Editor.Secrets, comp.SecretRowView{
				Collection: p.Collection,
				Name:       p.Name,
				Set:        p.Value != "",
				DecidedBy:  "server",
			})
		} else {
			v.Editor.Params = append(v.Editor.Params, comp.ParamRowView{
				Collection: p.Collection,
				Name:       p.Name,
				Value:      p.Value,
				DecidedBy:  "server",
			})
		}
	}
	return vars.Editor(v), nil
}

// POST /-/admin/params: merge the form; answers the editor into #vars-editor.
func (h *handler) SetParams(c echo.Context) error {
	form, err := c.FormParams()
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest)
	}
	_, err = h.orch.SetParams(c.Request().Context(), service.ServerParamScope, render.ParamEntries(form))
	return h.paramsAfter(c, "Saved.", err)
}

// POST /-/admin/params/delete (collection, name).
func (h *handler) DeleteParam(c echo.Context) error {
	_, err := h.orch.DeleteParam(c.Request().Context(), service.ServerParamScope, c.FormValue("collection"), c.FormValue("name"))
	return h.paramsAfter(c, "Deleted.", err)
}

func (h *handler) paramsAfter(c echo.Context, note string, err error) error {
	status, msg := http.StatusOK, ""
	if err != nil {
		var ok bool
		if msg, ok = render.Refused(err); !ok {
			return middleware.HTTPError(err)
		}
		status, note = http.StatusUnprocessableEntity, ""
	}
	body, err := h.params(c, msg, note)
	if err != nil {
		return middleware.HTTPError(err)
	}
	c.Response().Header().Set("HX-Retarget", "#"+vars.Root)
	c.Response().Header().Set("HX-Reswap", "outerHTML")
	return respond.HTML(c, status, body)
}

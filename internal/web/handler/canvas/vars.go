package canvas

import (
	"net/http"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	comp "github.com/FyrmForge/stackr/internal/ui/components"
	"github.com/FyrmForge/stackr/internal/ui/drawer/vars"
	"github.com/FyrmForge/stackr/internal/web/render"
)

// paramScope is the deepest level s names.
func paramScope(s service.Scope) (service.ParamScope, string) {
	switch {
	case s.Env != nil:
		return service.ParamScope{Kind: "env", ID: s.Env.ID}, "env " + s.Env.Name
	case s.Stack != nil:
		return service.ParamScope{Kind: "stack", ID: s.Stack.ID}, "stack " + s.Stack.Name
	}
	return service.ParamScope{Kind: "org", ID: s.Org.ID}, "org " + s.Org.Name
}

// vars is the editor of s's own params. Secrets are read only with
// variable.write; a secret's value never reaches the view.
// ponytail: this level's rows only; "decided by" and overrides need the
// resolved chain, which no verb returns yet.
func (h *handler) vars(c echo.Context, s service.Scope, errMsg, note string) (templ.Component, error) {
	ps, name := paramScope(s)
	write := can(c, s, "variable.write")
	rows, err := h.orch.Params(c.Request().Context(), ps, write)
	if err != nil {
		return nil, err
	}
	base := urlOf(s) + "/-/vars"
	v := vars.View{
		Scope:  name,
		Error:  errMsg,
		Note:   note,
		Editor: comp.ParamEditorView{Action: base, DeleteAction: base + "/delete"},
	}
	if !write {
		v.Editor = comp.ParamEditorView{
			ReadOnly: true,
			Why:      "Your role reads params; changing them, and seeing secrets, needs write.",
		}
	}
	for _, p := range rows {
		if p.Kind == "secret" {
			v.Editor.Secrets = append(v.Editor.Secrets, comp.SecretRowView{
				Collection: p.Collection,
				Name:       p.Name,
				Set:        p.Value != "",
				DecidedBy:  ps.Kind,
			})
		} else {
			v.Editor.Params = append(v.Editor.Params, comp.ParamRowView{
				Collection: p.Collection,
				Name:       p.Name,
				Value:      p.Value,
				DecidedBy:  ps.Kind,
			})
		}
	}
	return vars.Editor(v), nil
}

// POST <level>/-/vars: merge the form; answers the editor into #vars-editor.
func (h *handler) SetVars(c echo.Context) error {
	form, err := c.FormParams()
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest)
	}
	s := middleware.ScopeOf(c)
	ps, _ := paramScope(s)
	_, err = h.orch.SetParams(c.Request().Context(), ps, render.ParamEntries(form))
	return h.varsAfter(c, s, "Saved.", err)
}

// POST <level>/-/vars/delete (collection, name).
func (h *handler) DeleteVar(c echo.Context) error {
	s := middleware.ScopeOf(c)
	ps, _ := paramScope(s)
	_, err := h.orch.DeleteParam(c.Request().Context(), ps, c.FormValue("collection"), c.FormValue("name"))
	return h.varsAfter(c, s, "Deleted.", err)
}

func (h *handler) varsAfter(c echo.Context, s service.Scope, note string, err error) error {
	status, msg := http.StatusOK, ""
	if err != nil {
		var ok bool
		if msg, ok = refused(err); !ok {
			return middleware.HTTPError(err)
		}
		status, note = http.StatusUnprocessableEntity, ""
	}
	body, err := h.vars(c, s, msg, note)
	if err != nil {
		return middleware.HTTPError(err)
	}
	c.Response().Header().Set("HX-Retarget", "#"+vars.Root)
	c.Response().Header().Set("HX-Reswap", "outerHTML")
	return respond.HTML(c, status, body)
}

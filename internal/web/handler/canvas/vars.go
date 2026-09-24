package canvas

import (
	"net/http"
	"sort"
	"strings"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	comp "github.com/FyrmForge/stackr/internal/ui/components"
	"github.com/FyrmForge/stackr/internal/ui/drawer/vars"
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

// entries reads the editor's form: param.<collection>.<name>,
// secret.<collection>.<name> (empty = keep the stored one) and the new_ row.
func entries(form map[string][]string) []service.ParamEntry {
	var out []service.ParamEntry
	for k, vs := range form {
		kind, rest, ok := strings.Cut(k, ".")
		coll, name, ok2 := strings.Cut(rest, ".")
		if !ok || !ok2 || (kind != "param" && kind != "secret") {
			continue
		}
		out = append(out, service.ParamEntry{
			Collection: coll,
			Name:       name,
			Kind:       kind,
			Value:      vs[0],
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Collection+"."+out[i].Name < out[j].Collection+"."+out[j].Name
	})
	if n := strings.TrimSpace(first(form["new_name"])); n != "" {
		out = append(out, service.ParamEntry{
			Collection: strings.TrimSpace(first(form["new_collection"])),
			Name:       n,
			Kind:       first(form["new_kind"]),
			Value:      first(form["new_value"]),
		})
	}
	return out
}

func first(vs []string) string {
	if len(vs) == 0 {
		return ""
	}
	return vs[0]
}

// POST <level>/-/vars: merge the form; answers the editor into #vars-editor.
func (h *handler) SetVars(c echo.Context) error {
	form, err := c.FormParams()
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest)
	}
	s := middleware.ScopeOf(c)
	ps, _ := paramScope(s)
	return h.varsAfter(c, s, "Saved.", h.orch.SetParams(c.Request().Context(), ps, entries(form)))
}

// POST <level>/-/vars/delete (collection, name).
func (h *handler) DeleteVar(c echo.Context) error {
	s := middleware.ScopeOf(c)
	ps, _ := paramScope(s)
	return h.varsAfter(
		c,
		s,
		"Deleted.",
		h.orch.DeleteParam(c.Request().Context(), ps, c.FormValue("collection"), c.FormValue("name")),
	)
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

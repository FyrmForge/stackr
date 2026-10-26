package canvas

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	comp "github.com/FyrmForge/stackr/internal/ui/components"
	"github.com/FyrmForge/stackr/internal/ui/drawer/vars"
	"github.com/FyrmForge/stackr/internal/web/render"
)

// col is one column of the params grid and the scope it edits.
type col struct {
	vars.Col
	ps service.ParamScope
}

// cols are the columns of s's grid, which are also the only scopes its
// forms may write: org = its tiers and pr, or all and pr while it has no
// tiers; stack = its static envs and pr; env = that env alone.
func (h *handler) cols(ctx context.Context, s service.Scope) ([]col, string, error) {
	tiers, err := h.orch.Tiers(ctx, s.Org.ID)
	if err != nil {
		return nil, "", err
	}
	pr := col{vars.Col{Token: "org_pr", Label: "pr", Sub: "sandbox", Hue: "teal"}, service.ParamScope{Kind: "org_pr", ID: s.Org.ID}}
	if s.Stack == nil {
		if len(tiers) == 0 {
			all := col{vars.Col{Token: "org", Label: "all", Sub: "org", Hue: comp.EnvColors[0]}, service.ParamScope{Kind: "org", ID: s.Org.ID}}
			return []col{all, pr}, "org " + s.Org.Name, nil
		}
		var out []col
		for i, t := range tiers {
			out = append(out, col{vars.Col{Token: "tier:" + t.ID, Label: t.Slug, Sub: "tier", Hue: comp.EnvColors[(i+1)%len(comp.EnvColors)], Locked: t.Locked}, service.ParamScope{Kind: "tier", ID: t.ID}})
		}
		return append(out, pr), "org " + s.Org.Name, nil
	}
	inTier := map[string]service.Tier{}
	for _, t := range tiers {
		inTier[t.Slug] = t
	}
	pr = col{vars.Col{Token: "stack_pr", Label: "pr", Sub: "sandbox", Hue: "teal"}, service.ParamScope{Kind: "stack_pr", ID: s.Stack.ID}}
	es, err := h.orch.Ladder(ctx, s.Stack.ID)
	if err != nil {
		return nil, "", err
	}
	hues := service.EnvHues(es)
	var out []col
	for _, e := range es {
		c := col{vars.Col{Token: "env:" + e.ID, Label: e.Slug, Sub: "env", Hue: hues[e.ID], Locked: e.Locked}, service.ParamScope{Kind: "env", ID: e.ID}}
		if len(tiers) > 0 {
			if t, ok := inTier[e.Slug]; ok {
				c.Sub, c.Locked = "tier", t.Locked
			} else {
				c.Sub, c.Own = "off-tier", true
			}
		}
		if s.Env == nil || s.Env.ID == e.ID {
			out = append(out, c)
		}
	}
	name := "stack " + s.Stack.Name
	if s.Env != nil {
		name = "env " + s.Env.Name
		if s.Env.Type == "ephemeral" { // a PR env reads its stack's pr block
			return []col{pr}, name, nil
		}
		return out, name, nil
	}
	return append(out, pr), name, nil
}

// pick is the column a form names (scope), or the only one.
func pick(cs []col, token string) (col, bool) {
	if token == "" && len(cs) == 1 {
		return cs[0], true
	}
	for _, c := range cs {
		if c.Token == token {
			return c, true
		}
	}
	return col{}, false
}

// vars is the grid of s's own params. A secret's value never reaches the view.
// ponytail: no drift chip; the file's value per cell is not returned by any
// verb the editor can call cheaply (the plan computes it per apply).
func (h *handler) vars(c echo.Context, s service.Scope, errMsg, note string) (templ.Component, error) {
	ctx := c.Request().Context()
	write := can(c, s, "variable.write")
	cs, name, err := h.cols(ctx, s)
	if err != nil {
		return nil, err
	}
	base := urlOf(s) + "/-/vars"
	v := vars.View{Scope: name, Error: errMsg, Note: note}
	g, err := h.grid(ctx, cs, write)
	if err != nil {
		return nil, err
	}
	g.Action, g.DeleteAction = base, base+"/delete"
	if !write {
		g.ReadOnly, g.Why = true, "Only members and owners can change variables or see secrets."
	}
	if form, _ := c.FormParams(); c.Request().Method == http.MethodPost && errMsg == "" && form.Has("add_form") {
		group := form.Get("add_form") // the note right after the add form that posted
		g.Added, g.Note = &group, note
	}
	v.Grid = &g
	return vars.Editor(v), nil
}

// grid reads every column's scope. A writer sees secret names (masked: the
// value is never read); a reader sees params only.
func (h *handler) grid(ctx context.Context, cs []col, write bool) (vars.Grid, error) {
	g := vars.Grid{}
	type key struct{ coll, name string }
	cells := map[key][]vars.Cell{}
	plain := map[key]bool{} // a name with a plain cell is not a secret row
	for i, c := range cs {
		g.Cols = append(g.Cols, c.Col)
		var rows []service.Param
		var err error
		if write {
			rows, err = h.orch.MaskedParams(ctx, c.ps)
		} else {
			rows, err = h.orch.Params(ctx, c.ps, false)
		}
		if err != nil {
			return g, err
		}
		for _, p := range rows {
			k := key{p.Collection, p.Name}
			if cells[k] == nil {
				cells[k] = make([]vars.Cell, len(cs))
			}
			cells[k][i] = vars.Cell{Value: p.Value, Set: true, Secret: p.Kind == "secret"}
			if p.Kind == "secret" {
				cells[k][i].Value = ""
			} else {
				plain[k] = true
			}
		}
	}
	for _, k := range slices.SortedFunc(maps.Keys(cells), func(a, b key) int {
		return cmp.Or(strings.Compare(a.coll, b.coll), strings.Compare(a.name, b.name))
	}) {
		if n := len(g.Groups); n == 0 || g.Groups[n-1].Name != k.coll {
			g.Groups = append(g.Groups, vars.Group{Name: k.coll})
		}
		gr := &g.Groups[len(g.Groups)-1]
		gr.Rows = append(gr.Rows, vars.Row{Name: k.name, Secret: !plain[k], Cells: cells[k]})
	}
	return g, nil
}

// POST <level>/-/vars: one cell (scope + param./secret. fields), or an Add
// row (add_group, add_name, add_kind, add_value.<scope>). Answers the editor into #vars-editor.
func (h *handler) SetVars(c echo.Context) error {
	form, err := c.FormParams()
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest)
	}
	s := middleware.ScopeOf(c)
	ctx := c.Request().Context()
	cs, _, err := h.cols(ctx, s)
	if err != nil {
		return middleware.HTTPError(err)
	}
	if form.Has("add_form") || strings.TrimSpace(form.Get("add_name")) != "" {
		return h.addVar(c, s, cs, form)
	}
	col, ok := pick(cs, form.Get("scope"))
	if !ok {
		return echo.NewHTTPError(http.StatusBadRequest)
	}
	_, err = h.orch.SetParams(ctx, col.ps, render.ParamEntries(form))
	return h.varsAfter(c, s, "Saved.", err)
}

// addVar is an Add row: a group, a name and a value in at least one
// column. Every refusal comes at once, each under its own input; nothing is
// written until all pass.
func (h *handler) addVar(c echo.Context, s service.Scope, cs []col, form url.Values) error {
	ctx := c.Request().Context()
	e := service.ParamEntry{Collection: strings.TrimSpace(form.Get("add_group")), Name: strings.TrimSpace(form.Get("add_name")), Kind: "param"}
	field := "add_value."
	if form.Get("add_kind") == "secret" {
		e.Kind, field = "secret", "add_secret."
	}
	var bad []error // the shape first: FieldErrs keeps a field's first reason
	if e.Collection == "" {
		bad = append(bad, errs.Invalidf("add_group", "Give the group a name."))
	}
	if e.Name == "" {
		bad = append(bad, errs.Invalidf("add_name", "Give the variable a name."))
	}
	bad = append(bad, render.AsFields(h.orch.CheckParam(e), map[string]string{"collection": "add_group", "name": "add_name"}))
	var set []col
	for _, col := range cs {
		if form.Get(field+col.Token) != "" {
			set = append(set, col)
		}
	}
	if len(set) == 0 {
		bad = append(bad, errs.Invalidf("add_value", "Set a value in at least one column."))
	}
	if err := errors.Join(bad...); err != nil {
		return h.varsAfter(c, s, "", err)
	}
	for _, col := range set {
		e.Value = form.Get(field + col.Token)
		if _, err := h.orch.SetParams(ctx, col.ps, []service.ParamEntry{e}); err != nil {
			return h.varsAfter(c, s, "", render.AsFields(err, map[string]string{"collection": "add_group", "name": "add_name"}))
		}
	}
	return h.varsAfter(c, s, "Saved.", nil)
}

// POST <level>/-/vars/delete (collection, name).
func (h *handler) DeleteVar(c echo.Context) error {
	s := middleware.ScopeOf(c)
	ctx := c.Request().Context()
	cs, _, err := h.cols(ctx, s)
	if err != nil {
		return middleware.HTTPError(err)
	}
	col, ok := pick(cs, c.FormValue("scope"))
	if !ok {
		return echo.NewHTTPError(http.StatusBadRequest)
	}
	_, err = h.orch.DeleteParam(ctx, col.ps, c.FormValue("collection"), c.FormValue("name"))
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
		// an add or cell form shows its own refusal, open with what was typed
		if form, _ := c.FormParams(); vars.Takes(form) {
			r := &comp.Refused{Msg: msg, Vals: form, More: render.FieldErrs(err)}
			c.SetRequest(c.Request().WithContext(comp.WithRefused(c.Request().Context(), r)))
			msg = ""
		}
	}
	body, err := h.vars(c, s, msg, note)
	if err != nil {
		return middleware.HTTPError(err)
	}
	c.Response().Header().Set("HX-Retarget", "#"+vars.Root)
	c.Response().Header().Set("HX-Reswap", "outerHTML")
	return respond.HTML(c, status, body)
}

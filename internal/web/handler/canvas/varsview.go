package canvas

import (
	"context"
	"net/http"
	"strings"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/ui/drawer/vars"
	"github.com/FyrmForge/stackr/internal/web/render"
)

// varsReply answers one editor state into #vars-editor. It may carry a
// secret, so it is never cached.
func (h *handler) varsReply(c echo.Context, s service.Scope, mods ...func(*vars.Grid)) error {
	body, err := h.vars(c, s, "", "", mods...)
	if err != nil {
		return middleware.HTTPError(err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	c.Response().Header().Set("HX-Retarget", "#"+vars.Root)
	c.Response().Header().Set("HX-Reswap", "outerHTML")
	return respond.HTML(c, http.StatusOK, body)
}

// GET <level>/-/vars/edit?scope&group&name: the editor with that cell open as
// an input; with no cell, with nothing open (Cancel, Undo, Hide).
func (h *handler) EditVar(c echo.Context) error {
	s := middleware.ScopeOf(c)
	cs, _, err := h.cols(c.Request().Context(), s)
	if err != nil {
		return middleware.HTTPError(err)
	}
	cl, ok := pick(cs, c.QueryParam("scope"))
	if c.QueryParam("name") == "" || !ok {
		return h.varsReply(c, s)
	}
	ref := &vars.CellRef{Scope: cl.Token, Group: c.QueryParam("group"), Name: c.QueryParam("name")}
	return h.varsReply(c, s, func(g *vars.Grid) { g.Edit = ref })
}

// POST <level>/-/vars/reveal (scope, collection, name): the editor with that
// one secret shown. The route needs the right that editing needs; the value
// is read for this call only and the reveal is logged by name.
func (h *handler) RevealVar(c echo.Context) error {
	s := middleware.ScopeOf(c)
	ctx := c.Request().Context()
	cs, _, err := h.cols(ctx, s)
	if err != nil {
		return middleware.HTTPError(err)
	}
	cl, ok := pick(cs, c.FormValue("scope"))
	if !ok {
		return echo.NewHTTPError(http.StatusBadRequest)
	}
	group, name := c.FormValue("collection"), c.FormValue("name")
	val, err := h.orch.RevealParam(ctx, middleware.Principal(c), cl.ps, group, name)
	if err != nil {
		return middleware.HTTPError(err)
	}
	return h.varsReply(c, s, func(g *vars.Grid) {
		g.Shown = &vars.Shown{CellRef: vars.CellRef{Scope: cl.Token, Group: group, Name: name}, Value: val}
	})
}

// POST <level>/-/vars/preview: one edited cell (scope + param./secret.
// field), held and shown with what applying would do to the running tiles.
// Nothing is written.
func (h *handler) PreviewVar(c echo.Context) error {
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
	cl, ok := pick(cs, form.Get("scope"))
	es := render.ParamEntries(form)
	if !ok || len(es) != 1 {
		return echo.NewHTTPError(http.StatusBadRequest)
	}
	e := es[0]
	p, err := h.pendingOf(ctx, cl, e)
	if err != nil {
		return middleware.HTTPError(err)
	}
	if p == nil { // nothing to apply
		return h.varsReply(c, s)
	}
	p.Label = e.Collection + "." + e.Name
	if len(cs) > 1 {
		p.Label += " in " + cl.Label
	}
	return h.varsReply(c, s, func(g *vars.Grid) { g.Pending = p })
}

// pendingOf is the held change of e in cl: nil when it changes nothing (a
// blank secret keeps the stored one, a plain value the same as it was).
func (h *handler) pendingOf(ctx context.Context, cl col, e service.ParamEntry) (*vars.Pending, error) {
	p := &vars.Pending{
		CellRef: vars.CellRef{Scope: cl.Token, Group: e.Collection, Name: e.Name},
		Field:   e.Kind + "." + e.Collection + "." + e.Name,
		Value:   e.Value,
	}
	rows, err := h.orch.MaskedParams(ctx, cl.ps)
	if err != nil {
		return nil, err
	}
	was, old := false, ""
	for _, r := range rows {
		if r.Collection == e.Collection && r.Name == e.Name {
			was, old = true, r.Value
		}
	}
	p.Old = "unset"
	if e.Kind == "secret" {
		if e.Value == "" {
			return nil, nil
		}
		p.New = "a new value"
		if was {
			p.Old = "hidden"
		}
	} else {
		if was && old == e.Value {
			return nil, nil
		}
		p.New = e.Value
		if was {
			p.Old = old
		}
		if e.Value == "" {
			p.New = "empty"
		}
	}
	waits, err := h.orch.UnsetParams(ctx, cl.ps)
	if err != nil {
		return nil, err
	}
	if tiles := waits[e.Collection+"."+e.Name]; len(tiles) > 0 && e.Kind == "secret" {
		p.Starts, p.Tiles, p.Impact = true, len(tiles), []string{"Starts " + strings.Join(tiles, ", ") + "."}
		return p, nil
	}
	tiles, err := h.orch.ParamImpact(ctx, cl.ps, []service.ParamEntry{e})
	if err != nil {
		return nil, err
	}
	if len(tiles) > 0 {
		p.Tiles, p.Impact = len(tiles), []string{"Restarts " + strings.Join(tiles, ", ") + "."}
	}
	return p, nil
}

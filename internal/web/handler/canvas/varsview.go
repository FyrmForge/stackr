package canvas

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/ui/drawer/vars"
	"github.com/FyrmForge/stackr/internal/web/render"
)

// varsReplyMsg answers one editor state into #vars-editor, with a refusal
// shown above it. It may carry a secret, so it is never cached.
func (h *handler) varsReplyMsg(c echo.Context, s service.Scope, msg string, mods ...func(*vars.Grid)) error {
	body, err := h.vars(c, s, msg, "", mods...)
	if err != nil {
		return middleware.HTTPError(err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	c.Response().Header().Set("HX-Retarget", "#"+vars.Root)
	c.Response().Header().Set("HX-Reswap", "outerHTML")
	return respond.HTML(c, http.StatusOK, body)
}

// heldIn is the changes a request carries from the footer of the held ones
// (held.param.g.n, held.secret.g.n), minus the one `drop` names (Undo).
func heldIn(form url.Values) []service.ParamEntry {
	in := url.Values{}
	for k, v := range form {
		if rest, ok := strings.CutPrefix(k, "held."); ok {
			in[rest] = v
		}
	}
	es := render.ParamEntries(in)
	return slices.DeleteFunc(es, func(e service.ParamEntry) bool { return e.Kind+"."+e.Collection+"."+e.Name == form.Get("drop") })
}

// entriesOf is what a save posts: the held changes and an open input's value.
func entriesOf(form url.Values) []service.ParamEntry {
	es := heldIn(form)
	for _, e := range render.ParamEntries(form) {
		if i := slices.IndexFunc(es, func(x service.ParamEntry) bool { return x.Collection == e.Collection && x.Name == e.Name }); i >= 0 {
			es[i] = e
		} else {
			es = append(es, e)
		}
	}
	return es
}

// heldReply draws the editor with the changes the form holds, add (a cell just
// edited) merged over them, and open as the one cell with an input. A change
// for another column than the held ones is refused: they are applied or
// cancelled first.
func (h *handler) heldReply(c echo.Context, s service.Scope, cs []col, form url.Values, scope string, add []service.ParamEntry, open *vars.CellRef, openSecret bool, mods ...func(*vars.Grid)) error {
	held := heldIn(form)
	hs, msg := form.Get("held_scope"), ""
	// an open input's typed value rides along on any redraw (typed_scope names
	// its column; Cancel blanks it) and is held like a blur would have held it
	if ts := form.Get("typed_scope"); add == nil && ts != "" && (scope == "" || scope == ts) && (hs == "" || hs == ts) {
		add, scope = render.ParamEntries(form), ts
	}
	if len(held) > 0 && scope != "" && scope != hs && (len(add) > 0 || open != nil) {
		msg = "Apply or cancel the held changes first."
		add, open = nil, nil
	}
	if len(held) > 0 || scope == "" {
		scope = hs
	}
	if scope == "" && len(add) == 0 {
		return h.varsReplyMsg(c, s, msg, append(mods, func(g *vars.Grid) { g.Edit = open })...)
	}
	cl, ok := pick(cs, scope)
	if !ok {
		return echo.NewHTTPError(http.StatusBadRequest)
	}
	es := held
	for _, e := range add {
		if i := slices.IndexFunc(es, func(x service.ParamEntry) bool { return x.Collection == e.Collection && x.Name == e.Name }); i >= 0 {
			es[i] = e
		} else {
			es = append(es, e)
		}
	}
	var draft *service.ParamEntry
	if open != nil && openSecret && open.Scope == cl.Token && !slices.ContainsFunc(es, func(x service.ParamEntry) bool { return x.Collection == open.Group && x.Name == open.Name }) {
		draft = &service.ParamEntry{Collection: open.Group, Name: open.Name, Kind: "secret", Value: "x"}
	}
	hd, err := h.heldOf(c.Request().Context(), cl, es, draft, len(cs) > 1)
	if err != nil {
		return middleware.HTTPError(err)
	}
	return h.varsReplyMsg(c, s, msg, append(mods, func(g *vars.Grid) { g.Held, g.Edit = hd, open })...)
}

// GET or POST <level>/-/vars/edit (scope, group, name): the editor with that
// cell open as an input; with no cell, with nothing open (Cancel, Hide). A
// POST carries the held changes along.
func (h *handler) EditVar(c echo.Context) error {
	s := middleware.ScopeOf(c)
	form, err := c.FormParams()
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest)
	}
	cs, _, err := h.cols(c.Request().Context(), s)
	if err != nil {
		return middleware.HTTPError(err)
	}
	cl, ok := pick(cs, form.Get("scope"))
	if form.Get("name") == "" || !ok {
		return h.heldReply(c, s, cs, form, "", nil, nil, false)
	}
	ref := &vars.CellRef{Scope: cl.Token, Group: form.Get("group"), Name: form.Get("name")}
	return h.heldReply(c, s, cs, form, cl.Token, nil, ref, form.Get("secret") != "")
}

// POST <level>/-/vars/reveal (scope, collection, name): the editor with that
// one secret shown. The route needs the right that editing needs; the value
// is read for this call only and the reveal is logged by name.
func (h *handler) RevealVar(c echo.Context) error {
	s := middleware.ScopeOf(c)
	ctx := c.Request().Context()
	form, err := c.FormParams()
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest)
	}
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
	return h.heldReply(c, s, cs, form, "", nil, nil, false, func(g *vars.Grid) {
		g.Shown = &vars.Shown{CellRef: vars.CellRef{Scope: cl.Token, Group: group, Name: name}, Value: val}
	})
}

// POST <level>/-/vars/preview: the held changes (held.*) plus one edited cell
// (scope + param./secret. field), or one to drop (Undo). Each is shown with what
// applying would do to the running tiles. Nothing is written.
func (h *handler) PreviewVar(c echo.Context) error {
	form, err := c.FormParams()
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest)
	}
	s := middleware.ScopeOf(c)
	cs, _, err := h.cols(c.Request().Context(), s)
	if err != nil {
		return middleware.HTTPError(err)
	}
	es := render.ParamEntries(form)
	if len(es) > 1 || (len(es) == 0 && form.Get("drop") == "") {
		return echo.NewHTTPError(http.StatusBadRequest)
	}
	return h.heldReply(c, s, cs, form, form.Get("scope"), es, nil, false)
}

// heldOf is the held changes of es in cl, and what applying all of them does
// to the running tiles. A change that changes nothing (a blank secret keeps the
// stored one, a plain value the same as it was) is dropped. draft is a secret
// whose input is still open: listed, its value not held. nil when nothing is held.
func (h *handler) heldOf(ctx context.Context, cl col, es []service.ParamEntry, draft *service.ParamEntry, multi bool) (*vars.Held, error) {
	rows, err := h.orch.MaskedParams(ctx, cl.ps)
	if err != nil {
		return nil, err
	}
	var held vars.Held
	var rest []service.ParamEntry
	var starts []string
	waits, err := h.orch.UnsetParams(ctx, cl.ps)
	if err != nil {
		return nil, err
	}
	for _, e := range append(slices.Clone(es), orEmpty(draft)...) {
		p := pendingOf(cl, rows, e, draft != nil && e == *draft)
		if p == nil {
			continue
		}
		p.Label = e.Collection + "." + e.Name
		if multi {
			p.Label += " in " + cl.Label
		}
		held.Changes = append(held.Changes, *p)
		if tiles := waits[e.Collection+"."+e.Name]; len(tiles) > 0 && e.Kind == "secret" {
			for _, t := range tiles {
				if !slices.Contains(starts, t) {
					starts = append(starts, t)
				}
			}
		} else {
			rest = append(rest, e)
		}
	}
	if len(held.Changes) == 0 {
		return nil, nil
	}
	var redeploy []string
	if len(rest) > 0 {
		if redeploy, err = h.orch.ParamImpact(ctx, cl.ps, rest); err != nil {
			return nil, err
		}
	}
	slices.Sort(starts)
	if len(starts) > 0 {
		held.Impact = append(held.Impact, "Starts "+strings.Join(starts, ", ")+".")
	}
	if len(redeploy) > 0 {
		held.Impact = append(held.Impact, "Redeploys "+strings.Join(redeploy, ", ")+".")
	}
	held.Tiles = len(slices.Compact(slices.Sorted(slices.Values(slices.Concat(starts, redeploy)))))
	held.Starts = len(redeploy) == 0 && len(starts) > 0
	return &held, nil
}

func orEmpty(e *service.ParamEntry) []service.ParamEntry {
	if e == nil {
		return nil
	}
	return []service.ParamEntry{*e}
}

// pendingOf is the held change of e against the stored rows: nil when it
// changes nothing.
func pendingOf(cl col, rows []service.Param, e service.ParamEntry, draft bool) *vars.Pending {
	p := &vars.Pending{
		CellRef: vars.CellRef{Scope: cl.Token, Group: e.Collection, Name: e.Name},
		Field:   e.Kind + "." + e.Collection + "." + e.Name,
		Value:   e.Value,
		Draft:   draft,
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
			return nil
		}
		p.New = "a new value"
		if was {
			p.Old = "hidden"
		}
		return p
	}
	if was && old == e.Value {
		return nil
	}
	p.New = e.Value
	if was {
		p.Old = old
	}
	if e.Value == "" {
		p.New = "empty"
	}
	return p
}

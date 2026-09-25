// Package canvas serves every level's canvas (ui-plan §2) from one
// handler: the page at /, /:org, /:org/:stack and /:org/:stack/:env, and
// under each a reserved "/-/" segment (never a slug) for its helper
// routes: positions, reset, notes, events, and the drawers and create
// dialogs of the cards on it (drawer.go). The level comes from
// the path the access middleware resolved.
package canvas

import (
	"context"
	"hash/fnv"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/api/stream"
	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	ui "github.com/FyrmForge/stackr/internal/ui/graph"
	"github.com/FyrmForge/stackr/internal/ui/graph/cards"
	"github.com/FyrmForge/stackr/internal/web/render"
)

type handler struct{ orch *service.Orchestrator }

func NewHandler(orch *service.Orchestrator) *handler { return &handler{orch: orch} }

// Every is how often a canvas stream rebuilds its view (a status pass
// reads Docker per tile, so slower than stream.PollEvery).
var Every = 3 * time.Second

// level is the canvas the request is on.
type level struct {
	scope service.CanvasScope
	base  string // "" on home
	title string
	empty string // v0's line for a canvas with nothing on it yet
}

func where(c echo.Context) level {
	sc := middleware.ScopeOf(c)
	switch {
	case sc.Env != nil:
		return level{
			scope: service.CanvasScope{Kind: service.CanvasEnv, ID: sc.Env.ID},
			base:  "/" + sc.Org.Slug + "/" + sc.Stack.Slug + "/" + sc.Env.Slug,
			title: sc.Env.Name,
			empty: "No tiles in this environment yet.",
		}
	case sc.Stack != nil:
		return level{
			scope: service.CanvasScope{Kind: service.CanvasStack, ID: sc.Stack.ID},
			base:  "/" + sc.Org.Slug + "/" + sc.Stack.Slug,
			title: sc.Stack.Name,
			empty: "This stack has no environments yet.",
		}
	case sc.Org != nil:
		return level{
			scope: service.CanvasScope{Kind: service.CanvasOrg, ID: sc.Org.ID},
			base:  "/" + sc.Org.Slug,
			title: sc.Org.Name,
			empty: "This organization has no stacks yet. Create one to get started.",
		}
	}
	p := middleware.Principal(c)
	l := level{
		scope: service.CanvasScope{Kind: service.CanvasHome, ID: p.User.ID},
		title: "Organizations",
		empty: "You are not in an organization yet. Ask an administrator to add you.",
	}
	if p.Access.Admin {
		l.empty = "Welcome to stackr. Create your first organization with the button above."
	}
	return l
}

// show reads the query params: "<name>=0" turns a kind off.
func show(c echo.Context) service.GraphShow {
	on := func(k string) bool { return c.QueryParam(k) != "0" }
	return service.GraphShow{
		System:  on("system"),
		Refs:    on("refs"),
		Startup: on("startup"),
		Traffic: on("traffic"),
	}
}

func (h *handler) view(c echo.Context) (ui.View, error) {
	return h.build(c.Request().Context(), where(c), show(c), c.QueryParam("focus"))
}

// build is the canvas as drawn, the env's traffic lanes at the last
// sample included (so a "graph" swap keeps them).
func (h *handler) build(ctx context.Context, l level, sh service.GraphShow, focus string) (ui.View, error) {
	gv, err := h.orch.Canvas(ctx, l.scope, sh)
	if err != nil {
		return ui.View{}, err
	}
	v := mapView(gv, l, sh, focus)
	if v.Lanes != nil {
		es, err := h.orch.Traffic(ctx, l.scope.ID)
		if err != nil {
			return v, err
		}
		v.Lanes = lanes(es)
	}
	return v, nil
}

func lanes(es []service.Edge) []cards.Lane {
	out := make([]cards.Lane, 0, len(es))
	for _, e := range es {
		out = append(out, cards.Lane{From: e.From, To: e.To, BPS: e.BPS})
	}
	return out
}

// GET /, /:org, /:org/:stack, /:org/:stack/:env. A fresh load of
// ?drawer=<node id>&tab= comes with that card's drawer open.
func (h *handler) Page(c echo.Context) error {
	v, err := h.view(c)
	if err != nil {
		return middleware.HTTPError(err)
	}
	// The top bar holds the create buttons; home has no bar, so its canvas does.
	var actions templ.Component
	if l := where(c); l.scope.Kind == service.CanvasHome {
		v.Create = createButtons(c, l)
	} else if cs := createButtons(c, l); len(cs) > 0 {
		actions = ui.Actions(cs)
	}
	if v.Banner, err = h.planBanner(c); err != nil {
		return middleware.HTTPError(err)
	}
	var drawer templ.Component
	if id := c.QueryParam("drawer"); id != "" && !isHTMX(c) {
		if drawer, err = h.drawer(c, v, id, c.QueryParam("tab")); err != nil {
			return middleware.HTTPError(err)
		}
	}
	return render.PageWith(c, http.StatusOK, where(c).title, ui.Page(v), actions, drawer)
}

func isHTMX(c echo.Context) bool {
	return c.Request().Header.Get("HX-Request") == "true" &&
		c.Request().Header.Get("HX-History-Restore-Request") != "true"
}

// POST …/-/positions (node_id, x, y): a card or note was dropped.
func (h *handler) Positions(c echo.Context) error {
	x, errX := strconv.Atoi(c.FormValue("x"))
	y, errY := strconv.Atoi(c.FormValue("y"))
	if errX != nil || errY != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "x and y are whole numbers")
	}
	err := h.orch.SetPosition(c.Request().Context(), where(c).scope, c.FormValue("node_id"), service.Point{X: x, Y: y})
	if err != nil {
		return middleware.HTTPError(err)
	}
	return c.NoContent(http.StatusNoContent)
}

// POST …/-/reset: forget this canvas's positions, answer the re-arranged
// canvas into #graph.
func (h *handler) Reset(c echo.Context) error {
	if err := h.orch.ResetPositions(c.Request().Context(), where(c).scope); err != nil {
		return middleware.HTTPError(err)
	}
	return h.canvas(c)
}

// POST …/-/notes (id?, kind, text, x, y, w, h): create or save a note or
// box; an emptied note is deleted. Answers the canvas.
func (h *handler) Notes(c echo.Context) error {
	var n [4]int
	for i, k := range []string{"x", "y", "w", "h"} {
		if v := c.FormValue(k); v != "" {
			var err error
			if n[i], err = strconv.Atoi(v); err != nil {
				return echo.NewHTTPError(http.StatusBadRequest, k+" is a whole number")
			}
		}
	}
	a := service.Annotation{
		ID:   c.FormValue("id"),
		Kind: c.FormValue("kind"),
		Text: c.FormValue("text"),
		X:    n[0],
		Y:    n[1],
		W:    n[2],
		H:    n[3],
	}
	if _, _, err := h.orch.SetAnnotation(c.Request().Context(), where(c).scope, a); err != nil {
		return middleware.HTTPError(err)
	}
	return h.canvas(c)
}

// POST …/-/notes/delete (id).
func (h *handler) DeleteNote(c echo.Context) error {
	if err := h.orch.DeleteAnnotation(c.Request().Context(), where(c).scope, c.FormValue("id")); err != nil {
		return middleware.HTTPError(err)
	}
	return h.canvas(c)
}

func (h *handler) canvas(c echo.Context) error {
	v, err := h.view(c)
	if err != nil {
		return middleware.HTTPError(err)
	}
	return respond.HTML(c, http.StatusOK, ui.Canvas(v))
}

// GET …/-/events?n=<sig>: the canvas's stream. "footer:<id>" when a card's
// footer changes (every footer once at connect), "graph" with the whole
// canvas when the set of cards, edges or notes moves away from the one
// the page drew (n); on the env canvas also "traffic", the lanes after
// each sample.
func (h *handler) Events(c echo.Context) error {
	poll := h.Poll(c)
	if l := where(c); l.scope.Kind == service.CanvasEnv && show(c).Traffic {
		poll = both(poll, h.traffic(l.scope.ID))
	}
	return stream.Watch(c, poll)
}

type producer = func(context.Context) ([]stream.Msg, error)

func both(a, b producer) producer {
	return func(ctx context.Context) ([]stream.Msg, error) {
		ma, err := a(ctx)
		if err != nil {
			return nil, err
		}
		mb, err := b(ctx)
		return append(ma, mb...), err
	}
}

// traffic sends the rendered lanes at connect and whenever a new sample
// lands.
func (h *handler) traffic(env string) producer {
	last := int64(-1)
	return func(ctx context.Context) ([]stream.Msg, error) {
		seq := h.orch.TrafficSeq()
		if seq == last {
			return nil, nil
		}
		es, err := h.orch.Traffic(ctx, env)
		if err != nil {
			return nil, err
		}
		last = seq
		body, err := render.Event(ctx, cards.Lanes(lanes(es)))
		return []stream.Msg{{Name: "traffic", Body: body}}, err
	}
}

// Poll is the canvas stream's producer: its cards' footers and the whole
// canvas when its shape moves.
func (h *handler) Poll(c echo.Context) producer {
	l, sh, drawn := where(c), show(c), c.QueryParam("n")
	sent := map[string]stream.HTML{}
	var last time.Time
	return func(ctx context.Context) ([]stream.Msg, error) {
		if time.Since(last) < Every {
			return nil, nil
		}
		last = time.Now()
		v, err := h.build(ctx, l, sh, "")
		if err != nil {
			return nil, err
		}
		if sig := Sig(v); sig != drawn {
			drawn = sig
			clear(sent)
			body, err := render.Event(ctx, ui.Canvas(v))
			return []stream.Msg{{Name: "graph", Body: body}}, err
		}
		var out []stream.Msg
		for _, n := range v.Nodes {
			body, err := render.Event(ctx, n.Foot())
			if err != nil {
				return nil, err
			}
			if sent[n.ID] != body {
				sent[n.ID] = body
				out = append(out, stream.Msg{Name: "footer:" + n.ID, Body: body})
			}
		}
		return out, nil
	}
}

// Sig names what the canvas draws apart from positions and footers: a
// rename or recolour redraws too.
func Sig(v ui.View) string {
	var parts []string
	for _, n := range v.Nodes {
		parts = append(parts, "n "+n.ID+" "+n.Name+" "+n.Detail+" "+n.Color)
	}
	for _, e := range v.Edges {
		parts = append(parts, "e "+e.Kind+" "+e.From+" "+e.To)
	}
	for _, n := range v.Notes {
		parts = append(parts, "a "+n.ID+" "+n.Text)
	}
	sort.Strings(parts)
	f := fnv.New64a()
	_, _ = f.Write([]byte(strings.Join(parts, "\n")))
	return strconv.FormatUint(f.Sum64(), 36)
}

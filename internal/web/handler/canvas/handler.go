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
}

func where(c echo.Context) level {
	sc := middleware.ScopeOf(c)
	switch {
	case sc.Env != nil:
		return level{service.CanvasScope{Kind: service.CanvasEnv, ID: sc.Env.ID},
			"/" + sc.Org.Slug + "/" + sc.Stack.Slug + "/" + sc.Env.Slug, sc.Env.Name}
	case sc.Stack != nil:
		return level{service.CanvasScope{Kind: service.CanvasStack, ID: sc.Stack.ID}, "/" + sc.Org.Slug + "/" + sc.Stack.Slug, sc.Stack.Name}
	case sc.Org != nil:
		return level{service.CanvasScope{Kind: service.CanvasOrg, ID: sc.Org.ID}, "/" + sc.Org.Slug, sc.Org.Name}
	}
	return level{service.CanvasScope{Kind: service.CanvasHome, ID: middleware.Principal(c).User.ID}, "", "Orgs"}
}

// show reads the query params: "<name>=0" turns a kind off.
func show(c echo.Context) service.GraphShow {
	on := func(k string) bool { return c.QueryParam(k) != "0" }
	return service.GraphShow{System: on("system"), Refs: on("refs"), Startup: on("startup"), Traffic: on("traffic")}
}

func (h *handler) view(c echo.Context) (ui.View, error) {
	l, sh := where(c), show(c)
	gv, err := h.orch.Canvas(c.Request().Context(), l.scope, sh)
	if err != nil {
		return ui.View{}, err
	}
	return mapView(gv, l, sh, c.QueryParam("focus")), nil
}

// GET /, /:org, /:org/:stack, /:org/:stack/:env. A fresh load of
// ?drawer=<node id>&tab= comes with that card's drawer open.
func (h *handler) Page(c echo.Context) error {
	v, err := h.view(c)
	if err != nil {
		return middleware.HTTPError(err)
	}
	v.Create = createButtons(c, where(c))
	var drawer templ.Component
	if id := c.QueryParam("drawer"); id != "" && !isHTMX(c) {
		if drawer, err = h.drawer(c, v, id, c.QueryParam("tab")); err != nil {
			return middleware.HTTPError(err)
		}
	}
	return render.PageWith(c, http.StatusOK, where(c).title, ui.Page(v), drawer)
}

func isHTMX(c echo.Context) bool {
	return c.Request().Header.Get("HX-Request") == "true" && c.Request().Header.Get("HX-History-Restore-Request") != "true"
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
	a := service.Annotation{ID: c.FormValue("id"), Kind: c.FormValue("kind"), Text: c.FormValue("text"), X: n[0], Y: n[1], W: n[2], H: n[3]}
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
// the page drew (n).
func (h *handler) Events(c echo.Context) error {
	return stream.Watch(c, h.Poll(c))
}

// Poll is the canvas stream's producer, for a stream that carries more
// (the env stream folds it in beside its traffic lanes).
func (h *handler) Poll(c echo.Context) func(context.Context) ([]stream.Msg, error) {
	l, sh, drawn := where(c), show(c), c.QueryParam("n")
	sent := map[string]stream.HTML{}
	var last time.Time
	return func(ctx context.Context) ([]stream.Msg, error) {
		if time.Since(last) < Every {
			return nil, nil
		}
		last = time.Now()
		gv, err := h.orch.Canvas(ctx, l.scope, sh)
		if err != nil {
			return nil, err
		}
		v := mapView(gv, l, sh, "")
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

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
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/api/stream"
	"github.com/FyrmForge/stackr/internal/authz"
	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	comp "github.com/FyrmForge/stackr/internal/ui/components"
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
	stack string // the stack's id on an env canvas, for its elevated access
	base  string // "" on home
	title string
	empty string // v0's line for a canvas with nothing on it yet
	plans bool   // org canvas: the viewer may read the org's config plans (its window lists them)
	// viewOnly: the viewer cannot promote here, so the window's links read
	// "View plan".
	viewOnly bool
}

func where(c echo.Context) level {
	sc := middleware.ScopeOf(c)
	switch {
	case sc.Env != nil:
		return level{
			scope: service.CanvasScope{Kind: service.CanvasEnv, ID: sc.Env.ID},
			stack: sc.Stack.ID,
			base:  "/" + sc.Org.Slug + "/" + sc.Stack.Slug + "/" + sc.Env.Slug,
			title: sc.Env.Name,
			empty: "No tiles in this environment yet.",

			viewOnly: !can(c, sc, "env.write"),
		}
	case sc.Stack != nil:
		return level{
			scope: service.CanvasScope{Kind: service.CanvasStack, ID: sc.Stack.ID},
			base:  "/" + sc.Org.Slug + "/" + sc.Stack.Slug,
			title: sc.Stack.Name,
			empty: "This stack has no environments yet.",

			viewOnly: !can(c, sc, "env.write"),
		}
	case sc.Org != nil:
		return level{
			scope: service.CanvasScope{Kind: service.CanvasOrg, ID: sc.Org.ID},
			base:  "/" + sc.Org.Slug,
			title: sc.Org.Name,
			empty: "This organization has no stacks yet. Create one to get started.",
			plans: can(c, sc, "org.owner.read"),

			viewOnly: !can(c, sc, "env.write"),
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

// show reads the query params: "<name>=0" turns a kind off; operated is
// off unless "operated=1".
func show(c echo.Context) service.GraphShow {
	on := func(k string) bool { return c.QueryParam(k) != "0" }
	return service.GraphShow{
		System:   on("system"),
		Refs:     on("refs"),
		Startup:  on("startup"),
		Traffic:  on("traffic"),
		Operated: c.QueryParam("operated") == "1",
	}
}

func (h *handler) view(c echo.Context) (ui.View, error) {
	q := syncOf(c)
	if c.QueryParam("job") != "" { // a deploy's page: sync= names the drawer's source, no review
		q = syncQ{}
	}
	v, err := h.build(c.Request().Context(), where(c), show(c), c.QueryParam("focus"), q)
	if v.Releases != nil {
		v.Releases.Open = c.QueryParam("rel") == "1"
		if v.Releases.Switch && c.QueryParam("relview") == "org" {
			v.Releases.View = "org"
		}
	}
	if v.SyncBar != nil && !can(c, middleware.ScopeOf(c), "env.write") { // the review is for anyone; deploying it is not
		v.SyncBar.Deploy = nil
	}
	return v, err
}

// build is the canvas as drawn, the env's traffic lanes at the last
// sample included (so a "graph" swap keeps them). With a sync review (env
// canvas only) it is the canvas with the sync laid over it and no stream:
// the overlay is recomputed on each navigation instead.
func (h *handler) build(ctx context.Context, l level, sh service.GraphShow, focus string, q syncQ) (ui.View, error) {
	var gv service.GraphView
	var pl service.EnvSyncPlan
	var err error
	review := q.from != "" && l.scope.Kind == service.CanvasEnv
	if review {
		gv, pl, err = h.orch.EnvSyncCanvas(ctx, l.scope.ID, q.from, q.drop, sh)
	} else {
		gv, err = h.orch.Canvas(ctx, l.scope, sh)
	}
	if err != nil {
		return ui.View{}, err
	}
	var g service.HostGrant
	if l.stack != "" {
		if g, err = h.orch.HostGrant(ctx, l.stack); err != nil {
			return ui.View{}, err
		}
	}
	v := mapView(gv, l, sh, focus, g)
	v.Releases = h.relWindow(ctx, l, gv)
	if review {
		v.Events, v.SyncBar, v.Review = "", syncBar(l, pl, q), q.query()
	}
	if v.Empty != "" && l.scope.Kind == service.CanvasEnv {
		if rel, ok := h.orch.WaitingRelease(ctx, l.stack, l.scope.ID); ok {
			v.Empty = "Release " + strconv.Itoa(rel.Number) + " is waiting."
			v.EmptyNote = "Nothing runs here until it is promoted."
			v.EmptyHref = l.base + "?drawer=" + url.QueryEscape("env:"+l.scope.ID) + "&tab=releases&plan=" + rel.ID
			if l.viewOnly {
				v.EmptyLink = "View plan"
			}
		}
	}
	if v.Empty != "" && l.scope.Kind == service.CanvasStack {
		h.pushWhy(ctx, &v, l)
	}
	if v.Lanes != nil {
		es, err := h.orch.Traffic(ctx, l.scope.ID)
		if err != nil {
			return v, err
		}
		v.Lanes = lanes(es)
	}
	return v, nil
}

// pushWhy says on an empty stack page what the last push did: building,
// failed with its error, or done with why it made no env. The stack file's
// push is the only thing that makes the first env.
func (h *handler) pushWhy(ctx context.Context, v *ui.View, l level) {
	p, ok := h.orch.PushState(ctx, l.scope.ID)
	if !ok {
		return
	}
	v.EmptyLink, v.EmptyHref = "Job log", path.Dir(l.base)+"/-/jobs/"+p.JobID // a stack's job log is the org's
	switch {
	case p.Failed:
		short, _ := comp.ErrTail(p.Error, 80)
		v.Empty = "The last push failed: " + strings.TrimRight(short, ".") + "."
	case p.Done && len(p.Skipped) > 0:
		v.EmptyNote = strings.Join(p.Skipped, "; ")
	case p.Done:
		v.EmptyLink, v.EmptyHref = "", ""
	case p.Waits != "":
		v.Empty = p.Waits + "."
	default:
		v.Empty, v.EmptyAt = "Building "+l.title+comp.AtSha(p.Sha)+", started ", p.Started
		if p.Queued {
			v.Empty = "Waiting to build " + l.title + comp.AtSha(p.Sha) + ", queued "
		}
	}
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
	if to, err := h.carryOn(c); err != nil || to != "" {
		if err != nil {
			return middleware.HTTPError(err)
		}
		return respond.Redirect(c, to)
	}
	v, err := h.view(c)
	if err != nil {
		return middleware.HTTPError(err)
	}
	// The top bar holds the create buttons.
	var actions templ.Component
	home := where(c).scope.Kind == service.CanvasHome
	if l := where(c); home {
		if cs := createButtons(c, l); len(cs) > 0 {
			actions = ui.Actions(cs)
		}
	} else {
		cs := createButtons(c, l)
		sync, err := h.syncButton(c, l)
		if err != nil {
			return middleware.HTTPError(err)
		}
		if cs = append(cs, sync...); len(cs) > 0 {
			actions = ui.Actions(cs)
		}
	}
	if where(c).scope.Kind == service.CanvasEnv {
		v.DNS = v.Base + "/-/dns-banner"
	}
	var drawer templ.Component
	if id := c.QueryParam("drawer"); id != "" && (!isHTMX(c) || reviewOpen(c, syncOf(c))) {
		if drawer, err = h.drawer(c, v, id, c.QueryParam("tab")); err != nil {
			return middleware.HTTPError(err)
		}
		if shown, ok := c.Get(shownTabKey).(string); ok { // the address names the tab it shows
			q := c.QueryParams()
			q.Set("tab", shown)
			q.Del("open")
			return c.Redirect(http.StatusSeeOther, c.Request().URL.Path+"?"+q.Encode())
		}
	}
	body := ui.Page(v)
	if drawer != nil && isHTMX(c) { // Drop and Undo move the canvas; the open drawer follows
		body = templ.Join(body, ui.DrawerOOB(drawer))
	}
	if home {
		return render.PageBar(c, http.StatusOK, "Organizations", "", body, actions, drawer)
	}
	return render.PageWith(c, http.StatusOK, where(c).title, body, actions, drawer)
}

// carryOn is v0's home rule: someone whose only org is the one they are
// setting up is here to carry on, not to look at a canvas of one card.
func (h *handler) carryOn(c echo.Context) (string, error) {
	p := middleware.Principal(c)
	if where(c).scope.Kind != service.CanvasHome || p == nil {
		return "", nil
	}
	orgs, err := h.orch.Orgs(c.Request().Context(), p.User.ID)
	if err != nil || len(orgs) != 1 || orgs[0].SetupDoneAt != nil {
		return "", err
	}
	if authz.Can(p.Access, "org.setup.step", authz.Resource{OrgID: orgs[0].ID}) != nil {
		return "", nil
	}
	return "/" + orgs[0].Slug + "/-/setup/done", nil
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
	var keep comp.RelKeeper // a failed read of the window keeps its last counts
	return func(ctx context.Context) ([]stream.Msg, error) {
		if time.Since(last) < Every {
			return nil, nil
		}
		last = time.Now()
		v, err := h.build(ctx, l, sh, "", syncQ{})
		if err != nil {
			return nil, err
		}
		v.Releases = keep.Hold(v.Releases, last)
		if sig := Sig(v); sig != drawn {
			drawn = sig
			clear(sent)
			body, err := render.Event(ctx, ui.Canvas(v))
			return []stream.Msg{{Name: "graph", Body: body}}, err
		}
		var out []stream.Msg
		if v.Releases != nil {
			body, err := render.Event(ctx, comp.RelUpdate(*v.Releases))
			if err != nil {
				return nil, err
			}
			if sent["releases"] != body {
				sent["releases"] = body
				out = append(out, stream.Msg{Name: "releases", Body: body})
			}
		}
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
		parts = append(parts, "n "+n.ID+" "+n.Name+" "+n.Detail+" "+n.Color+" "+strconv.FormatBool(n.Pending)+" "+n.FailHref+" "+n.NoteHref)
	}
	parts = append(parts, "empty "+v.Empty+" "+v.EmptyHref+" "+v.EmptyNote+" "+v.EmptyLink+" "+v.EmptyAt.String())
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

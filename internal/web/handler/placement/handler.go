// Package placement serves the Placement tab of an org, stack, env and tile
// (Runs on, Ingress, Exits), the env's move dialog and its progress. Every
// answer is the tab's fragment (#placement); the drawers load it.
package placement

import (
	"errors"
	"net/http"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/authz"
	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	ui "github.com/FyrmForge/stackr/internal/ui/placement"
)

type handler struct{ orch *service.Orchestrator }

func NewHandler(orch *service.Orchestrator) *handler { return &handler{orch: orch} }

// level is one scope's paths and the verbs that guard them.
type level struct {
	path        string
	read, write authz.Verb
}

var levels = []level{
	{"/:org/-/placement", "org.read", "org.write"},
	{"/:org/:stack/-/placement", "org.read", "stack.write"},
	{"/:org/:stack/:env/-/placement", "org.read", "env.write"},
	{"/:org/:stack/:env/-/tiles/:tile/placement", "tile.read", "tile.write"},
}

// Mount registers the tab on g. The env's move routes sit under its own path.
func (h *handler) Mount(g *echo.Group, a *middleware.Access) {
	for _, l := range levels {
		read, write := a.Require(l.read), a.Require(l.write)
		g.GET(l.path, h.Get, read)
		g.POST(l.path+"/pin", h.act("Saved.", h.pin), write)
		g.POST(l.path+"/ingress", h.act("Saved.", h.ingress), write)
		g.POST(l.path+"/exits", h.act("Saved.", h.reorder), write)
		g.POST(l.path+"/exits/add", h.act("Saved.", h.add), write)
		g.POST(l.path+"/exits/remove", h.act("Saved.", h.remove), write)
		g.POST(l.path+"/move-all", h.act("Moving: one job per env.", h.moveAll), write)
	}
	e := levels[2].path
	g.GET(e+"/move", h.MoveDialog, a.Require("env.write"))
	g.POST(e+"/move", h.Move, a.Require("env.write"))
	g.GET(e+"/move/status", h.Status, a.Require("org.read"))
}

// kind and id of the scope the middleware resolved.
func scopeOf(c echo.Context) (string, string) {
	s := middleware.ScopeOf(c)
	switch {
	case s.Tile != nil:
		return service.ScopeTile, s.Tile.ID
	case s.Env != nil:
		return service.ScopeEnv, s.Env.ID
	case s.Stack != nil:
		return service.ScopeStack, s.Stack.ID
	}
	return service.ScopeOrg, s.Org.ID
}

func envURL(s service.Scope) string {
	return "/" + s.Org.Slug + "/" + s.Stack.Slug
}

// base is the route all of this tab's actions hang off.
func base(c echo.Context) string {
	s := middleware.ScopeOf(c)
	u := "/" + s.Org.Slug
	if s.Stack != nil {
		u += "/" + s.Stack.Slug
	}
	if s.Env != nil {
		u += "/" + s.Env.Slug
	}
	if s.Tile != nil {
		return u + "/-/tiles/" + s.Tile.Slug + "/placement"
	}
	return u + "/-/placement"
}

func can(c echo.Context, v authz.Verb) bool {
	p := middleware.Principal(c)
	return p != nil && authz.Can(p.Access, v, authz.Resource{OrgID: middleware.ScopeOf(c).Org.ID}) == nil
}

func admin(c echo.Context) bool {
	p := middleware.Principal(c)
	return p != nil && p.Access.Admin
}

func writeVerb(kind string) authz.Verb {
	return map[string]authz.Verb{
		service.ScopeOrg: levels[0].write, service.ScopeStack: levels[1].write,
		service.ScopeEnv: levels[2].write, service.ScopeTile: levels[3].write,
	}[kind]
}

// refused is the message of an error the viewer can act on.
func refused(err error) (string, bool) {
	var he *echo.HTTPError
	if errors.As(middleware.HTTPError(err), &he) && he.Code < 500 && he.Code != http.StatusNotFound {
		if m, ok := he.Message.(string); ok {
			return m, true
		}
	}
	return "", false
}

func options(ns []service.PlacementNode, keep func(service.PlacementNode) bool) []ui.Option {
	var out []ui.Option
	for _, n := range ns {
		if keep == nil || keep(n) {
			out = append(out, ui.Option{ID: n.ID, Name: n.Name})
		}
	}
	return out
}

// build is the tab for the resolved scope.
func (h *handler) build(c echo.Context) (ui.View, error) {
	ctx, s := c.Request().Context(), middleware.ScopeOf(c)
	kind, id := scopeOf(c)
	pv, err := h.orch.Placement(ctx, kind, id)
	if err != nil {
		return ui.View{}, err
	}
	v := ui.View{
		Scope:       kind,
		RunsOn:      kind != service.ScopeTile,
		Nodes:       options(pv.Nodes, nil),
		Pin:         pv.Pin,
		PinFrom:     pv.PinFrom,
		PinName:     pv.PinName,
		StayCount:   pv.Stay.Count,
		StayWhere:   pv.Stay.Where,
		Ingress:     pv.Ingress,
		IngressFrom: pv.IngressFrom,
		IngressOpts: options(pv.Nodes, func(n service.PlacementNode) bool { return n.Ingress }),
		Inherited:   pv.Inherited,
		ExitsFrom:   pv.ExitsFrom,
		Spare:       options(pv.Spare, nil),
		OwnListed:   pv.OwnListed,
		NoNet:       pv.NoNet,
		NoNetWhy:    pv.NoNetWhy,
	}
	if can(c, writeVerb(kind)) {
		v.Base = base(c)
	}
	v.MoveAll = admin(c) && v.Base != ""
	for _, r := range pv.Rungs {
		v.Rungs = append(v.Rungs, ui.Rung{NodeID: r.NodeID, Name: r.Name, State: r.State, Detail: r.Detail, Own: r.Own, Up: r.Up, Down: r.Down})
	}
	if kind == service.ScopeEnv {
		v.CanMove = admin(c) && v.Base != ""
		ms, err := h.orch.EnvMoveStatus(ctx, s.Env.ID)
		if err != nil {
			return v, err
		}
		if ms != nil && ms.Active {
			v.Moving = envURL(s) + "?drawer=env:" + s.Env.ID + "&tab=moving"
		}
	}
	return v, nil
}

func (h *handler) show(c echo.Context, status int, note, errMsg string) error {
	v, err := h.build(c)
	if err != nil {
		return middleware.HTTPError(err)
	}
	v.Note, v.Error = note, errMsg
	return respond.HTML(c, status, ui.Placement(v))
}

// Get is the tab.
func (h *handler) Get(c echo.Context) error { return h.show(c, http.StatusOK, "", "") }

// act answers an action with the tab: a refusal shows over it (422).
func (h *handler) act(note string, do func(echo.Context, string, string) error) echo.HandlerFunc {
	return func(c echo.Context) error {
		kind, id := scopeOf(c)
		if err := do(c, kind, id); err != nil {
			msg, ok := refused(err)
			if !ok {
				return middleware.HTTPError(err)
			}
			return h.show(c, http.StatusUnprocessableEntity, "", msg)
		}
		return h.show(c, http.StatusOK, note, "")
	}
}

func (h *handler) pin(c echo.Context, kind, id string) error {
	return h.orch.SetPin(c.Request().Context(), middleware.Principal(c), kind, id, c.FormValue("node"))
}

func (h *handler) ingress(c echo.Context, kind, id string) error {
	return h.orch.SetIngress(c.Request().Context(), middleware.Principal(c), kind, id, c.FormValue("node"))
}

func (h *handler) reorder(c echo.Context, kind, id string) error {
	form, _ := c.FormParams()
	return h.orch.SetExits(c.Request().Context(), middleware.Principal(c), kind, id, form["ids"])
}

func (h *handler) add(c echo.Context, kind, id string) error {
	return h.orch.ExitsAdd(c.Request().Context(), middleware.Principal(c), kind, id, c.FormValue("node"))
}

func (h *handler) remove(c echo.Context, kind, id string) error {
	return h.orch.ExitsRemove(c.Request().Context(), middleware.Principal(c), kind, id, c.FormValue("node"))
}

func (h *handler) moveAll(c echo.Context, kind, id string) error {
	_, err := h.orch.MoveEnvsToPin(c.Request().Context(), middleware.Principal(c), kind, id)
	return err
}

// MoveDialog is GET …/move?to=<node>: the dialog for moving this env there.
// Empty when the pick is the node it is on.
func (h *handler) MoveDialog(c echo.Context) error {
	s := middleware.ScopeOf(c)
	if s.Env.NodeID == c.QueryParam("to") {
		return respond.HTML(c, http.StatusOK, templ.NopComponent)
	}
	p, err := h.orch.PlanMove(c.Request().Context(), s.Env.ID, c.QueryParam("to"))
	if err != nil {
		if msg, ok := refused(err); ok {
			return h.show(c, http.StatusUnprocessableEntity, "", msg)
		}
		return middleware.HTTPError(err)
	}
	v := ui.MoveView{
		Base: base(c),
		Env:  p.EnvName, From: p.From, To: p.To, ToID: c.QueryParam("to"),
		Targets: options(p.Targets, nil), Volumes: p.Volumes,
	}
	if p.Bytes >= 0 {
		v.Data = service.HumanBytes(p.Bytes)
	}
	return respond.HTML(c, http.StatusOK, ui.Move(v))
}

// Move is POST …/move: queue it and open the progress tab.
func (h *handler) Move(c echo.Context) error {
	s := middleware.ScopeOf(c)
	_, err := h.orch.MoveEnv(c.Request().Context(), middleware.Principal(c), s.Env.ID, c.FormValue("to"), c.FormValue("how") != "cold")
	if err != nil {
		msg, ok := refused(err)
		if !ok {
			return middleware.HTTPError(err)
		}
		return h.show(c, http.StatusUnprocessableEntity, "", msg)
	}
	c.Response().Header().Set("HX-Redirect", envURL(s)+"?drawer=env:"+s.Env.ID+"&tab=moving")
	return c.NoContent(http.StatusOK)
}

// Status is the progress tab; it polls itself while the move runs.
func (h *handler) Status(c echo.Context) error {
	s := middleware.ScopeOf(c)
	ms, err := h.orch.EnvMoveStatus(c.Request().Context(), s.Env.ID)
	if err != nil {
		return middleware.HTTPError(err)
	}
	if ms == nil {
		return middleware.HTTPError(errs.ErrNotFound)
	}
	v := ui.ProgressView{
		Env: s.Env.Name, From: ms.From, To: ms.To,
		Started: ms.Move.StartedAt.Local().Format("15:04"),
		State:   "running", Error: ms.Move.Error,
	}
	switch ms.Move.Phase {
	case "done", "rolled_back":
		v.State = ms.Move.Phase
	default:
		v.Refresh = base(c) + "/move/status"
	}
	for _, st := range ms.Steps {
		v.Steps = append(v.Steps, ui.StepView{Label: st.Label, State: st.State, Detail: st.Detail})
	}
	return respond.HTML(c, http.StatusOK, ui.Progress(v))
}

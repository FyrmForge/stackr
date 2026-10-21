// Package placement serves the Placement tab of an org, stack, env and tile
// (Runs on, Ingress, Exits), the env's pin change dialog and its progress.
// Every answer is the tab's fragment (#placement); the drawers load it.
// Placements are named, nodes are not: a member never sees a node name.
package placement

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

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

// level is one scope's paths and the verb that guards its reads. Every
// write is placement.write (owner), as on the API: a member may deploy, but
// moving an env or rerouting its traffic is the owner's call.
type level struct {
	path string
	read authz.Verb
}

const write authz.Verb = "placement.write"

var levels = []level{
	{"/:org/-/placement", "org.read"},
	{"/:org/:stack/-/placement", "org.read"},
	{"/:org/:stack/:env/-/placement", "org.read"},
	{"/:org/:stack/:env/-/tiles/:tile/placement", "tile.read"},
}

// Mount registers the tab on g. The env's move routes sit under its own path.
// A pin change that moves the env answers with the dialog; Move confirms it.
func (h *handler) Mount(g *echo.Group, a *middleware.Access) {
	for _, l := range levels {
		read, write := a.Require(l.read), a.Require(write)
		g.GET(l.path, h.Get, read)
		g.POST(l.path+"/pin", h.act("Saved.", h.pin), write)
		g.POST(l.path+"/ingress", h.act("Saved.", h.ingress), write)
		g.POST(l.path+"/exits", h.act("Saved.", h.reorder), write)
		g.POST(l.path+"/exits/add", h.act("Saved.", h.add), write)
		g.POST(l.path+"/exits/remove", h.act("Saved.", h.remove), write)
	}
	e := levels[2].path
	g.POST(e+"/move", h.Move, a.Require(write))
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

// hint is a placement's fit, shown after its name in a select.
func hint(o service.PlacementOffer) string {
	var parts []string
	for _, t := range o.Tags {
		if t != "" {
			parts = append(parts, t)
		}
	}
	if o.Why != "" {
		parts = append(parts, o.Why)
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

// offers is the org's placement list, by name and in order.
func (h *handler) offers(c echo.Context) (map[string]service.PlacementOffer, []service.PlacementOffer, error) {
	list, err := h.orch.OrgPlacements(c.Request().Context(), middleware.ScopeOf(c).Org.ID)
	if err != nil {
		return nil, nil, err
	}
	by := map[string]service.PlacementOffer{}
	for _, o := range list {
		by[o.Name] = o
	}
	return by, list, nil
}

func options(ns []service.PlacementNode, by map[string]service.PlacementOffer, keep func(service.PlacementNode) bool) []ui.Option {
	var out []ui.Option
	for _, n := range ns {
		if keep == nil || keep(n) {
			out = append(out, ui.Option{ID: n.ID, Name: n.Name, Hint: hint(by[n.Name])})
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
	by, list, err := h.offers(c)
	if err != nil {
		return ui.View{}, err
	}
	v := ui.View{
		Scope:       kind,
		RunsOn:      kind != service.ScopeTile,
		Nodes:       options(pv.Nodes, by, nil),
		Pin:         pv.Pin,
		PinFrom:     pv.PinFrom,
		Ingress:     pv.Ingress,
		IngressFrom: pv.IngressFrom,
		IngressOpts: options(pv.Nodes, by, func(n service.PlacementNode) bool { return n.Ingress }),
		Inherited:   pv.Inherited,
		ExitsFrom:   pv.ExitsFrom,
		Spare:       options(pv.Spare, by, nil),
		OwnListed:   pv.OwnListed,
		NoNet:       pv.NoNet,
		NoNetWhy:    pv.NoNetWhy,
	}
	if can(c, write) {
		v.Base = base(c)
	}
	if kind == service.ScopeOrg {
		for _, o := range list {
			v.Offers = append(v.Offers, ui.Offer{Name: o.Name, Tags: strings.Join(o.Tags, ", "), Public: o.Public, Online: o.Online, Usable: o.Usable, Why: o.Why})
		}
	}
	for _, r := range pv.Rungs {
		v.Rungs = append(v.Rungs, ui.Rung{NodeID: r.NodeID, Name: r.Name, State: r.State, Detail: r.Detail, Own: r.Own, Up: r.Up, Down: r.Down})
	}
	if kind == service.ScopeEnv {
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
	return h.showWith(c, status, note, errMsg, nil)
}

// showWith is show with the pin change dialog over the tab.
func (h *handler) showWith(c echo.Context, status int, note, errMsg string, dialog *ui.MoveView) error {
	v, err := h.build(c)
	if err != nil {
		return middleware.HTTPError(err)
	}
	v.Note, v.Error = note, errMsg
	if dialog != nil {
		v.Move = ui.Move(*dialog)
	}
	return respond.HTML(c, status, ui.Placement(v))
}

// showIngressAsk is the tab with the ingress question open.
func (h *handler) showIngressAsk(c echo.Context, d templ.Component) error {
	v, err := h.build(c)
	if err != nil {
		return middleware.HTTPError(err)
	}
	v.IngressAsk = d
	return respond.HTML(c, http.StatusOK, ui.Placement(v))
}

// moveView is the plans of a pin change as the dialog shows them, one per
// env it moves. Confirm posts the target placement and word, the scope's
// slug, to action; the service checks it.
func moveView(action, word string, ps []service.PlacementMovePlan) ui.MoveView {
	v := ui.MoveView{Action: action, Env: word, OK: true}
	for i, p := range ps {
		if i == 0 {
			v.From, v.To = p.From, p.To
		} else if p.From != v.From {
			v.From = "their placements"
		}
		v.OK = v.OK && p.OK
		v.Restore = v.Restore || p.Restore
		for _, e := range p.Envs {
			d := count(e.Volumes, "volume")
			if e.Bytes >= 0 {
				d += ", " + service.HumanBytes(e.Bytes)
			}
			v.Envs = append(v.Envs, ui.MoveLine{Path: e.Path, Detail: d})
			if v.Downtime == "" {
				v.Downtime = strings.TrimPrefix(e.Downtime, "about ")
			}
		}
		for _, k := range p.Checks {
			name := k.Name
			if len(ps) > 1 {
				name = p.Placement + ": " + name
			}
			v.Checks = append(v.Checks, ui.CheckLine{Name: name, Detail: k.Detail, OK: k.OK})
		}
	}
	vals, _ := json.Marshal(map[string]string{"placement": v.To, "confirm": word})
	v.Vals = string(vals)
	return v
}

func count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

// Get is the tab.
func (h *handler) Get(c echo.Context) error { return h.show(c, http.StatusOK, "", "") }

// act answers an action with the tab: a refusal shows over it (422).
func (h *handler) act(note string, do func(echo.Context, string, string) error) echo.HandlerFunc {
	return func(c echo.Context) error {
		kind, id := scopeOf(c)
		if err := do(c, kind, id); err != nil {
			var mv *service.MoveNeeded
			if errors.As(err, &mv) {
				return h.moved(c, kind, mv)
			}
			var hm *service.IngressHostsMove
			if errors.As(err, &hm) {
				vals, _ := json.Marshal(map[string]string{"placement": c.FormValue("placement"), "confirm": "yes"})
				d := ui.IngressHosts(ui.IngressHostsView{Action: base(c) + "/ingress", To: hm.To, Vals: string(vals), Hosts: hm.Hosts})
				return h.showIngressAsk(c, d)
			}
			msg, ok := refused(err)
			if !ok {
				return middleware.HTTPError(err)
			}
			return h.show(c, http.StatusUnprocessableEntity, "", msg)
		}
		return h.show(c, http.StatusOK, note, "")
	}
}

// pin sets the scope's pin; with confirm (the scope's slug, from the move
// dialog) a pin that moves envs starts their moves.
func (h *handler) pin(c echo.Context, kind, id string) error {
	_, err := h.orch.PinScope(c.Request().Context(), middleware.Principal(c), kind, id, c.FormValue("placement"), c.FormValue("confirm"))
	return err
}

// ingress saves the scope's ingress pick; one that moves hosts to another
// proxy asks first (*service.IngressHostsMove), confirm=yes answers it.
func (h *handler) ingress(c echo.Context, kind, id string) error {
	return h.orch.PickIngress(c.Request().Context(), middleware.Principal(c), kind, id, c.FormValue("placement"), c.FormValue("confirm") == "yes")
}

func (h *handler) reorder(c echo.Context, kind, id string) error {
	form, _ := c.FormParams()
	return h.orch.SetExits(c.Request().Context(), middleware.Principal(c), kind, id, form["ids"])
}

func (h *handler) add(c echo.Context, kind, id string) error {
	return h.orch.ExitsAdd(c.Request().Context(), middleware.Principal(c), kind, id, c.FormValue("placement"))
}

func (h *handler) remove(c echo.Context, kind, id string) error {
	return h.orch.ExitsRemove(c.Request().Context(), middleware.Principal(c), kind, id, c.FormValue("placement"))
}

// moved answers a pin change that moves envs with the plan of each and,
// when every check passes, the confirm. An env's confirm goes to its move
// route; an org's or stack's back to its pin route.
func (h *handler) moved(c echo.Context, kind string, mv *service.MoveNeeded) error {
	s := middleware.ScopeOf(c)
	action, word := base(c)+"/pin", s.Org.Slug
	switch kind {
	case service.ScopeEnv:
		action, word = base(c)+"/move", s.Env.Slug
	case service.ScopeStack:
		word = s.Stack.Slug
	}
	v := moveView(action, word, mv.Plans)
	return h.showWith(c, http.StatusOK, "", "", &v)
}

// Move is POST …/move (placement, confirm): queue the env's move and open
// the progress tab.
func (h *handler) Move(c echo.Context) error {
	s := middleware.ScopeOf(c)
	_, err := h.orch.PinScope(c.Request().Context(), middleware.Principal(c), service.ScopeEnv, s.Env.ID, c.FormValue("placement"), c.FormValue("confirm"))
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
		Env:     s.Env.Name,
		Started: ms.Move.StartedAt,
		State:   "running", Error: ms.Move.Error,
	}
	if admin(c) { // node names are an admin's
		v.From, v.To = ms.From, ms.To
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

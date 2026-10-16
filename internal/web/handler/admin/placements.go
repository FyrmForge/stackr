package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	comp "github.com/FyrmForge/stackr/internal/ui/components"
	ui "github.com/FyrmForge/stackr/internal/ui/drawer/admin"
	"github.com/FyrmForge/stackr/internal/web/render"
)

const plBase = ui.Base + "/placements"

// plOpen is what a placements answer opens above the list.
type plOpen struct {
	wizard   *ui.MoveWizardView
	progress *ui.MoveProgressView
}

// mountPlacements is the Placements tab's actions and the move wizard. Every
// verb is node.manage, so the route's Require is the whole gate.
func (h *handler) mountPlacements(g *echo.Group, a *middleware.Access) {
	m := a.Require("node.manage")
	g.GET(plBase, func(c echo.Context) error { return h.plAnswer(c, "", plOpen{}, nil) }, m)
	g.POST(plBase, h.createPlacement, m)
	g.POST(plBase+"/:placement/open", h.openPlacement, m)
	g.POST(plBase+"/:placement/delete", h.deletePlacement, m)
	g.GET(plBase+"/:placement/move", h.moveWizard, m)
	g.POST(plBase+"/:placement/move", h.startMove, m)
	g.GET(plBase+"/:placement/progress", h.moveProgress, m)
	g.POST(plBase+"/:placement/retry", h.retryMove, m)
}

// plAnswer answers the Placements tab: a refusal over it (422), or the note.
func (h *handler) plAnswer(c echo.Context, note string, x plOpen, actErr error) error {
	f, status := frame("placements"), http.StatusOK
	if actErr != nil {
		msg, ok := render.Refused(actErr)
		if !ok {
			return middleware.HTTPError(actErr)
		}
		f.Error, status = msg, http.StatusUnprocessableEntity
	} else {
		f.Note = note
	}
	body, err := h.placementsTab(c, x)
	if err != nil {
		return middleware.HTTPError(err)
	}
	return respond.HTML(c, status, comp.Drawer(f, body))
}

func (h *handler) placementsTab(c echo.Context, x plOpen) (templ.Component, error) {
	ctx := c.Request().Context()
	ps, err := h.orch.Placements(ctx)
	if err != nil {
		return nil, err
	}
	ogs, err := h.orch.AllOrgs(ctx)
	if err != nil {
		return nil, err
	}
	ns, err := h.orch.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	v := ui.PlacementsView{Wizard: x.wizard, Progress: x.progress, Nodes: ui.Base + "?tab=nodes"}
	for _, p := range ps {
		v.Rows = append(v.Rows, placementRow(p, ogs))
	}
	v.Add = ui.PlacementFormView{Action: plBase, All: true, Orgs: orgPicks(ogs, nil)}
	for _, n := range ns {
		v.Add.Nodes = append(v.Add.Nodes, ui.Option{Value: n.Name, Label: n.Name})
	}
	return ui.Placements(v), nil
}

func orgPicks(ogs []service.Org, on []string) []ui.OrgPick {
	out := make([]ui.OrgPick, 0, len(ogs))
	for _, o := range ogs {
		out = append(out, ui.OrgPick{Slug: o.Slug, Name: o.Name, On: slices.Contains(on, o.Slug)})
	}
	return out
}

// placementRow is one placement as the tab draws it.
func placementRow(p service.Placement, ogs []service.Org) ui.PlacementRow {
	r := ui.PlacementRow{
		Name: p.Name, Node: p.NodeName, Tags: p.Tags, Envs: p.Envs, Default: p.Default,
		Open: "all orgs", Move: plBase + "/" + p.Name + "/move",
	}
	r.State, r.Tone = placementState(p)
	if p.Moving != nil {
		r.Progress = plBase + "/" + p.Name + "/progress"
	}
	if !p.OpenToAll {
		r.Open = fmt.Sprint(len(p.Orgs), " orgs")
		if len(p.Orgs) == 1 {
			r.Open = "1 org"
		}
		if len(p.Orgs) == 0 {
			r.Open = "no org"
		}
	}
	r.Edit = ui.PlacementFormView{Action: plBase + "/" + p.Name + "/open", All: p.OpenToAll, Orgs: orgPicks(ogs, p.Orgs)}
	if !p.Default {
		r.Delete = comp.ConfirmView{
			Button:  "Delete",
			Title:   "Delete " + p.Name + "?",
			Warning: "Refused while an org, stack or env still uses it.",
			Action:  plBase + "/" + p.Name + "/delete",
			Target:  target(),
			Quiet:   true,
		}
	}
	return r
}

func target() string { return "#" + comp.DrawerRoot }

// placementState is the one word under a placement's name and its chip tone.
func placementState(p service.Placement) (word, tone string) {
	switch {
	case p.Moving != nil && p.Moving.State == "running":
		return fmt.Sprintf("moving %d of %d", p.Moving.Done, p.Moving.All), "warn"
	case p.Moving != nil && p.Moving.State == "failed":
		return "move failed", "danger"
	case p.NodeForgotten:
		return "node forgotten: repoint to restore", "danger"
	case p.NodeID == "":
		return "no node yet", "warn"
	case !p.Online:
		return "offline", "danger"
	}
	return "online", "success"
}

// POST /-/admin/placements
func (h *handler) createPlacement(c echo.Context) error {
	f, err := c.FormParams()
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad form")
	}
	_, err = h.orch.CreatePlacement(c.Request().Context(), middleware.Principal(c), service.PlacementSpec{
		Name: f.Get("name"), Node: f.Get("node"), OpenToAll: f.Get("all") != "", Orgs: f["org"],
	})
	return h.plAnswer(c, "Placement added.", plOpen{}, err)
}

// POST /-/admin/placements/:placement/open
func (h *handler) openPlacement(c echo.Context) error {
	f, err := c.FormParams()
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad form")
	}
	err = h.orch.SetPlacementOpen(c.Request().Context(), middleware.Principal(c), c.Param("placement"), f.Get("all") != "", f["org"])
	return h.plAnswer(c, "Saved.", plOpen{}, err)
}

// POST /-/admin/placements/:placement/delete
func (h *handler) deletePlacement(c echo.Context) error {
	err := h.orch.DeletePlacement(c.Request().Context(), middleware.Principal(c), c.Param("placement"))
	return h.plAnswer(c, "Placement deleted.", plOpen{}, err)
}

// GET /-/admin/placements/:placement/move?to=: step 1 without to, the checks
// with it. Re-check is the same GET.
func (h *handler) moveWizard(c echo.Context) error {
	ctx, name, to := c.Request().Context(), c.Param("placement"), c.QueryParam("to")
	p, err := h.orch.PlacementByName(ctx, name)
	if err != nil {
		return middleware.HTTPError(err)
	}
	ns, err := h.orch.Nodes(ctx)
	if err != nil {
		return middleware.HTTPError(err)
	}
	w := &ui.MoveWizardView{
		Placement: name, From: p.NodeName, Step: 1, To: to,
		Check: plBase + "/" + name + "/move", Cancel: plBase,
	}
	for _, n := range ns {
		if n.ID != p.NodeID {
			w.Nodes = append(w.Nodes, ui.Option{Value: n.Name, Label: n.Name})
		}
	}
	if to == "" {
		return h.plAnswer(c, "", plOpen{wizard: w}, nil)
	}
	plan, err := h.orch.PlanPlacementMove(ctx, name, to)
	if err != nil {
		msg, ok := render.Refused(err)
		if !ok {
			return middleware.HTTPError(err)
		}
		w.Error = msg
		return h.plAnswer(c, "", plOpen{wizard: w}, nil)
	}
	return h.plAnswer(c, "", plOpen{wizard: moveChecksView(w, plan)}, nil)
}

// moveChecksView is the wizard on step 2: the CI-style list, the envs, and
// the confirm once every check passes.
func moveChecksView(w *ui.MoveWizardView, plan service.PlacementMovePlan) *ui.MoveWizardView {
	w.Step, w.From, w.To, w.OK, w.Restore = 2, plan.From, plan.To, plan.OK, plan.Restore
	w.Checks, w.Envs = nil, nil
	ok := true
	for _, k := range plan.Checks {
		w.Checks = append(w.Checks, ui.CheckRow{Name: k.Name, OK: k.OK, Detail: k.Detail})
		ok = ok && k.OK
	}
	// Apply stays off unless the service said so and no line failed
	w.OK = plan.OK && ok
	for _, e := range plan.Envs {
		r := ui.MoveEnvRow{Path: e.Path, Volumes: e.Volumes, Downtime: e.Downtime}
		if e.Bytes >= 0 {
			r.Size = service.HumanBytes(e.Bytes)
		}
		w.Envs = append(w.Envs, r)
	}
	if !w.OK {
		return w
	}
	vals, _ := json.Marshal(map[string]string{"to": plan.To, "confirm": plan.Placement})
	w.Confirm = comp.ConfirmView{
		Button:  "Move",
		Title:   "Move " + plan.Placement + " to " + plan.To + "?",
		Warning: "Envs stop while they move. Apps come back on " + plan.To + ".",
		Kept:    []string{"the backups, and the data on " + plan.From + " until the move ends"},
		Word:    plan.Placement,
		Action:  plBase + "/" + plan.Placement + "/move",
		Target:  target(),
		Vals:    string(vals),
		Primary: true,
		Body:    ui.ConfirmBody(plan.From, plan.To, w.Envs, plan.Restore),
	}
	return w
}

// POST /-/admin/placements/:placement/move (to, confirm): starts the move
// and answers its progress; the job is not awaited.
func (h *handler) startMove(c echo.Context) error {
	ctx, name := c.Request().Context(), c.Param("placement")
	_, err := h.orch.MovePlacement(ctx, middleware.Principal(c), name, c.FormValue("to"), c.FormValue("confirm"))
	if err != nil {
		return h.plAnswer(c, "", plOpen{}, err)
	}
	return h.progressAnswer(c, "Move started.")
}

// POST /-/admin/placements/:placement/retry
func (h *handler) retryMove(c echo.Context) error {
	_, err := h.orch.RetryPlacementMove(c.Request().Context(), middleware.Principal(c), c.Param("placement"))
	if err != nil {
		return h.plAnswer(c, "", plOpen{}, err)
	}
	return h.progressAnswer(c, "Retrying the envs left behind.")
}

// GET /-/admin/placements/:placement/progress; with ?poll=1 only the box,
// which swaps itself while the move runs.
func (h *handler) moveProgress(c echo.Context) error {
	if c.QueryParam("poll") == "" {
		return h.progressAnswer(c, "")
	}
	v, err := h.progressView(c)
	if err != nil {
		return middleware.HTTPError(err)
	}
	if v == nil {
		return c.NoContent(http.StatusNoContent)
	}
	return respond.HTML(c, http.StatusOK, ui.MoveProgress(*v))
}

func (h *handler) progressAnswer(c echo.Context, note string) error {
	v, err := h.progressView(c)
	if err != nil {
		return middleware.HTTPError(err)
	}
	return h.plAnswer(c, note, plOpen{progress: v}, nil)
}

// progressView is the placement's latest move; nil = it never moved.
func (h *handler) progressView(c echo.Context) (*ui.MoveProgressView, error) {
	name := c.Param("placement")
	st, err := h.orch.PlacementMove(c.Request().Context(), name)
	if err != nil || st == nil {
		return nil, err
	}
	return moveProgressView(name, *st), nil
}

// moveProgressView is a repoint's status as the progress box draws it.
func moveProgressView(name string, st service.PlacementMoveStatus) *ui.MoveProgressView {
	base := plBase + "/" + name
	v := &ui.MoveProgressView{
		Placement: name, State: st.State, From: st.From, To: st.To, Error: st.Error,
		Done: st.Done, All: st.All, Back: plBase,
	}
	switch st.State {
	case "running":
		v.Poll = base + "/progress?" + url.Values{"poll": {"1"}}.Encode()
	case "failed":
		v.Retry = base + "/retry"
	}
	for _, m := range st.Envs {
		e := ui.MoveProgressEnv{State: "done", Step: "moved", Error: m.Move.Error}
		switch {
		case m.Active:
			e.State, e.Step = "moving", ""
		case m.Move.Error != "":
			e.State, e.Step = "failed", "rolled back"
		}
		for _, s := range m.Steps {
			if s.State == "running" {
				e.Step = s.Label
				if s.Detail != "" {
					e.Step += ": " + s.Detail
				}
			}
		}
		v.Envs = append(v.Envs, e)
	}
	return v
}

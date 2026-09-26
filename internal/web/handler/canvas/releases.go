package canvas

import (
	"context"
	"strconv"
	"strings"

	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/service"
	comp "github.com/FyrmForge/stackr/internal/ui/components"
	"github.com/FyrmForge/stackr/internal/web/render"
)

// jobKey carries the job a promote queued, and the env page its stream
// hangs under, to the tab that answers it.
const jobKey = "drawer-job"

type queued struct {
	job  service.Job
	page string
}

// releasesTab is v0's releases page for a stack or env card: the ladder,
// the stack's releases newest first, a Promote / Roll back per env on each
// row (the env card: its own only), and with ?env=&plan= the question for
// one move, open (the env card asks for itself).
func (h *handler) releasesTab(c echo.Context, cd card, f *comp.DrawerView) (templ.Component, error) {
	ctx, st := c.Request().Context(), cd.s.Stack
	rs, err := h.orch.Releases(ctx, st.ID)
	if err != nil {
		return nil, err
	}
	es, err := h.orch.Ladder(ctx, st.ID)
	if err != nil {
		return nil, err
	}
	who, err := h.who(ctx)
	if err != nil {
		return nil, err
	}
	number := map[string]int{}
	for _, r := range rs {
		number[r.ID] = r.Number
	}
	runs := func(e service.Environment) int {
		if e.ReleaseID == nil {
			return 0
		}
		return number[*e.ReleaseID]
	}
	hues := service.EnvHues(es)
	var v comp.ReleasesView
	for _, e := range es {
		run := comp.EnvRun{Name: e.Name, Color: hues[e.ID]}
		if n := runs(e); n > 0 {
			run.Runs = strconv.Itoa(n)
		}
		v.Ladder = append(v.Ladder, run)
	}
	// every rung, the bottom one too: v0 gave it no moves, which left a
	// one-env stack no way to roll back
	var targets []service.Environment
	for _, e := range es {
		if cd.kind == "stack" || e.ID == cd.s.Env.ID {
			targets = append(targets, e)
		}
	}
	rows := make([]comp.ReleaseRow, len(rs))
	for i, r := range rs {
		rows[i] = comp.ReleaseRow{Number: strconv.Itoa(r.Number), By: byWord(r.CreatedBy, who), When: day(r.CreatedAt)}
		for _, e := range es {
			if runs(e) == r.Number {
				rows[i].Envs = append(rows[i].Envs, comp.EnvRun{Name: e.Name, Color: hues[e.ID]})
			}
		}
		for _, e := range targets {
			if runs(e) == r.Number {
				continue
			}
			rows[i].Moves = append(rows[i].Moves, comp.ReleaseMove{
				Env:  e.Name,
				Back: r.Number < runs(e),
				Ask:  f.Base + "?tab=releases&env=" + e.ID + "&plan=" + r.ID,
			})
		}
	}
	v.Rows = rows
	if q, ok := c.Get(jobKey).(queued); ok {
		jv := render.JobView(q.page, q.job)
		jv.Refresh = f.Base + "?tab=releases"
		v.Job = &jv
	}
	// the question: one env, one release
	plan, envID := c.QueryParam("plan"), c.QueryParam("env")
	if cd.kind == "env" {
		envID = cd.s.Env.ID
	}
	ri, ei := -1, -1
	for i, r := range rs {
		if r.ID == plan {
			ri = i
		}
	}
	for i, e := range es {
		if e.ID == envID {
			ei = i
		}
	}
	if ri < 0 || ei < 0 {
		return comp.Releases(v), nil
	}
	e, n, cur := es[ei], rs[ri].Number, runs(es[ei])
	p, err := h.orch.PlanPromote(ctx, e.ID, plan)
	if err != nil {
		return nil, err
	}
	ask := comp.PromoteAsk{Back: n < cur, Target: e.Name, Plan: planView(p)}
	lo, hi := min(n, cur), max(n, cur)
	for i, r := range rs {
		if r.Number > lo && r.Number <= hi {
			ask.Carried = append(ask.Carried, rows[i])
		}
	}
	if ei > 0 && !ask.Back && runs(es[ei-1]) < n {
		ask.Skipped = es[ei-1].Name
	}
	m := "#" + strconv.Itoa(n)
	cv := comp.ConfirmView{
		Button:  "Promote",
		Title:   "Promote " + m + " to " + e.Name + "?",
		Primary: true,
		Open:    true,
		Body:    comp.Ask(ask),
		Target:  "#" + comp.DrawerRoot,
	}
	switch {
	case ask.Back:
		cv.Button, cv.Title, cv.Primary = "Roll back", "Roll back "+e.Name+" to "+m+"?", false
	case ask.Skipped != "":
		cv.Button = "Promote anyway"
	}
	if p.CanDeploy && can(c, cd.s, "env.write") {
		cv.Action = f.Base + "/promote/" + plan
		if cd.kind == "stack" {
			cv.Action = f.Base + "/promote/" + e.ID + "/" + plan
		}
	}
	v.Ask = &cv
	return comp.Releases(v), nil
}

func planView(p service.PromotePlan) comp.PlanView {
	pv := comp.PlanView{
		Title:     "What changes",
		Blockers:  p.Plan.Blockers,
		Warnings:  p.Plan.Warnings,
		CanDeploy: p.CanDeploy,
	}
	for _, ch := range p.Plan.Changes {
		pv.Changes = append(pv.Changes, comp.ChangeView{
			Kind:  ch.Kind,
			Tile:  ch.Tile,
			Field: ch.Field,
			Old:   ch.Old,
			New:   ch.New,
			Note:  ch.Note,
		})
	}
	return pv
}

// who names users by id, for "user:<id>" in a release's CreatedBy.
// ponytail: the whole user list per render; a join verb replaces it.
func (h *handler) who(ctx context.Context) (map[string]string, error) {
	us, err := h.orch.Users(ctx)
	names := map[string]string{}
	for _, u := range us {
		names[u.ID] = u.Name
		if u.Name == "" {
			names[u.ID] = u.Email
		}
	}
	return names, err
}

// byWord is who or what cut a release, as a person reads it.
func byWord(by string, names map[string]string) string {
	if id, ok := strings.CutPrefix(by, "user:"); ok && names[id] != "" {
		return names[id]
	}
	switch by {
	case "push":
		return "git push"
	case "image-watch":
		return "image watch"
	}
	return by
}

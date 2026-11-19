package canvas

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/ui/access"
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
	who, err := h.who(ctx, cd.s.Org.ID)
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
	// one-env stack no way to roll back. A viewer, who cannot finish one,
	// gets none.
	var targets []service.Environment
	for _, e := range es {
		if !can(c, cd.s, "env.write") {
			break
		}
		if cd.kind == "stack" || e.ID == cd.s.Env.ID {
			targets = append(targets, e)
		}
	}
	rows := make([]comp.ReleaseRow, len(rs))
	for i, r := range rs {
		rows[i] = comp.ReleaseRow{Number: strconv.Itoa(r.Number), By: byWord(r.CreatedBy, who), Commit: r.Commit, Message: r.Message, When: r.CreatedAt}
		for _, e := range es {
			if runs(e) == r.Number {
				rows[i].Envs = append(rows[i].Envs, comp.EnvRun{Name: e.Name, Color: hues[e.ID]})
			}
		}
		for _, e := range targets {
			if runs(e) == r.Number {
				continue
			}
			label := "Review plan"
			if r.Number < runs(e) && h.orch.IsRollback(ctx, e.ID, r.ID) {
				label = "Review rollback"
			}
			if cd.kind == "stack" {
				label += " for " + e.Name
			}
			rows[i].Moves = append(rows[i].Moves, comp.ReleaseMove{
				Label: label,
				Ask:   f.Base + "?tab=releases&env=" + e.ID + "&plan=" + r.ID,
			})
		}
	}
	if cd.kind == "env" {
		if win, err := h.orch.ReleaseWindow(ctx, st.ID, cd.s.Env.ID); err == nil {
			for i := range rows {
				rows[i].Waiting = rowOf(win, rs[i].ID).State == service.StateWaiting
			}
		}
	}
	v.Rows = rows
	v.Live = f.Base + "?tab=releases"
	v.PushFail = render.PushFailure(c, h.orch)
	v.Lines = render.LiveLines(c, h.orch)
	if q, ok := c.Get(jobKey).(queued); ok {
		jv := render.JobView(q.page, q.job)
		jv.Refresh = f.Base + "?tab=releases"
		v.Job = &jv
	} else if jv, _ := render.ActiveJob(c, h.orch, "promote", "deploy"); jv != nil {
		jv.Refresh = f.Base + "?tab=releases"
		v.Job = jv
	}
	// the question: one env, one release
	plan, envID := c.QueryParam("plan"), c.QueryParam("env")
	if c.Request().Method == http.MethodGet && c.Param("release") != "" { // the page of one release's review
		plan = c.Param("release") // a promote's POST names one too, and answers with the tab
	}
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
	p, err := h.orch.PlanPromote(ctx, middleware.Principal(c), e.ID, plan)
	if err != nil {
		return nil, err
	}
	back := n < cur && h.orch.IsRollback(ctx, e.ID, plan)
	canGo := can(c, cd.s, "env.write") // the review disables its button on a blocker itself
	rv := comp.ReviewView{
		Back:   f.Base + "?tab=releases",
		Title:  "Release " + strconv.Itoa(n) + " for " + e.Name,
		By:     rows[ri].By,
		When:   rows[ri].When,
		Commit: rows[ri].Commit,
		Plan:   planView(p),
		Button: "Promote to " + e.Name,
	}
	rv.Plan.Title = "What changes"
	rv.Plan.Ticks = canGo
	if cd.kind == "env" && can(c, cd.s, "env.lock") {
		applyLocks(&rv.Plan, "/"+cd.s.Org.Slug+"/"+cd.s.Stack.Slug+"/"+e.Slug+"/-/drawer/lock")
	}
	rv.Plan.Gaps = nil
	for _, g := range p.Plan.Gaps {
		if g.Kind == "secret" && g.Scope == "env" {
			rv.Plan.Gaps = append(rv.Plan.Gaps, comp.GapView{Kind: "secret", Label: g.Key, Field: g.Key, Post: gapPost(f.Base, cd.kind, e.Slug, plan, e.ID)})
		}
	}
	if len(p.Access) > 0 { // the access blocker is its own block, not a refusal
		rv.Plan.Blockers = nil
		rv.Access = accessView(p.Access, middleware.Principal(c).Access.Admin)
	}
	win, _ := h.orch.ReleaseWindow(ctx, st.ID, e.ID)
	rv.Skip = skipNote(win, plan, f.Base, e.ID)
	switch {
	case back:
		rv.Title, rv.Chip, rv.Tone, rv.Button, rv.Danger = "Roll back "+e.Name+" to release "+strconv.Itoa(n), "Rollback", "warn", "Roll back "+e.Name, true
		rv.Notes = append(rv.Notes, "Removes "+strconv.Itoa(cur-n)+" release"+map[bool]string{true: "", false: "s"}[cur-n == 1]+" from "+e.Name+".")
	case rowOf(win, plan).State == service.StateWaiting:
		rv.Chip, rv.Tone = "Waiting", "accent"
	}
	if ei > 0 && !back && runs(es[ei-1]) < n {
		rv.Notes = append(rv.Notes, es[ei-1].Name+" does not run this release yet.")
		rv.Button = "Promote anyway"
	}
	if rv.Running = h.rolloutRunning(ctx, cd, e, rs); rv.Running != nil {
		rv.Live = f.Base + "?tab=releases&env=" + e.ID + "&plan=" + plan // it clears by itself
	}
	if canGo {
		rv.Action = f.Base + "/promote/" + plan
		if cd.kind == "stack" {
			rv.Action = f.Base + "/promote/" + e.Slug + "/" + plan
		}
	}
	if rv.Plan.Ticks && slices.ContainsFunc(rv.Plan.Changes, func(c comp.ChangeView) bool { return c.Optional }) {
		rv.Include = "[name=ticked]" // htmx logs an error when the selector finds nothing
	}
	return comp.Review(rv), nil
}

func planView(p service.PromotePlan) comp.PlanView {
	pv := comp.PlanView{
		Title:     "What changes",
		Blockers:  p.Plan.Blockers,
		Warnings:  p.Plan.Warnings,
		CanDeploy: p.CanDeploy,
	}
	for _, b := range p.Plan.Blockers {
		if f, ok := blockerFix(b); ok {
			if pv.Fix == nil {
				pv.Fix = map[string]comp.BlockerFix{}
			}
			pv.Fix[b] = f
		}
	}
	for _, ch := range p.Plan.Changes {
		pv.Changes = append(pv.Changes, comp.ChangeView{
			Details:  details(ch.Details),
			Kind:     ch.Kind,
			Tile:     ch.Tile,
			Field:    ch.Field,
			Old:      ch.Old,
			New:      ch.New,
			Note:     ch.Note,
			Key:      ch.Key,
			Optional: ch.Optional,
		})
	}
	return pv
}

// details reads a create row's "Label: value" lines.
func details(s string) []comp.DetailView {
	var out []comp.DetailView
	for _, l := range strings.Split(s, "\n") {
		if k, v, ok := strings.Cut(l, ": "); ok {
			out = append(out, comp.DetailView{Label: k, Value: v})
		}
	}
	return out
}

// applyLocks gives each lock row its own button: the file's value, set by
// SetEnvLock, then the releases tab again.
func applyLocks(pv *comp.PlanView, action string) {
	for i, ch := range pv.Changes {
		if ch.Kind == "lock" {
			pv.Changes[i].Apply = action
			pv.Changes[i].Vals = `{"tab":"releases","locked":"` + map[bool]string{true: "1", false: ""}[ch.New == "locked"] + `"}`
		}
	}
}

// who names the org's members by id, for "user:<id>" in a release's CreatedBy.
func (h *handler) who(ctx context.Context, orgID string) (map[string]string, error) {
	return h.orch.MemberNames(ctx, orgID) // names of the org's members, never an email
}

// byWord is who or what cut a release, as a person reads it.
func byWord(by string, names map[string]string) string { return render.SourceWord(by, names) }

// rolloutRunning is a promote or deploy of the env that is queued or
// running: a new promote waits for it, so the review says so.
func (h *handler) rolloutRunning(ctx context.Context, cd card, e service.Environment, rs []service.Release) *comp.ReviewRunning {
	js, err := h.orch.ScopeJobs(ctx, cd.s.Org.ID, cd.s.Stack.ID, e.ID, 0, "queued", "running")
	if err != nil {
		return nil
	}
	for _, j := range js { // newest first
		if j.Kind != "promote" && j.Kind != "deploy" {
			continue
		}
		run := &comp.ReviewRunning{Text: "A deploy is still running in " + e.Name + ".", Href: "/" + cd.s.Org.Slug + "/" + cd.s.Stack.Slug + "/" + e.Slug + "/-/jobs/" + j.ID}
		if j.ReleaseID != nil {
			if i := slices.IndexFunc(rs, func(r service.Release) bool { return r.ID == *j.ReleaseID }); i >= 0 {
				run.Text = "Release " + strconv.Itoa(rs[i].Number) + " is still deploying to " + e.Name + "."
			}
		}
		return run
	}
	return nil
}

// gapPost is where a review's missing-secret form posts: it saves the value
// and plans again.
func gapPost(base, kind, envSlug, plan, envID string) string {
	if kind == "stack" {
		return base + "/promote/" + envSlug + "/" + plan + "/secret?env=" + envID + "&plan=" + plan
	}
	return base + "/promote/" + plan + "/secret?plan=" + plan
}

// accessView groups the lines of a plan's elevated access under their
// tile, one line per grant; the source fingerprint is not a grant to read.
func accessView(lines []string, admin bool) *comp.ReviewAccess {
	a := &comp.ReviewAccess{Admin: admin}
	for _, l := range lines {
		p := access.Parse(l)
		if p.Kind == access.KindSource {
			continue
		}
		word := strings.ToLower(p.Label)
		if p.Detail != "" {
			word += " " + p.Detail
		}
		if n := len(a.Tiles); n == 0 || a.Tiles[n-1].Tile != p.Tile {
			a.Tiles = append(a.Tiles, comp.AccessTile{Tile: p.Tile})
		}
		a.Tiles[len(a.Tiles)-1].Grants = append(a.Tiles[len(a.Tiles)-1].Grants, word)
	}
	return a
}

// fillPromoteGap saves the value of one secret the plan of release in env
// waits on (the review's "Save and re-plan"). Only a secret the plan names
// can be set here; the value is never logged or echoed.
func (h *handler) fillPromoteGap(c echo.Context, envID, release string) error {
	ctx := c.Request().Context()
	p, err := h.orch.PlanPromote(ctx, middleware.Principal(c), envID, release)
	if err != nil {
		return err
	}
	name, value := c.FormValue("name"), c.FormValue("value")
	if value == "" {
		return errs.Invalidf("value", "Enter a value.")
	}
	col, key, _ := strings.Cut(name, ".")
	named := false
	for _, g := range p.Plan.Gaps {
		named = named || g.Kind == "secret" && g.Scope == "env" && g.Key == name
	}
	if !named || key == "" {
		return errs.Invalidf("name", "That secret is not missing.")
	}
	_, err = h.orch.SetParams(ctx, service.ParamScope{Kind: "env", ID: envID}, []service.ParamEntry{{Collection: col, Name: key, Kind: "secret", Value: value}})
	return err
}

// skipNote is the review's line for a waiting release that leaves out a
// lower-numbered waiting one, with the link to that one's own review.
func skipNote(w service.ReleaseWindow, plan, base, envID string) *comp.ReviewSkip {
	lacks := rowOf(w, plan).Lacks
	for _, r := range w.Rows {
		if lacks > 0 && r.Number == lacks {
			n := strconv.Itoa(lacks)
			what := "Release " + n
			if r.Commit != "" {
				what += " (" + short(r.Commit) + ")"
			}
			return &comp.ReviewSkip{
				Text:  what + " is newer config and is not in this release.",
				Label: "Review release " + n,
				Ask:   base + "?tab=releases&env=" + envID + "&plan=" + r.ID,
			}
		}
	}
	return nil
}

// rowOf is the window's row of a release, zero when it lists none.
func rowOf(w service.ReleaseWindow, id string) service.WindowRow {
	for _, r := range w.Rows {
		if r.ID == id && r.Kind == service.RowRelease {
			return r
		}
	}
	return service.WindowRow{}
}

// blockerFix is the one link that fixes a blocker the plan words as a
// sentence. ponytail: matched on the wording; a typed blocker (kind and
// target) in flow/promote/plan.go replaces it when more kinds need a link.
func blockerFix(b string) (comp.BlockerFix, bool) {
	if strings.Contains(b, "is an external route") {
		return comp.BlockerFix{Label: "Open server routes", Href: "/-/admin/traffic", Hint: "Remove the route first."}, true
	}
	return comp.BlockerFix{}, false
}

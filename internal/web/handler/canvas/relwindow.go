package canvas

import (
	"context"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/FyrmForge/stackr/internal/service"
	comp "github.com/FyrmForge/stackr/internal/ui/components"
	"github.com/FyrmForge/stackr/internal/web/render"
)

// rowEnv is what a row of the window needs from the stack it belongs to:
// its envs' names and colours, the links to its reviews and job logs.
type rowEnv struct {
	stack      string // its name, for a build under way
	names, hue map[string]string
	jobHref    func(id string) string
	review     func(rel string, rv service.WindowReview) comp.RelLink
}

// relRows maps the service's rows; of says which stack a row is of.
func (h *handler) relRows(ctx context.Context, rows []service.WindowRow, of func(service.WindowRow) rowEnv) []comp.RelRow {
	var who map[string]string // loaded when a row names a person
	var out []comp.RelRow
	for _, r := range rows {
		e := of(r)
		row := comp.RelRow{
			Kind: r.Kind, ID: r.ID, Commit: short(r.Commit), Message: r.Message, State: r.State,
			Tone: render.RelTone[r.State], Newest: r.Newest, At: r.At, Stack: e.stack, Queued: r.Queued,
			CommitURL: r.CommitURL,
		}
		if r.Number > 0 {
			row.Number = strconv.Itoa(r.Number)
		}
		if r.Lacks > 0 {
			row.Lacks = strconv.Itoa(r.Lacks)
		}
		if strings.HasPrefix(r.By, "user:") && who == nil {
			who, _ = h.who(ctx)
		}
		row.By = byWord(r.By, who)
		if row.Message == "" && r.Kind == service.RowRelease && row.By != "" {
			row.Message = strings.ToUpper(row.By[:1]) + row.By[1:]
		}
		switch r.Kind {
		case service.RowFailed:
			row.Message = "Push failed: " + strings.TrimRight(errTail(r.Err), ".")
			row.Link = comp.RelLink{Label: "Job log", Href: e.jobHref(r.JobID)}
		case service.RowBuilding:
			row.Link = comp.RelLink{Label: "Job log", Href: e.jobHref(r.JobID)}
		}
		for _, id := range r.Runs {
			row.Envs = append(row.Envs, comp.EnvRun{Name: e.names[id], Color: e.hue[id]})
		}
		if r.State == service.StateFailed && r.Kind == service.RowRelease {
			row.Link = comp.RelLink{Label: "Job log", Href: e.jobHref(r.JobID)}
			row.State = "failed in " + r.FailEnv
		}
		for _, rv := range r.Reviews {
			row.Reviews = append(row.Reviews, e.review(r.ID, rv))
		}
		if r.State == service.StateWaiting && len(row.Reviews) > 0 {
			row.Link = row.Reviews[0]
		}
		for _, j := range r.Jobs {
			t := j.Kind + ", " + j.State
			if j.Took > 0 {
				t += " in " + j.Took.Round(1e9).String()
			}
			row.Jobs = append(row.Jobs, comp.RelJob{Text: t, Href: e.jobHref(j.ID)})
		}
		out = append(out, row)
	}
	return out
}

// reviewLink is the link into ui1's review for a row of the stack at base:
// the env's drawer when envID is set (an env canvas), the stack's with the
// env named otherwise.
// bare = the row is already of one env's line on a list of many stacks (the
// org chart): the label leaves the env out.
func reviewLink(base, stackID, envID string, bare bool) func(string, service.WindowReview) comp.RelLink {
	return func(rel string, rv service.WindowReview) comp.RelLink {
		label := "Review plan"
		if rv.Rollback {
			label = "Review rollback"
		}
		q := "?drawer=" + url.QueryEscape("env:"+rv.EnvID) + "&tab=releases&plan=" + rel
		if envID == "" {
			if !bare {
				label += " for " + rv.Env
			}
			q = "?drawer=" + url.QueryEscape("stack:"+stackID) + "&tab=releases&env=" + rv.EnvID + "&plan=" + rel
		}
		return comp.RelLink{Label: label, Href: base + q}
	}
}

// relWindow is the releases window of a canvas: a stack or env's (the env
// links from the ladder, its releases), the org's (the org file's plans
// and every stack's open releases, with the switch between them). nil on
// the other canvases.
func (h *handler) relWindow(ctx context.Context, l level, gv service.GraphView) *comp.RelWindow {
	switch l.scope.Kind {
	case service.CanvasOrg:
		return h.orgWindow(ctx, l)
	case service.CanvasEnv, service.CanvasStack:
		return h.stackWindow(ctx, l, gv)
	}
	return nil
}

func (h *handler) stackWindow(ctx context.Context, l level, gv service.GraphView) *comp.RelWindow {
	stackID, envID := l.scope.ID, ""
	if l.scope.Kind == service.CanvasEnv {
		stackID, envID = l.stack, l.scope.ID
	}
	stack := l.base
	if envID != "" {
		stack = path.Dir(l.base) // the links are this env's siblings
	}
	w := &comp.RelWindow{}
	logEnv := ""
	hues, names := map[string]string{}, map[string]string{}
	for _, r := range gv.Compare {
		w.Envs = append(w.Envs, comp.RelEnv{Name: r.Name, Color: r.Color, Href: stack + "/" + r.Slug})
		hues[r.EnvID], names[r.EnvID] = r.Color, r.Name
		if logEnv == "" || r.EnvID == envID {
			logEnv = r.Slug
		}
	}
	win, err := h.orch.ReleaseWindow(ctx, stackID, envID)
	if err != nil {
		w.Err = true
		return w
	}
	w.Waiting, w.Failed = win.Waiting, win.Failed
	e := rowEnv{
		stack: win.Stack, names: names, hue: hues,
		jobHref: func(id string) string { return stack + "/" + logEnv + "/-/jobs/" + id },
		review:  reviewLink(l.base, stackID, envID, false),
	}
	w.Rows = h.relRows(ctx, win.Rows, func(service.WindowRow) rowEnv { return e })
	return w
}

// orgWindow is the org chart's window. A row says where it belongs first;
// the rows of a stack are what waits, fails, builds or runs there.
func (h *handler) orgWindow(ctx context.Context, l level) *comp.RelWindow {
	w := &comp.RelWindow{Switch: l.plans}
	win, err := h.orch.OrgReleaseWindow(ctx, l.scope.ID, l.plans)
	if err != nil {
		w.Err = true
		return w
	}
	w.Waiting, w.Failed, w.OrgWaiting, w.OrgFailed = win.Waiting, win.Failed, win.OrgWaiting, win.OrgFailed
	cfg := l.base + "?drawer=org:" + l.scope.ID + "&tab=config"
	envs := map[string]rowEnv{}
	for id, st := range win.Stacks {
		e := rowEnv{stack: st.Name, names: map[string]string{}, hue: map[string]string{}, jobHref: func(id string) string { return l.base + "/-/jobs/" + id }}
		for _, ev := range st.Envs {
			e.names[ev.ID], e.hue[ev.ID] = ev.Name, ev.Color
		}
		e.review = reviewLink(l.base+"/"+st.Slug, id, "", true)
		envs[id] = e
	}
	plans := 0
	for _, r := range win.Rows {
		if r.Kind == service.RowPlan {
			plans++
		}
	}
	if plans == 0 && l.plans {
		w.Note = comp.RelNote{Text: "No config plans yet."}
		if !win.Bound {
			w.Note = comp.RelNote{Text: "No config file is bound.", Link: "Bind a repository", Href: cfg}
		}
	}
	var out []comp.RelRow
	for _, r := range win.Rows {
		if r.Kind == service.RowPlan {
			out = append(out, render.RelPlanRow(r, "org config", cfg))
			continue
		}
		row := h.relRows(ctx, []service.WindowRow{r}, func(r service.WindowRow) rowEnv { return envs[r.StackID] })[0]
		row.OfStack, row.Scope = true, envs[r.StackID].stack
		if row.Number != "" {
			row.Scope += " " + row.Number
		}
		if r.Kind == service.RowBuilding {
			row.Stack = "" // the scope column says it
		}
		out = append(out, row)
	}
	w.Rows = out
	return w
}

func short(sha string) string { return sha[:min(7, len(sha))] }

func errTail(err string) string {
	s, _ := comp.ErrTail(err, 80)
	return s
}

package canvas

import (
	"context"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/FyrmForge/stackr/internal/service"
	comp "github.com/FyrmForge/stackr/internal/ui/components"
)

// relWindow is the releases window of a stack or env canvas: the env links
// from the ladder, the rows from the service. nil on the other canvases.
func (h *handler) relWindow(ctx context.Context, l level, gv service.GraphView) *comp.RelWindow {
	var stackID, envID string
	switch l.scope.Kind {
	case service.CanvasEnv:
		stackID, envID = l.stack, l.scope.ID
	case service.CanvasStack:
		stackID = l.scope.ID
	default:
		return nil
	}
	stack := l.base
	if envID != "" {
		stack = path.Dir(l.base) // the links are this env's siblings
	}
	w := &comp.RelWindow{}
	logEnv := ""
	for _, r := range gv.Compare {
		w.Envs = append(w.Envs, comp.RelEnv{Name: r.Name, Color: r.Color, Href: stack + "/" + r.Slug})
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
	hues := map[string]string{}
	names := map[string]string{}
	for _, r := range gv.Compare {
		hues[r.EnvID] = r.Color
		names[r.EnvID] = r.Name
	}
	var who map[string]string // loaded when a row names a person
	jobHref := func(id string) string { return stack + "/" + logEnv + "/-/jobs/" + id }
	review := func(rel string, rv service.WindowReview) comp.RelLink {
		label := "Review plan"
		if rv.Rollback {
			label = "Review rollback"
		}
		q := "?drawer=" + url.QueryEscape("env:"+rv.EnvID) + "&tab=releases&plan=" + rel
		if envID == "" {
			label += " for " + rv.Env
			q = "?drawer=" + url.QueryEscape("stack:"+stackID) + "&tab=releases&env=" + rv.EnvID + "&plan=" + rel
		}
		return comp.RelLink{Label: label, Href: l.base + q}
	}
	for _, r := range win.Rows {
		row := comp.RelRow{
			Kind: r.Kind, ID: r.ID, Commit: short(r.Commit), Message: r.Message, State: r.State,
			Tone: stateTone[r.State], Newest: r.Newest, At: r.At, Stack: win.Stack, Queued: r.Queued,
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
			row.Link = comp.RelLink{Label: "Job log", Href: jobHref(r.JobID)}
		case service.RowBuilding:
			row.Link = comp.RelLink{Label: "Job log", Href: jobHref(r.JobID)}
		}
		for _, id := range r.Runs {
			row.Envs = append(row.Envs, comp.EnvRun{Name: names[id], Color: hues[id]})
		}
		if r.State == service.StateFailed && r.Kind == service.RowRelease {
			row.Link = comp.RelLink{Label: "Job log", Href: jobHref(r.JobID)}
			row.State = "failed in " + r.FailEnv
		}
		for _, rv := range r.Reviews {
			row.Reviews = append(row.Reviews, review(r.ID, rv))
		}
		if r.State == service.StateWaiting && len(row.Reviews) > 0 {
			row.Link = row.Reviews[0]
		}
		for _, j := range r.Jobs {
			t := j.Kind + ", " + j.State
			if j.Took > 0 {
				t += " in " + j.Took.Round(1e9).String()
			}
			row.Jobs = append(row.Jobs, comp.RelJob{Text: t, Href: jobHref(j.ID)})
		}
		w.Rows = append(w.Rows, row)
	}
	return w
}

var stateTone = map[string]string{
	service.StateWaiting:   "accent",
	service.StateDeploying: "warn",
	service.StateRunning:   "success",
	service.StateFailed:    "danger",
}

func short(sha string) string { return sha[:min(7, len(sha))] }

func errTail(err string) string {
	s, _ := comp.ErrTail(err, 80)
	return s
}

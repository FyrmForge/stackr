package canvas

import (
	"context"
	"net/url"
	"path"
	"strings"

	"github.com/FyrmForge/stackr/internal/service"
	comp "github.com/FyrmForge/stackr/internal/ui/components"
	"github.com/FyrmForge/stackr/internal/web/render"
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
	w := &comp.RelWindow{All: "/-/releases?scope=" + url.QueryEscape(strings.TrimPrefix(stack, "/"))}
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
		verb := "Review"
		if l.viewOnly {
			verb = "View" // the review says who can promote
		}
		label := verb + " plan"
		if rv.Rollback {
			label = verb + " rollback"
		}
		q := "?drawer=" + url.QueryEscape("env:"+rv.EnvID) + "&tab=releases&plan=" + rel
		if envID == "" {
			label += " for " + rv.Env
			q = "?drawer=" + url.QueryEscape("stack:"+stackID) + "&tab=releases&env=" + rv.EnvID + "&plan=" + rel
		}
		return comp.RelLink{Label: label, Href: l.base + q}
	}
	envs := map[string]comp.EnvRun{}
	for _, r := range gv.Compare {
		envs[r.EnvID] = comp.EnvRun{Name: r.Name, Color: r.Color}
	}
	for _, r := range win.Rows {
		if strings.HasPrefix(r.By, "user:") && who == nil {
			who, _ = h.who(ctx)
		}
		row := render.RelRowOf(r, win.Stack, who, envs, jobHref)
		for _, rv := range r.Reviews {
			row.Reviews = append(row.Reviews, review(r.ID, rv))
		}
		if r.State == service.StateWaiting && len(row.Reviews) > 0 {
			row.Link = row.Reviews[0]
		}
		w.Rows = append(w.Rows, row)
	}
	return w
}

func short(sha string) string { return render.ShortSHA(sha) }

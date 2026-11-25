package canvas

import (
	"context"
	"net/url"
	"path"
	"slices"
	"strings"

	"github.com/FyrmForge/stackr/internal/service"
	comp "github.com/FyrmForge/stackr/internal/ui/components"
	"github.com/FyrmForge/stackr/internal/web/render"
)

// rowEnv is what a row of the window needs from the stack it belongs to:
// its envs' names and colours, the links to its reviews and job logs.
type rowEnv struct {
	stack   string                 // its name
	base    string                 // its address, where the release modal is under an env
	envs    map[string]comp.EnvRun // env id to name and hue
	jobHref func(id string) string
}

// whoOf names the org's members once for a render, when a row names a person
// (nil otherwise).
func (h *handler) whoOf(ctx context.Context, orgID string, rows []service.WindowRow) map[string]string {
	if !slices.ContainsFunc(rows, func(r service.WindowRow) bool { return render.IsPerson(r.By) }) {
		return nil
	}
	who, _ := h.who(ctx, orgID)
	return who
}

// relRows maps the service's rows; of says which stack a row is of, who the
// member names of the render (see whoOf). A row has no review link: its
// modal holds the way into the review.
func (h *handler) relRows(rows []service.WindowRow, who map[string]string, of func(service.WindowRow) rowEnv) []comp.ReleaseLineView {
	var out []comp.ReleaseLineView
	for _, r := range rows {
		e := of(r)
		out = append(out, render.ReleaseLineOf(r, e.stack, e.base, who, e.envs, e.jobHref))
	}
	return out
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
	w := &comp.RelWindow{All: "/-/releases?scope=" + url.QueryEscape(strings.TrimPrefix(stack, "/")), Retry: l.base + "?rel=1"}
	logEnv := ""
	envs := map[string]comp.EnvRun{}
	for _, r := range gv.Compare {
		w.Envs = append(w.Envs, comp.RelEnv{Name: r.Name, Color: r.Color, Href: stack + "/" + r.Slug})
		envs[r.EnvID] = comp.EnvRun{Name: r.Name, Color: r.Color}
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
		stack: win.Stack, base: stack, envs: envs,
		jobHref: func(id string) string { return stack + "/" + logEnv + "/-/jobs/" + id },
	}
	w.Rows = h.relRows(win.Rows, h.whoOf(ctx, l.org, win.Rows), func(service.WindowRow) rowEnv { return e })
	return w
}

// orgWindow is the org chart's window. A row says where it belongs first;
// the rows of a stack are what waits, fails, builds or runs there.
func (h *handler) orgWindow(ctx context.Context, l level) *comp.RelWindow {
	w := &comp.RelWindow{Switch: l.plans, All: "/-/releases?scope=" + url.QueryEscape(strings.TrimPrefix(l.base, "/")), Retry: l.base + "?rel=1"}
	win, err := h.orch.OrgReleaseWindow(ctx, l.scope.ID, l.plans)
	if err != nil {
		w.Err = true
		return w
	}
	w.Waiting, w.Failed, w.OrgWaiting, w.OrgFailed = win.Waiting, win.Failed, win.OrgWaiting, win.OrgFailed
	cfg := l.base + "?drawer=org:" + l.scope.ID + "&tab=config"
	envs := map[string]rowEnv{}
	for id, st := range win.Stacks {
		e := rowEnv{stack: st.Name, base: l.base + "/" + st.Slug, envs: map[string]comp.EnvRun{}, jobHref: func(id string) string { return l.base + "/-/jobs/" + id }}
		for _, ev := range st.Envs {
			e.envs[ev.ID] = comp.EnvRun{Name: ev.Name, Color: ev.Color}
		}
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
	who := h.whoOf(ctx, l.org, win.Rows)
	var out []comp.ReleaseLineView
	for _, r := range win.Rows {
		if r.Kind == service.RowPlan {
			out = append(out, render.RelPlanRow(r, "org config", cfg, l.viewOnly))
			continue
		}
		row := h.relRows([]service.WindowRow{r}, who, func(r service.WindowRow) rowEnv { return envs[r.StackID] })[0]
		row.OfStack = true
		out = append(out, row)
	}
	w.Rows = out
	return w
}

func short(sha string) string { return render.ShortSHA(sha) }

// Package releases is the Releases screen: every release, failure and config
// plan of the scopes the viewer can read, with filters and search, kept
// current by its own poll (releases view RFC, section 5).
package releases

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/ui/components"
	"github.com/FyrmForge/stackr/internal/web/render"
)

type handler struct{ orch *service.Orchestrator }

func NewHandler(orch *service.Orchestrator) *handler { return &handler{orch: orch} }

const path = "/-/releases"

// href is the screen at a scope and filters; empty ones stay out of it.
func href(scope, state, source, q string) string {
	v := url.Values{}
	for k, s := range map[string]string{"scope": scope, "state": state, "source": source, "q": q} {
		if s != "" {
			v.Set(k, s)
		}
	}
	if len(v) == 0 {
		return path
	}
	return path + "?" + v.Encode()
}

// GET /-/releases?scope=&state=&source=&q=
func (h *handler) Page(c echo.Context) error {
	ctx := c.Request().Context()
	q := service.ScreenQuery{Scope: c.QueryParam("scope"), State: c.QueryParam("state"), Source: c.QueryParam("source"), Q: strings.TrimSpace(c.QueryParam("q"))}
	sc, err := h.orch.ReleaseScreen(ctx, middleware.Principal(c), q)
	if err != nil {
		return middleware.HTTPError(err)
	}
	who := map[string]string{} // ponytail: the whole user list per render, as the canvas does
	if us, err := h.orch.Users(ctx); err == nil {
		for _, u := range us {
			who[u.ID] = u.Name
			if u.Name == "" {
				who[u.ID] = u.Email
			}
		}
	}
	v := components.RelScreen{
		Action: path, Live: c.Request().URL.RequestURI(), Scope: q.Scope, State: q.State, Source: q.Source, Q: q.Q, Total: sc.Total,
	}
	// the tree
	scopeName := ""
	if len(sc.Scopes) > 1 {
		v.Scopes = append(v.Scopes, components.RelScope{Label: "All scopes", Href: href("", q.State, q.Source, q.Q), Active: q.Scope == ""})
	}
	org := ""
	for _, s := range sc.Scopes {
		if s.Name != "" {
			org = s.Name
			v.Scopes = append(v.Scopes, components.RelScope{Label: s.Name}) // the heading
		}
		v.Scopes = append(v.Scopes, components.RelScope{Label: s.Label, Href: href(s.Key, q.State, q.Source, q.Q), Depth: s.Depth, Waiting: s.Waiting, Active: s.Key == q.Scope})
		if s.Key == q.Scope {
			scopeName = s.Label
			if s.Depth > 0 {
				scopeName = org + " / " + s.Label
			} else if s.Name != "" {
				scopeName = s.Name
			}
		}
	}
	// the state switch
	for _, st := range []struct{ key, label string }{{"", "All"}, {service.StateWaiting, "Waiting"}, {service.StateFailed, "Failed"}, {service.StateRunning, "Running"}} {
		v.States = append(v.States, components.RelStateLink{Label: st.label, Count: sc.Counts[st.key], Href: href(q.Scope, st.key, q.Source, q.Q), Active: q.State == st.key})
	}
	sourceLabel := func(key string) string {
		if key == "user" {
			return "manual"
		}
		return render.SourceWord(key, nil)
	}
	for _, s := range sc.Sources {
		v.Sources = append(v.Sources, components.RelOption{Value: s, Label: sourceLabel(s)})
	}
	for _, r := range sc.Rows {
		v.Rows = append(v.Rows, row(r, who))
	}
	if q.State != "" || q.Source != "" || q.Q != "" {
		v.Clear = href(q.Scope, "", "", "")
	}
	v.Empty = "No releases yet. A push or a first deploy makes one."
	if v.Clear != "" {
		var what []string
		if q.State != "" {
			what = append(what, q.State)
		}
		if q.Source != "" {
			what = append(what, sourceLabel(q.Source))
		}
		if q.Q != "" {
			what = append(what, strconv.Quote(q.Q))
		}
		in := "every scope"
		if scopeName != "" {
			in = scopeName
		}
		v.Empty = "No releases match " + strings.Join(what, ", ") + " in " + in + "."
	} else if q.Scope != "" && scopeName != "" {
		v.Empty = "No releases in " + scopeName + " yet."
	}
	return render.Page(c, http.StatusOK, "Releases", components.RelScreenView(v))
}

// row is a screen row as the window's row shape, with the screen's columns.
func row(r service.ScreenRow, who map[string]string) components.RelRow {
	out := components.RelRow{
		Kind: r.Kind, ID: r.ID, Commit: render.ShortSHA(r.Commit), Message: r.Message, State: r.State, Tone: render.StateTone[r.State],
		Newest: r.Newest, At: r.At, CommitURL: r.CommitURL, Scope: r.Scope, Plan: r.Kind == service.RowPlan, Fixed: r.Fixed,
		By: render.SourceWord(r.By, who),
	}
	if out.By == "" { // a release cut before its source was kept
		out.By = "git push"
	}
	out.Source = out.By
	if r.Number > 0 {
		out.Number = strconv.Itoa(r.Number)
	}
	if r.Lacks > 0 {
		out.Lacks = strconv.Itoa(r.Lacks)
	}
	if out.Message == "" && r.Kind == service.RowRelease {
		out.Message = strings.ToUpper(out.By[:1]) + out.By[1:]
	}
	base := "/" + r.Org + "/" + r.StackSlug
	jobHref := func(id string) string { return base + "/" + r.LogEnv + "/-/jobs/" + id }
	verb := "Review"
	if !r.CanPromote {
		verb = "View" // the review says who can promote
	}
	switch r.Kind {
	case service.RowFailed:
		out.Message = "Push failed: " + strings.TrimRight(render.ErrTail(r.Err), ".")
		if r.FailEnv != "" {
			out.Message = "Release " + out.Number + " failed in " + r.FailEnv + ": " + strings.TrimRight(render.ErrTail(r.Err), ".")
		}
		out.Link = components.RelLink{Label: "Job log", Href: jobHref(r.JobID)}
	case service.RowBuilding:
		out.Link = components.RelLink{Label: "Job log", Href: jobHref(r.JobID)}
	case service.RowPlan:
		out.Message = strings.TrimRight(out.Message, ".")
		if r.State == service.StateWaiting || r.State == service.StateFailed || r.State == "applying" {
			out.Link = components.RelLink{Label: verb + " plan", Href: "/-/admin/config"}
			if r.Org != "" {
				out.Link.Href = "/" + r.Org + "?drawer=" + url.QueryEscape("org:"+r.OrgID) + "&tab=config"
			}
		}
	}
	names := map[string]components.EnvRun{}
	for _, e := range r.Envs {
		names[e.ID] = components.EnvRun{Name: e.Name, Color: e.Color}
	}
	for _, id := range r.Runs {
		out.Envs = append(out.Envs, names[id])
	}
	switch {
	case r.Fixed && r.FixedBy > 0:
		out.State, out.Tone = "failed, fixed by release "+strconv.Itoa(r.FixedBy), ""
	case r.Fixed:
		out.State, out.Tone = "failed, fixed", ""
	case r.State == service.StateFailed && r.Kind == service.RowRelease:
		out.Link = components.RelLink{Label: "Job log", Href: jobHref(r.JobID)}
		out.State = "failed in " + r.FailEnv
	}
	for _, rv := range r.Reviews {
		label := verb + " plan"
		if rv.Rollback {
			label = verb + " rollback"
		}
		label += " for " + rv.Env
		out.Reviews = append(out.Reviews, components.RelLink{
			Label: label,
			Href:  base + "/" + rv.EnvSlug + "?drawer=" + url.QueryEscape("env:"+rv.EnvID) + "&tab=releases&plan=" + r.ID,
			Ask:   base + "/" + rv.EnvSlug + "/-/drawer?tab=releases&plan=" + r.ID,
		})
	}
	if r.State == service.StateWaiting && len(out.Reviews) > 0 {
		out.Link = out.Reviews[0]
		if len(out.Reviews) == 1 {
			out.Link.Label = strings.TrimSuffix(out.Link.Label, " for "+r.Reviews[0].Env)
		}
	}
	for _, j := range r.Jobs {
		t := j.Kind + ", " + j.State
		if j.Took > 0 {
			t += " in " + j.Took.Round(1e9).String()
		}
		out.Jobs = append(out.Jobs, components.RelJob{Text: t, Href: jobHref(j.ID)})
	}
	return out
}

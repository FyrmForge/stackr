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

// row is a screen row: the window's row, with the screen's columns.
func row(r service.ScreenRow, who map[string]string) components.RelRow {
	base := "/" + r.Org + "/" + r.StackSlug
	jobHref := func(id string) string { return base + "/" + r.LogEnv + "/-/jobs/" + id }
	envs := map[string]components.EnvRun{}
	for _, e := range r.Envs {
		envs[e.ID] = components.EnvRun{Name: e.Name, Color: e.Color}
	}
	out := render.RelRowOf(r.WindowRow, r.Scope, who, envs, jobHref)
	out.Scope, out.Source, out.Plan = r.Scope, out.By, r.Kind == service.RowPlan
	verb := "Review"
	if !r.CanPromote {
		verb = "View" // the review says who can promote
	}
	if r.Kind == service.RowPlan {
		out.Message = strings.TrimRight(out.Message, ".")
		if r.State == service.StateWaiting || r.State == service.StateFailed || r.State == "applying" {
			// the exact plan: the config tab opens its review, not the newest one's
			out.Link = components.RelLink{Label: verb + " plan", Href: "/-/admin/config?plan=" + url.QueryEscape(r.ID)}
			if r.Org != "" {
				out.Link.Href = "/" + r.Org + "?drawer=" + url.QueryEscape("org:"+r.OrgID) + "&tab=config&plan=" + url.QueryEscape(r.ID)
			}
		}
	}
	switch {
	case r.Fixed && r.FixedBy > 0:
		out.State, out.Tone = "failed, fixed by release "+strconv.Itoa(r.FixedBy), ""
	case r.Fixed:
		out.State, out.Tone = "failed, fixed", ""
	}
	for _, rv := range r.Reviews {
		label := verb + " plan"
		if rv.Rollback {
			label = verb + " rollback"
		}
		out.Reviews = append(out.Reviews, components.RelLink{
			Label: label + " for " + rv.Env,
			Href:  base + "/" + rv.EnvSlug + "/-/releases/" + r.ID, // the review as a page of its own
		})
	}
	if r.State == service.StateWaiting && len(out.Reviews) > 0 {
		out.Link = out.Reviews[0]
		if len(out.Reviews) == 1 {
			out.Link.Label = strings.TrimSuffix(out.Link.Label, " for "+r.Reviews[0].Env)
		}
	}
	return out
}

// Package releases is the Releases screen: every release, failure and config
// plan of the scopes the viewer can read, with filters and search, kept
// current by its own poll (releases view RFC, section 5).
package releases

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
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

// href is the screen at a scope, filters and page; empty ones stay out of it
// (page 1 is no page).
func href(scope, state, source, q string, page int) string {
	v := url.Values{}
	for k, s := range map[string]string{"scope": scope, "state": state, "source": source, "q": q} {
		if s != "" {
			v.Set(k, s)
		}
	}
	if page > 1 {
		v.Set("page", strconv.Itoa(page))
	}
	if len(v) == 0 {
		return path
	}
	return path + "?" + v.Encode()
}

// GET /-/releases?scope=&state=&source=&q=&page=
func (h *handler) Page(c echo.Context) error {
	ctx := c.Request().Context()
	q := service.ScreenQuery{Scope: c.QueryParam("scope"), State: c.QueryParam("state"), Source: c.QueryParam("source"), Q: strings.TrimSpace(c.QueryParam("q"))}
	q.Page, _ = strconv.Atoi(c.QueryParam("page")) // a bad one is 0, which is page 1
	sc, err := h.orch.ReleaseScreen(ctx, middleware.Principal(c), q)
	if err != nil {
		return middleware.HTTPError(err)
	}
	// names of the members of the orgs the rows belong to; never an email
	var orgs []string
	for _, r := range sc.Rows {
		if r.OrgID != "" && !slices.Contains(orgs, r.OrgID) {
			orgs = append(orgs, r.OrgID)
		}
	}
	who, _ := h.orch.MemberNames(ctx, orgs...)
	v := components.RelScreen{
		Action: path, Live: c.Request().URL.RequestURI(), Scope: q.Scope, State: q.State, Source: q.Source, Q: q.Q, Total: sc.Total,
	}
	// the tree
	scopeName := ""
	if len(sc.Scopes) > 1 {
		v.Scopes = append(v.Scopes, components.RelScope{Label: "All scopes", Href: href("", q.State, q.Source, q.Q, 0), Active: q.Scope == ""})
	}
	org := ""
	for _, s := range sc.Scopes {
		if s.Name != "" {
			org = s.Name
			v.Scopes = append(v.Scopes, components.RelScope{Label: s.Name}) // the heading
		}
		v.Scopes = append(v.Scopes, components.RelScope{Label: s.Label, Href: href(s.Key, q.State, q.Source, q.Q, 0), Depth: s.Depth, Waiting: s.Waiting, Active: s.Key == q.Scope})
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
		v.States = append(v.States, components.RelStateLink{Label: st.label, Count: sc.Counts[st.key], Href: href(q.Scope, st.key, q.Source, q.Q, 0), Active: q.State == st.key})
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
		v.Clear = href(q.Scope, "", "", "", 0)
	}
	if sc.Total > service.ScreenPage {
		from := (sc.Page-1)*service.ScreenPage + 1
		v.Range = fmt.Sprintf("%d-%d of %d", from, from+len(sc.Rows)-1, sc.Total)
		if sc.Page > 1 {
			v.Newer = href(q.Scope, q.State, q.Source, q.Q, sc.Page-1)
		}
		if from+len(sc.Rows) <= sc.Total {
			v.Older = href(q.Scope, q.State, q.Source, q.Q, sc.Page+1)
		}
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
	return render.PageBar(c, http.StatusOK, "Releases", "", components.RelScreenView(v), nil, nil)
}

// row is a screen row: the window's row, plus the screen's plans and fixed
// failures. A release opens its modal; a plan's link opens its review.
func row(r service.ScreenRow, who map[string]string) components.ReleaseLineView {
	base := "/" + r.Org + "/" + r.StackSlug
	jobHref := func(id string) string { return base + "/" + r.LogEnv + "/-/jobs/" + id }
	envs := map[string]components.EnvRun{}
	for _, e := range r.Envs {
		envs[e.ID] = components.EnvRun{Name: e.Name, Color: e.Color}
	}
	out := render.ReleaseLineOf(r.WindowRow, r.Scope, base, who, envs, jobHref)
	if r.Kind == service.RowPlan {
		verb := "Review"
		if !r.CanPromote {
			verb = "View" // the review says who can promote
		}
		out.Number, out.Message, out.Cause = "", "", strings.TrimRight(r.Message, ".")
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
	return out
}

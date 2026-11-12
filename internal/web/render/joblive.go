package render

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/ui/components"
)

// ActiveJob is the newest queued, waiting or running job of the request's
// scope (the panel when the route has no org, else the env, else the org) whose
// kind is one of kinds, as a status line with the stream that follows it. nil
// = none runs. A tab uses it when it was handed no job, so a job started
// elsewhere shows up, and JobLive answers the slot's poll with it.
func ActiveJob(c echo.Context, orch *service.Orchestrator, kinds ...string) (*components.JobStatusView, error) {
	return activeJob(c, orch, nil, kinds)
}

// activeJob is ActiveJob narrowed to the jobs whose payload holds every pair
// of where (tile_id, node_id); no kinds = any kind.
func activeJob(c echo.Context, orch *service.Orchestrator, where map[string]string, kinds []string) (*components.JobStatusView, error) {
	return scopeJob(c, orch, where, kinds, nil, time.Time{})
}

// scopeJob is activeJob over states (nil = the active ones, empty = any); a
// non-zero since keeps only the jobs created at or after it.
func scopeJob(c echo.Context, orch *service.Orchestrator, where map[string]string, kinds, states []string, since time.Time) (*components.JobStatusView, error) {
	ctx, s := c.Request().Context(), middleware.ScopeOf(c)
	active := states
	if states == nil {
		active = []string{"queued", "waiting", "running"}
	}
	var js []service.Job
	var err error
	page := ""
	switch {
	case s.Org == nil:
		js, err = orch.Jobs(ctx, 0, active...)
	case s.Env != nil:
		page = EnvURL(c)
		js, err = orch.ScopeJobs(ctx, s.Org.ID, s.Stack.ID, s.Env.ID, 0, active...)
	case s.Stack != nil:
		page = "/" + s.Org.Slug // job routes exist at the org, not the stack
		js, err = orch.ScopeJobs(ctx, s.Org.ID, s.Stack.ID, "", 0, active...)
	default:
		page = "/" + s.Org.Slug
		js, err = orch.ScopeJobs(ctx, s.Org.ID, "", "", 0, active...)
	}
	if err != nil {
		return nil, err
	}
	for _, j := range js { // newest first
		if (len(kinds) == 0 || slices.Contains(kinds, j.Kind)) && inPayload(j, where) && !j.CreatedAt.Before(since) {
			v := JobView(page, j)
			return &v, nil
		}
	}
	return nil, nil
}

func inPayload(j service.Job, where map[string]string) bool {
	if len(where) == 0 {
		return true
	}
	var p map[string]any
	_ = json.Unmarshal([]byte(j.Payload), &p)
	for k, v := range where {
		if s, _ := p[k].(string); s != v {
			return false
		}
	}
	return true
}

// JobLive answers a job slot's poll (components.JobSlot) with ActiveJob's job
// as the slot's live status, narrowed to ?kind=a,b (none = any), ?tile=<id>
// and ?node=<id> (the job's payload), or 204 when none runs, which leaves the
// slot as it is. A live status refreshes ?refresh=<tab uri> when its job ends,
// else asks for the slot again with ?end=1, which answers the newest job of the
// slot (finished or not) as an idle slot that polls once more, so a later job
// is picked up. With ?since=<unix ms> (the slot's render time) a job created
// since in any state answers too, so a sub-second job still shows.
func JobLive(orch *service.Orchestrator) echo.HandlerFunc {
	return func(c echo.Context) error {
		var kinds []string
		if k := c.QueryParam("kind"); k != "" {
			kinds = strings.Split(k, ",")
		}
		where, filter := map[string]string{}, ""
		if t := c.QueryParam("tile"); t != "" {
			where["tile_id"], filter = t, "tile="+url.QueryEscape(t)
		}
		if n := c.QueryParam("node"); n != "" {
			where["node_id"], filter = n, "node="+url.QueryEscape(n)
		}
		tab := c.QueryParam("refresh")
		if !strings.HasPrefix(tab, "/") || strings.HasPrefix(tab, "//") || strings.Contains(tab, "\\") {
			tab = ""
		}
		r := c.Request()
		ctx := components.WithTab(r.Context(), tab) // the slot drawn below polls with it
		c.SetRequest(r.WithContext(ctx))
		end := c.QueryParam("end") != ""
		var states []string
		if end {
			states = []string{} // any state: the job that just finished shows
		}
		var since time.Time
		if ms, err := strconv.ParseInt(c.QueryParam("since"), 10, 64); err == nil && ms > 0 {
			since = time.UnixMilli(ms)
		}
		v, err := scopeJob(c, orch, where, kinds, states, time.Time{})
		if err == nil && v == nil && !since.IsZero() && !end {
			// a job that began and ended between two polls
			v, err = scopeJob(c, orch, where, kinds, []string{}, since)
		}
		if err != nil {
			return middleware.HTTPError(err)
		}
		if v == nil {
			if end {
				return respond.HTML(c, http.StatusOK, components.JobSlotFor(nil, strings.Join(kinds, ","), filter))
			}
			return c.NoContent(http.StatusNoContent)
		}
		if !v.Live {
			return respond.HTML(c, http.StatusOK, components.JobSlotFor(v, strings.Join(kinds, ","), filter))
		}
		if tab != "" {
			v.Refresh = tab
		} else {
			q := r.URL.Query()
			q.Set("end", "1")
			q.Del("refresh") // not the tab: it failed the check above
			v.Refresh, v.Target = r.URL.Path+"?"+q.Encode(), "#job-slot"
		}
		return respond.HTML(c, http.StatusOK, components.JobSlotLive(*v))
	}
}

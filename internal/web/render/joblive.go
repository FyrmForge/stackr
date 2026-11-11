package render

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"

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
	ctx, s := c.Request().Context(), middleware.ScopeOf(c)
	active := []string{"queued", "waiting", "running"}
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
		page = "/" + s.Org.Slug + "/" + s.Stack.Slug
		js, err = orch.ScopeJobs(ctx, s.Org.ID, s.Stack.ID, "", 0, active...)
	default:
		page = "/" + s.Org.Slug
		js, err = orch.ScopeJobs(ctx, s.Org.ID, "", "", 0, active...)
	}
	if err != nil {
		return nil, err
	}
	for _, j := range js { // newest first
		if (len(kinds) == 0 || slices.Contains(kinds, j.Kind)) && inPayload(j, where) {
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
// slot as it is.
func JobLive(orch *service.Orchestrator) echo.HandlerFunc {
	return func(c echo.Context) error {
		var kinds []string
		if k := c.QueryParam("kind"); k != "" {
			kinds = strings.Split(k, ",")
		}
		where := map[string]string{}
		if t := c.QueryParam("tile"); t != "" {
			where["tile_id"] = t
		}
		if n := c.QueryParam("node"); n != "" {
			where["node_id"] = n
		}
		v, err := activeJob(c, orch, where, kinds)
		if err != nil {
			return middleware.HTTPError(err)
		}
		if v == nil {
			return c.NoContent(http.StatusNoContent)
		}
		return respond.HTML(c, http.StatusOK, components.JobSlotLive(*v))
	}
}

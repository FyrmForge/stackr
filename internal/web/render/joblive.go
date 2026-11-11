package render

import (
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
	default:
		page = "/" + s.Org.Slug
		js, err = orch.ScopeJobs(ctx, s.Org.ID, "", "", 0, active...)
	}
	if err != nil {
		return nil, err
	}
	for _, j := range js { // newest first
		if slices.Contains(kinds, j.Kind) {
			v := JobView(page, j)
			return &v, nil
		}
	}
	return nil, nil
}

// JobLive answers a job slot's poll (components.JobSlot) with ActiveJob's job
// as the slot's live status, narrowed to ?kind=a,b, or 204 when none runs,
// which leaves the slot as it is.
func JobLive(orch *service.Orchestrator) echo.HandlerFunc {
	return func(c echo.Context) error {
		v, err := ActiveJob(c, orch, strings.Split(c.QueryParam("kind"), ",")...)
		if err != nil {
			return middleware.HTTPError(err)
		}
		if v == nil {
			return c.NoContent(http.StatusNoContent)
		}
		return respond.HTML(c, http.StatusOK, components.JobSlotLive(*v))
	}
}

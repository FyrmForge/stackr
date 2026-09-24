package v1

import (
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/service"
)

// JobLogOut is a job and its log from an offset: poll again from Next
// until End.
type JobLogOut struct {
	Job  service.Job `json:"job"`
	Log  string      `json:"log"`
	Next int64       `json:"next"`
	End  bool        `json:"end"`
}

func (h *H) GetJob() Endpoint {
	return Get(func(c echo.Context) (service.Job, error) { return h.S.GetJob(rc(c), c.Param("job")) })
}

// PollJob is the job plus its log from ?offset=.
func (h *H) PollJob() Endpoint {
	return Get(func(c echo.Context) (JobLogOut, error) {
		var offset int64
		if err := echo.QueryParamsBinder(c).Int64("offset", &offset).BindError(); err != nil {
			return JobLogOut{}, err
		}
		j, l, err := h.S.PollJob(rc(c), c.Param("job"), offset)
		return JobLogOut{j, string(l.Chunk), l.Next, l.End}, err
	})
}

func (h *H) CancelJob() Endpoint {
	return Done(func(c echo.Context, _ None) error { return h.S.CancelJob(rc(c), c.Param("job")) })
}

// Jobs is every job, ?state= repeated to filter.
func (h *H) Jobs() Endpoint {
	return Get(func(c echo.Context) ([]service.Job, error) { return list(h.S.Jobs(rc(c), c.QueryParams()["state"]...)) })
}

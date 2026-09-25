package v1

import (
	"context"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/api/stream"
	"github.com/FyrmForge/stackr/internal/service"
)

const eventStream = "text/event-stream"

// JobEvents follows a job: an "update" event each time its log grows, an
// "end" event with the finished job. Each carries a JobLogOut whose Log is
// the new part only.
func (h *H) JobEvents() Endpoint {
	return Streamed(eventStream, func(c echo.Context) error {
		id := c.Param("job")
		return stream.Poll(c, func(ctx context.Context, offset int64) (any, int64, bool, error) {
			j, l, err := h.Orch.PollJob(ctx, id, max(offset, 0))
			return JobLogOut{j, string(l.Chunk), l.Next, l.End}, l.Next, l.End, err
		})
	})
}

// EnvEvents is the env canvas's live stream. Today one event: "traffic",
// the env's lanes ([]Edge) after each 5 s sample.
func (h *H) EnvEvents() Endpoint {
	return Streamed(eventStream, func(c echo.Context) error {
		env := envID(c)
		last, edges := int64(-1), []service.Edge{}
		return stream.PollAs(c, "traffic", func(ctx context.Context, _ int64) (any, int64, bool, error) {
			// the lanes are re-read only when a sample landed
			if seq := h.Orch.TrafficSeq(); seq != last {
				es, err := list(h.Orch.Traffic(ctx, env))
				if err != nil {
					return nil, 0, false, err
				}
				last, edges = seq, es
			}
			return edges, last, false, nil
		})
	})
}

// LogStream follows one replica's log (?container=, ?tail=), or with
// ?run= a cron or function run's log until the run ends: a "line" event
// per line.
func (h *H) LogStream() Endpoint {
	return Streamed(eventStream, func(c echo.Context) error {
		tail := 100
		if err := echo.QueryParamsBinder(c).Int("tail", &tail).BindError(); err != nil {
			return err
		}
		ctx := context.WithoutCancel(rc(c))
		follow := func() (<-chan string, func(), error) {
			return h.Orch.FollowLogs(ctx, tileID(c), c.QueryParam("container"), tail)
		}
		if run := c.QueryParam("run"); run != "" {
			follow = func() (<-chan string, func(), error) { return h.Orch.FollowRunLog(ctx, tileID(c), run, tail) }
		}
		lines, stop, err := follow()
		if err != nil {
			return err
		}
		return stream.Lines(c, lines, stop)
	}).Q("container", "run", "tail")
}

// Exec runs ?cmd= (repeated, one argv word each) in one replica
// (?container=): the request body is its stdin, the response its output.
func (h *H) Exec() Endpoint {
	return Streamed("application/octet-stream", func(c echo.Context) error {
		out, wait, err := h.Orch.Terminal(
			context.WithoutCancel(rc(c)),
			tileID(c),
			c.QueryParam("container"),
			c.QueryParams()["cmd"],
			c.Request().Body,
		)
		if err != nil {
			return err
		}
		defer func() { _ = wait() }()
		return stream.Pipe(c, "application/octet-stream", out)
	}).Q("cmd", "container")
}

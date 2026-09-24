// Package env is the env canvas's routes that are not the canvas itself:
// its events stream and the env-level drawers and dialogs (slice, volume,
// proxy, create tile, rollback). Every handler is one verb call and a
// view struct.
package env

import (
	"context"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/api/stream"
	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/ui/graph/cards"
	"github.com/FyrmForge/stackr/internal/web/render"
)

type handler struct{ orch *service.Orchestrator }

func NewHandler(orch *service.Orchestrator) *handler { return &handler{orch: orch} }

func scope(c echo.Context) service.Scope { return middleware.ScopeOf(c) }

// GET /:org/:stack/:env/events: the env canvas's stream. Today one event,
// "traffic": the rendered lanes after each 5 s sample, the first at
// connect. ponytail: the canvas (task 7) folds its "footer:<id>" and
// "graph" events into this one stream.
func (h *handler) Events(c echo.Context) error {
	env := scope(c).Env.ID
	last, body := int64(-1), stream.HTML("")
	return stream.PollAs(c, "traffic", func(ctx context.Context, _ int64) (any, int64, bool, error) {
		if seq := h.orch.TrafficSeq(); seq != last {
			es, err := h.orch.Traffic(ctx, env)
			if err != nil {
				return nil, 0, false, err
			}
			if body, err = render.Event(ctx, cards.Lanes(lanes(es))); err != nil {
				return nil, 0, false, err
			}
			last = seq
		}
		return body, last, false, nil
	})
}

func lanes(es []service.Edge) []cards.Lane {
	out := make([]cards.Lane, 0, len(es))
	for _, e := range es {
		out = append(out, cards.Lane{From: e.From, To: e.To, BPS: e.BPS})
	}
	return out
}

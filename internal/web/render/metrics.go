package render

import (
	"context"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/api/stream"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/ui/components"
)

// MetricsLive is an open metrics view: its window (a Series or a
// TileMetricsOut), written into the page chart by chart, and the events URL
// its charts append from (after= the round the window holds). reload is
// the view's GET without ?range=.
func MetricsLive(orch *service.Orchestrator, body any, rng, reload, events string) components.MetricsView {
	return components.MetricsView{
		Range:  rng,
		Reload: reload,
		Events: events + "?after=" + strconv.FormatInt(orch.MetricsSeq(), 10),
		Pick:   func(measures string) any { return pick(body, strings.Fields(measures)) },
		Long:   orch.ObserveOn(),
	}
}

// pick is body with only the measures one chart draws.
func pick(body any, measures []string) any {
	only := func(s service.Series) service.Series {
		out := service.Series{}
		for _, m := range measures {
			if xs, ok := s[m]; ok {
				out[m] = xs
			}
		}
		return out
	}
	switch b := body.(type) {
	case service.Series:
		return only(b)
	case service.TileMetricsOut:
		out := service.TileMetricsOut{Tile: only(b.Tile), Replicas: map[string]service.Series{}}
		for r, s := range b.Replicas {
			out.Replicas[r] = only(s)
		}
		return out
	}
	return body
}

// MetricsEvents streams each sampler round's points in scope as a "points"
// event, shaped like the window the page holds.
func MetricsEvents(c echo.Context, orch *service.Orchestrator, s service.MetricsScope) error {
	seq, _ := strconv.ParseInt(c.QueryParam("after"), 10, 64)
	seq = min(seq, orch.MetricsSeq()) // a restarted panel counts from 0 again
	return stream.Watch(c, func(context.Context) ([]stream.Msg, error) {
		var body any
		body, seq = orch.MetricsAfter(s, seq)
		if body == nil {
			return nil, nil
		}
		return []stream.Msg{{Name: "points", Body: body}}, nil
	})
}

package render

import (
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/ui/components"
)

// LogHistory is the log viewer for the request's filters (?tile= ?kind=
// ?source= ?stream= ?run= ?since= ?text= ?before=) over the env of s. With
// ?from=tile it is one tile's (?tile= its slug), with a Live button back to
// the tile's live tail. The lines come from SearchLogs under s, so the org is
// the caller's own whatever the query says.
func LogHistory(c echo.Context, orch *service.Orchestrator, s service.Scope) (components.LogHistoryView, error) {
	q := c.QueryParam
	env := "/" + s.Org.Slug + "/" + s.Stack.Slug + "/" + s.Env.Slug
	v := components.LogHistoryView{
		Action: env + "/-/logs", Tile: q("tile"), Kind: q("kind"), Source: q("source"),
		Stream: q("stream"), Run: q("run"), Text: q("text"), Range: "1h",
	}
	for _, r := range components.LogRanges {
		if q("since") == r.Value {
			v.Range = r.Value
		}
	}
	if q("from") == "tile" {
		v.Live = env + "/-/tiles/" + url.PathEscape(v.Tile) + "?tab=logs"
	} else {
		ts, err := orch.Tiles(c.Request().Context(), s.Env.ID)
		if err != nil {
			return v, err
		}
		v.Tiles = []components.LogOption{{Value: "", Label: "all tiles"}}
		for _, t := range ts {
			v.Tiles = append(v.Tiles, components.LogOption{Value: t.Slug, Label: t.Name})
		}
		slices.SortFunc(v.Tiles[1:], func(a, b components.LogOption) int { return strings.Compare(a.Label, b.Label) })
	}
	in := service.LogsIn{Tile: v.Tile, Kind: v.Kind, Source: v.Source, Stream: v.Stream, Run: v.Run, Text: v.Text}
	in.Since, _ = time.ParseDuration(v.Range)
	in.Before, _ = time.Parse(time.RFC3339Nano, q("before"))
	ls, err := orch.SearchLogs(c.Request().Context(), s, in)
	if err != nil {
		if msg, ok := Refused(err); ok {
			v.Error = msg
			return v, nil
		}
		return v, err
	}
	for _, l := range ls {
		v.Lines = append(v.Lines, components.LogHistoryLine{
			Time: l.Time, Tile: l.Tile, Source: l.Source,
			Text: logText(l), Err: l.Stream == "stderr",
		})
	}
	if len(ls) == service.LogsPage {
		p := url.Values{}
		for k, x := range map[string]string{
			"tile": v.Tile, "kind": v.Kind, "source": v.Source, "stream": v.Stream, "run": v.Run,
			"text": v.Text, "since": v.Range, "from": q("from"),
			"before": ls[len(ls)-1].Time.UTC().Format(time.RFC3339Nano),
		} {
			if x != "" {
				p.Set(k, x)
			}
		}
		v.Older = v.Action + "?" + p.Encode()
	}
	return v, nil
}

// logText is a line as shown: an access line compact ("GET /path 200 12ms"),
// any other its message.
func logText(l service.LogLine) string {
	if l.Source != "access" || l.HTTP == nil {
		return l.Msg
	}
	s := strings.TrimSpace(l.HTTP["http.method"] + " " + l.HTTP["http.path"] + " " + l.HTTP["http.status"])
	if d := l.HTTP["http.duration_ms"]; d != "" {
		s += " " + d + "ms"
	}
	return s
}

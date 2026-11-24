package render

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
	"regexp"
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
	// a tile viewer's tile is its scope, not a filter
	v.Filtered = v.Kind+v.Source+v.Stream+v.Run+v.Text != "" || (v.Live == "" && v.Tile != "")
	in := service.LogsIn{Tile: v.Tile, Kind: v.Kind, Source: v.Source, Stream: v.Stream, Run: v.Run, Text: v.Text}
	in.Since, _ = time.ParseDuration(v.Range)
	in.Before, _ = time.Parse(time.RFC3339Nano, q("before"))
	ls, err := orch.SearchLogs(c.Request().Context(), s, in)
	if errors.Is(err, service.ErrObserveDown) {
		slog.Warn("observe: log search failed", "err", err)
		v.Error = "Logs are unavailable right now. The observe service is not running."
		return v, nil
	}
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
			Text: logText(l), Level: logLevel(l.Msg, l.Stream),
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

var (
	logfmtLevel   = regexp.MustCompile(`(?i)(?:^|\s)(?:level|lvl|severity)="?([a-z]+)`)
	bracketLevel  = regexp.MustCompile(`(?i)\[(?:[a-z_]+:)?(error|err|fatal|emerg|crit|critical|alert|warn|warning|info|notice|debug|trace)\]`)
	bareLevel     = regexp.MustCompile(`\b(ERROR|FATAL|PANIC|SEVERE|CRITICAL|WARN|WARNING|INFO|DEBUG|TRACE|LOG|NOTICE|DETAIL|HINT|STATEMENT|CONTEXT)\b`)
	glogLevel     = regexp.MustCompile(`^([EFWI])\d{4} \d\d:\d\d:\d\d`)
	crashPrefixes = []string{"panic:", "fatal error:", "traceback (most recent call last):", "exception in thread", "error:", "unhandled exception"}
	jsonLevelKeys = []string{"level", "severity", "lvl", "log.level", "@l"}
)

// levelWord maps a level word to "error", "warn" or "info" (any quieter
// level); "" is not a level word.
func levelWord(w string) string {
	switch strings.ToLower(w) {
	case "error", "err", "eror", "fatal", "crit", "critical", "alert", "emerg", "emergency", "panic", "severe":
		return "error"
	case "warn", "warning":
		return "warn"
	case "info", "information", "informational", "notice", "log", "detail", "hint", "statement", "context", "debug", "dbg", "trace", "verbose":
		return "info"
	}
	return ""
}

// logLevel is how a line is coloured: "error", "warn" or "". It reads the
// level the app wrote (JSON, logfmt, [Error], ERROR, glog, crash text); a line
// that names none is an error when it came from stderr. A line that names a
// quiet level (info, debug) is plain even on stderr.
func logLevel(text, stream string) string {
	t := strings.TrimSpace(components.StripStamp(components.StripANSI(text)))
	lv := ""
	low := strings.ToLower(t)
	for _, p := range crashPrefixes {
		if strings.HasPrefix(low, p) {
			return "error"
		}
	}
	if strings.HasPrefix(t, "{") {
		var m map[string]any
		if json.Unmarshal([]byte(t), &m) == nil {
			for _, k := range jsonLevelKeys {
				switch v := m[k].(type) {
				case string:
					lv = levelWord(v)
				case float64:
					if k == "level" && v >= 10 && v <= 60 {
						lv = "info"
						if v >= 50 {
							lv = "error"
						} else if v >= 40 {
							lv = "warn"
						}
					}
				}
				if lv != "" {
					break
				}
			}
		}
	}
	head := t
	if len(head) > 80 {
		head = head[:80]
	}
	if lv == "" {
		if m := logfmtLevel.FindStringSubmatch(t); m != nil {
			lv = levelWord(m[1])
		}
	}
	if lv == "" {
		if m := bracketLevel.FindStringSubmatch(head); m != nil {
			lv = levelWord(m[1])
		}
	}
	if lv == "" {
		if m := bareLevel.FindStringSubmatch(head); m != nil {
			lv = levelWord(m[1])
		}
	}
	if lv == "" {
		if m := glogLevel.FindStringSubmatch(t); m != nil {
			lv = map[string]string{"E": "error", "F": "error", "W": "warn", "I": "info"}[m[1]]
		}
	}
	switch {
	case lv == "info":
		return ""
	case lv != "":
		return lv
	case stream == "stderr":
		return "error"
	}
	return ""
}

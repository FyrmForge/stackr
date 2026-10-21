package service

import (
	"context"
	"slices"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/logs"
)

// LogsPage is how many lines one search returns; LogsMaxSince is how far back
// a search may reach (the longest range the viewer offers).
const (
	LogsPage     = 500
	LogsMaxSince = 7 * 24 * time.Hour
)

// LogsOn says whether log history is kept (observe and observe_logs_enabled).
func (o *Orchestrator) LogsOn() bool { return o.logship.on.Load() }

// LogsIn narrows a search inside a scope. Tile is a tile of the scope's env
// (slug or id); Since defaults to 1h; Before pages: only lines older than it.
type LogsIn struct {
	Tile, Kind, Source, Stream, Run, Text string
	Since                                 time.Duration
	Before                                time.Time
}

// LogLine is one stored log line. Access lines carry their fields in HTTP
// ("http.method", "http.uri", "http.status", "http.duration_ms", ...).
type LogLine struct {
	Time      time.Time         `json:"time"`
	Msg       string            `json:"msg"`
	Org       string            `json:"org"`
	Stack     string            `json:"stack"`
	Env       string            `json:"env"`
	Tile      string            `json:"tile"`
	TileID    string            `json:"tile_id"`
	Kind      string            `json:"kind"`
	Node      string            `json:"node,omitempty"`
	Container string            `json:"container,omitempty"`
	Stream    string            `json:"stream,omitempty"`
	Source    string            `json:"source"`
	RunID     string            `json:"run_id,omitempty"`
	DeployID  string            `json:"deploy_id,omitempty"`
	HTTP      map[string]string `json:"http,omitempty"`
}

// SearchLogs is the scope's stored lines, newest first, LogsPage at most. The
// org (and the stack, env and tile the scope has) come from s, which the
// caller resolved and authorized, never from in.
func (o *Orchestrator) SearchLogs(ctx context.Context, s Scope, in LogsIn) ([]LogLine, error) {
	if s.Org == nil {
		return nil, logs.ErrNoOrg
	}
	if !o.LogsOn() {
		return nil, errs.Refusedf("log history is off")
	}
	f := logs.Filter{Org: s.Org.ID, Kind: in.Kind, Source: in.Source, Stream: in.Stream, Run: in.Run, Text: in.Text}
	if s.Stack != nil {
		f.Stack = s.Stack.ID
	}
	if s.Env != nil {
		f.Env = s.Env.ID
	}
	switch {
	case s.Tile != nil:
		f.Tile = s.Tile.ID
	case in.Tile != "" && s.Env != nil:
		t, err := o.tiles.GetBySlug(ctx, s.Env.ID, in.Tile)
		if err != nil {
			if t, err = o.tiles.Get(ctx, in.Tile); err != nil || t.EnvironmentID != s.Env.ID {
				return nil, errs.Invalidf("tile", "no such tile here")
			}
		}
		f.Tile = t.ID
	}
	if in.Source != "" && !slices.Contains([]string{logs.SourceApp, logs.SourceAccess, logs.SourceRun, logs.SourceBuild}, in.Source) {
		return nil, errs.Invalidf("source", "must be app, access, run or build")
	}
	if in.Stream != "" && in.Stream != "stdout" && in.Stream != "stderr" {
		return nil, errs.Invalidf("stream", "must be stdout or stderr")
	}
	since := in.Since
	switch {
	case since == 0:
		since = time.Hour
	case since < 0 || since > LogsMaxSince:
		return nil, errs.Invalidf("since", "must be up to 168h (7d)")
	}
	now := time.Now()
	from, to := now.Add(-since), now.Add(time.Second)
	if !in.Before.IsZero() {
		// ponytail: lines sharing the oldest shown nanosecond are skipped by
		// the next page; add a tiebreak cursor if that ever shows up.
		to = in.Before.Add(-time.Nanosecond)
	}
	ls, err := o.logStore().Query(ctx, f, from, to, LogsPage)
	if err != nil {
		return nil, err
	}
	out := make([]LogLine, len(ls))
	for i, l := range ls {
		out[i] = LogLine{
			Time: l.Time, Msg: l.Msg, Org: l.Org, Stack: l.Stack, Env: l.Env, Tile: l.Tile, TileID: l.TileID,
			Kind: l.Kind, Node: l.Node, Container: l.Container, Stream: l.Stream, Source: l.Source,
			RunID: l.RunID, DeployID: l.DeployID, HTTP: l.HTTP,
		}
	}
	return out, nil
}

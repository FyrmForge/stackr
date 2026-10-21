package graph

import (
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

func TestServiceWord(t *testing.T) {
	deployFailed := &store.Job{Kind: "deploy", State: "failed"}
	for _, c := range []struct {
		name string
		word string // tile.State's container word
		up   int
		job  *store.Job
		want string
	}{
		{"all replicas up", "running", 3, nil, "running"},
		{"some replicas down", "degraded", 2, nil, "degraded"},
		{"a replica unhealthy", "unhealthy", 3, nil, "unhealthy"},
		{"failed deploy, old version runs", "running", 1, deployFailed, "degraded"},
		{"failed deploy, old version partly up", "degraded", 1, deployFailed, "degraded"},
		{"failed deploy, old version unhealthy", "unhealthy", 1, deployFailed, "unhealthy"},
		{"failed deploy, nothing runs", "stopped", 0, deployFailed, "error"},
		{"failed deploy, never ran", "none", 0, deployFailed, "error"},
		{"failed backup over a healthy tile", "running", 1, &store.Job{Kind: "backup", State: "failed"}, "running"},
		{"failed env-wide promote over a running tile", "running", 1, &store.Job{Kind: "promote", State: "failed"}, "running"},
		{"deploy in flight", "running", 1, &store.Job{Kind: "deploy", State: "running"}, "building"},
		{"deploy done", "running", 1, &store.Job{Kind: "deploy", State: "done"}, "running"},
	} {
		got := c.word
		if c.job != nil {
			got = LayerJob(c.word, c.up, false, *c.job)
		}
		if got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestRunWord(t *testing.T) {
	runs := func(ss ...string) []store.Run { // newest first
		rs := make([]store.Run, len(ss))
		for i, s := range ss {
			rs[i].Status = s
		}
		return rs
	}
	for _, c := range []struct {
		name string
		rs   []store.Run
		want string
	}{
		{"never run", nil, "none"},
		{"last run ok", runs("ok", "failed"), "done"},
		{"one failure", runs("failed"), "degraded"},
		{"one failure after a success", runs("failed", "ok"), "degraded"},
		{"two failures, fewer than three runs", runs("failed", "failed"), "degraded"},
		{"three failures in a row", runs("failed", "failed", "failed", "ok"), "error"},
		{"two failures then a success", runs("ok", "failed", "failed"), "done"},
		{"success breaks the streak", runs("failed", "failed", "ok", "failed"), "degraded"},
		{"a cancel is not a link", runs("failed", "cancelled", "failed", "failed"), "error"},
		{"only cancelled", runs("cancelled"), "none"},
		{"running after failures", runs("running", "failed", "failed", "failed"), "running"},
	} {
		if got := RunWord(c.rs); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestRunKindJob(t *testing.T) {
	for _, c := range []struct {
		name string
		word string
		up   int
		job  store.Job
		want string
	}{
		{"its failed run job defers to the runs", "degraded", 1, store.Job{Kind: "run", State: "failed"}, "degraded"},
		{"its live run job defers to the runs", "running", 1, store.Job{Kind: "run", State: "running"}, "running"},
		{"failed redeploy, an older build fires", "done", 1, store.Job{Kind: "deploy", State: "failed"}, "degraded"},
		{"failed first deploy", "none", 0, store.Job{Kind: "deploy", State: "failed"}, "error"},
	} {
		if got := LayerJob(c.word, c.up, true, c.job); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// The env is its worst tile: one failed cron run is amber, not Down.
func TestEnvWorstTile(t *testing.T) {
	fold := func(ws ...string) string {
		w := ""
		for _, x := range ws {
			w = worse(w, x)
		}
		return w
	}
	for _, c := range []struct {
		tiles []string
		want  string
	}{
		{[]string{"running", RunWord([]store.Run{{Status: "failed"}})}, "degraded"},
		{[]string{"running", "degraded", "error"}, "error"},
		{[]string{"done", "running", "unhealthy"}, "unhealthy"},
		{[]string{"running", "done"}, "running"},
		{nil, ""},
	} {
		if got := fold(c.tiles...); got != c.want {
			t.Errorf("%v: %q, want %q", c.tiles, got, c.want)
		}
	}
}

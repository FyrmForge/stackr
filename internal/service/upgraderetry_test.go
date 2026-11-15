package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// A failing check (GitHub down, rate limited, box offline) is retried on
// every 30s tick: nothing remembers the attempt.
func TestAutoUpgradeCheckFailedWaits(t *testing.T) {
	w := newWorld(t)
	var hits atomic.Int32
	gh := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		rw.WriteHeader(http.StatusForbidden) // what a rate-limited GitHub answers
	}))
	t.Cleanup(gh.Close)
	w.orch.upgrade.URL, w.orch.upgrade.Version = gh.URL, "v0.8.22"
	for range 10 { // ten ticks = five minutes
		w.orch.autoUpgradeCheck(context.Background())
	}
	if hits.Load() != 1 {
		t.Fatalf("10 ticks after a failed check: %d requests, want 1", hits.Load())
	}
}

// A release whose digest asset is not up yet answers "" with no error; the
// check is stamped as done, so the release stays hidden for a whole interval.
func TestCheckUpgradeEmptyAnswerNotStamped(t *testing.T) {
	w := newWorld(t)
	var hits atomic.Int32
	gh := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = rw.Write([]byte(`{"tag_name":"v0.9.0"}`))
	}))
	t.Cleanup(gh.Close)
	dg := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(dg.Close)
	w.orch.upgrade.URL, w.orch.upgrade.Version, w.orch.upgrade.DigestFmt = gh.URL, "v0.8.22", dg.URL+"/%s"
	ctx := context.Background()
	w.orch.autoUpgradeCheck(ctx)
	if at := w.orch.UpgradeCheckedAt(ctx); !at.IsZero() {
		t.Fatalf("no release was learned, yet the check is stamped done at %s (hits %d)", at, hits.Load())
	}
}

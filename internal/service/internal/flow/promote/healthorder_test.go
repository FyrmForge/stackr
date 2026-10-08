package promote

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/release"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// waitWorld is prd holding a running, never healthy db and an api that
// waits on db:healthy and was never deployed; the release pins both and prd
// already runs it, so a re-promote has only api to roll out. Every poll of
// the wait calls onSleep.
func waitWorld(t *testing.T, onSleep func(*world)) (*world, store.Release, store.Tile) {
	w := syncWorld(t)
	db := w.mk(t, w.prd, "db", imgTile("nginx:1", 80))
	row := imgTile("nginx:1", 80)
	row.DependsOn = "db:healthy"
	api := w.mk(t, w.prd, "api", row)
	w.fake.Containers = append(w.fake.Containers, docker.Container{
		ID: "db1", State: "running", Health: "starting",
		Labels: map[string]string{tile.LabelTile: db.ID, tile.LabelRole: "replica"},
	})
	r := w.release(t, "",
		release.Pin{Slug: "db", Repo: "nginx", Digest: "sha256:one"},
		release.Pin{Slug: "api", Repo: "nginx", Digest: "sha256:one"})
	_, err := w.f.D.Envs.SetRelease(ctx, w.dev, r.ID)
	must(t, err)
	_, err = w.f.D.Envs.SetRelease(ctx, w.prd, r.ID)
	must(t, err)
	w.f.D.DepSleep = func(context.Context, time.Duration) error { onSleep(w); return nil }
	return w, r, api
}

// The wait for db:healthy happens before the swap, so the job can still be
// stopped or superseded while it waits.
func TestPromoteWaitsBeforeTheSwap(t *testing.T) {
	swapped := false
	var swappedDuringWait bool
	w, r, _ := waitWorld(t, func(*world) { swappedDuringWait = swappedDuringWait || swapped })
	_, err := w.f.Apply(ctx, w.prd.ID, r.ID, io.Discard, func() error { swapped = true; return nil })
	if err == nil {
		t.Fatal("want the dependency wait to fail")
	}
	if swappedDuringWait {
		t.Error("the job was marked swapping while it still waited")
	}
}

// A dependency failure leaves the env on the new release; the error says so.
func TestPromoteDependencyFailureSaysPartlyRolledOut(t *testing.T) {
	w, r, _ := waitWorld(t, func(*world) {})
	_, err := w.f.Apply(ctx, w.prd.ID, r.ID, io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "partly rolled out") ||
		!strings.Contains(err.Error(), "promote again") {
		t.Fatalf("err = %v, want it to say the env is partly rolled out", err)
	}
	if !strings.Contains(err.Error(), "waited for db to be healthy") {
		t.Errorf("err = %v, want the dependency's own reason kept", err)
	}
}

// The same wait in a parked sync's rollout: it waits unswapped, then swaps
// right before the tile's containers change.
func TestSyncRolloutWaitsBeforeTheSwap(t *testing.T) {
	swapped := false
	var swappedDuringWait bool
	w, _, api := waitWorld(t, func(w *world) {
		swappedDuringWait = swappedDuringWait || swapped
		w.fake.Containers[len(w.fake.Containers)-1].Health = "healthy"
	})
	done, err := w.f.SyncRollout(ctx, []string{api.ID}, io.Discard, func() error { swapped = true; return nil })
	if err != nil || len(done.Deployed) != 1 {
		t.Fatalf("rollout = %+v, %v", done, err)
	}
	if swappedDuringWait {
		t.Error("the job was marked swapping while it still waited")
	}
	if !swapped {
		t.Error("the job was never marked swapping before its containers changed")
	}
}

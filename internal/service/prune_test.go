package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// The daily prune takes finished jobs and their logs past 30 days, stale logs,
// security events past 90 days and dead join tokens; newer ones stay.
func TestPruneOld(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	o := w.orch
	logs := filepath.Join(o.cfg.DataDir, "jobs")
	must(t, os.MkdirAll(logs, 0o750))
	mk := func(id string, ago time.Duration) {
		fin := time.Now().Add(-ago)
		p := filepath.Join(logs, id+".log")
		must(t, os.WriteFile(p, []byte("x"), 0o600))
		must(t, w.st.Jobs.Create(ctx, store.Job{ID: id, Kind: "deploy", State: "done", LockSet: store.StringList{}, Payload: "{}",
			LogPath: p, CreatedAt: fin, FinishedAt: &fin}))
	}
	mk("old", 40*24*time.Hour)
	mk("new", 2*24*time.Hour)
	stale := filepath.Join(logs, "stray.log")
	must(t, os.WriteFile(stale, []byte("x"), 0o600))
	old := time.Now().Add(-40 * 24 * time.Hour)
	must(t, os.Chtimes(stale, old, old))
	must(t, w.st.SecurityEvents.Create(ctx, store.SecurityEvent{ID: "e-old", At: time.Now().Add(-100 * 24 * time.Hour), Action: "x"}))
	must(t, w.st.SecurityEvents.Create(ctx, store.SecurityEvent{ID: "e-new", At: time.Now().Add(-10 * 24 * time.Hour), Action: "x"}))

	o.pruneOld(ctx)

	if _, err := w.st.Jobs.Get(ctx, "old"); err == nil {
		t.Error("old job kept")
	}
	if _, err := w.st.Jobs.Get(ctx, "new"); err != nil {
		t.Errorf("new job: %v", err)
	}
	for p, want := range map[string]bool{filepath.Join(logs, "old.log"): false, filepath.Join(logs, "new.log"): true, stale: false} {
		if _, err := os.Stat(p); (err == nil) != want {
			t.Errorf("%s exists = %v, want %v", filepath.Base(p), err == nil, want)
		}
	}
	evs, err := w.st.SecurityEvents.Find(ctx, store.SecurityFilter{Action: "x"})
	must(t, err)
	if len(evs) != 1 || evs[0].ID != "e-new" {
		t.Errorf("security events = %+v", evs)
	}
	if err := w.st.SecurityEvents.DeleteBefore(ctx, time.Now()); err == nil {
		t.Error("security_events deletes without the prune row")
	}
	if _, ok, _ := w.st.Settings.Get(ctx, store.SecurityEventsPruning); ok {
		t.Error("the prune row stayed")
	}
}

// Move temp files left by a crash go at boot; other files stay.
func TestSweepMoveTemps(t *testing.T) {
	w := newWorld(t)
	d := w.orch.cfg.DataDir
	dir := filepath.Join(d, "rsync-build-123")
	must(t, os.MkdirAll(dir, 0o700))
	must(t, os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o600))
	tar := filepath.Join(d, "push-456.tar")
	keep := filepath.Join(d, "keep.tar")
	must(t, os.WriteFile(tar, []byte("x"), 0o600))
	must(t, os.WriteFile(keep, []byte("x"), 0o600))
	w.orch.sweepMoveTemps()
	for p, want := range map[string]bool{dir: false, tar: false, keep: true} {
		if _, err := os.Stat(p); (err == nil) != want {
			t.Errorf("%s exists = %v, want %v", filepath.Base(p), err == nil, want)
		}
	}
}

// A pulled image no tile names any more goes with its row; one a tile still
// names stays.
func TestPrunePulledImages(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	for _, ref := range []string{"nginx:old", "redis:7"} {
		_, err := w.orch.images.Checked(ctx, ref, "sha256:"+ref, "", nil)
		must(t, err)
		w.fake.Images = append(w.fake.Images, docker.Image{ID: "sha256:" + ref, Tags: []string{ref}, Size: 10})
	}
	tl := w.tile(t, "cache", false)
	tl.ImageRef = "redis:7"
	must(t, w.st.Tiles.Update(ctx, tl))

	w.orch.prunePulledImages(ctx, func(string, int, string, error) {})

	rows, err := w.st.Images.List(ctx)
	must(t, err)
	if len(rows) != 1 || rows[0].Ref != "redis:7" {
		t.Errorf("rows = %+v", rows)
	}
	removed := map[string]bool{}
	for _, c := range w.fake.Calls() {
		if c.Method == "RemoveImage" {
			removed[c.Args[0]] = true
		}
	}
	if !removed["nginx:old"] || removed["redis:7"] {
		t.Errorf("removed = %v", removed)
	}
}

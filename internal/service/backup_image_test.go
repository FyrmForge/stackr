package service

import (
	"context"
	"slices"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	bk "github.com/FyrmForge/stackr/internal/service/internal/leaf/backup"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
)

func TestImageTag(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"localhost:5000/o/pg:17.11-ext-amd64-0a1b2c3d": "17.11-ext-amd64-0a1b2c3d",
		"localhost:5000/o/pg@sha256:ff":                "ff",
		"pg":                                           "pg",
	} {
		if got := imageTag(in); got != want {
			t.Errorf("imageTag(%q) = %q, want %q", in, got, want)
		}
	}
}

// A base backup of a recipe-built instance saves its image next to the base
// and records the key on the run; one of an instance that is not recipe-built
// saves nothing and records none.
func TestBaseBackupSavesTheImageOfARecipeBuiltInstance(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	pg, err := w.orch.CreateOperatedTile(ctx, Tile{EnvironmentID: w.env, Name: "pg"}, "postgres", 0)
	must(t, err)
	_, dj, err := w.orch.SetPITR(ctx, pg.ID, 3)
	must(t, err)
	// the redeploy it queues declares the volumes (and fails on the fake's
	// health check, which does not matter here); wait for it to finish
	w.wait(t, dj.ID)
	data, err := w.orch.dataVolume(ctx, pg)
	must(t, err)
	w.fake.Containers = []docker.Container{{
		ID: "c-pg", Name: "pg", State: "running",
		Labels: map[string]string{tile.LabelTile: pg.ID, tile.LabelRole: "replica"},
	}}
	w.fake.ExecFn = func(cmd []string) string {
		if slices.Contains(cmd, "pg_walfile_name(pg_current_wal_lsn())") || len(cmd) > 0 && cmd[0] == "psql" {
			return "000000010000000000000003\n"
		}
		return ""
	}
	run := func() BackupRun {
		t.Helper()
		j, err := w.orch.BackupNow(ctx, data.ID, "", "base", "")
		must(t, err)
		if j = w.wait(t, j.ID); j.State != job.Done {
			t.Fatalf("base backup: %s %q", j.State, j.Error)
		}
		rs, err := w.orch.BackupRuns(ctx, data.ID)
		must(t, err)
		return rs[0]
	}
	if r := run(); r.ImageKey != "" {
		t.Errorf("not recipe-built, image key %q", r.ImageKey)
	}
	m, err := w.orch.operated.GetByTile(ctx, pg.ID)
	must(t, err)
	must(t, w.st.OperatedInstances.SetBuild(ctx, m.ID, m.Preload, m.Extensions, `{"base":"b","ref":"r"}`))
	must(t, w.st.Tiles.SetImageRef(ctx, pg.ID, "localhost:5000/o/pg:17.11-ext-amd64-0a1b2c3d"))
	want := bk.ImageKey(w.org, data.ID, "17.11-ext-amd64-0a1b2c3d")
	if r := run(); r.ImageKey != want {
		t.Errorf("image key %q, want %q", r.ImageKey, want)
	}
}

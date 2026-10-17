package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/backup"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// The panel-backup job and run that wrote the restored archive finish done
// with a note on the first boot after a restore; other running jobs are
// left for the runner, and with no marker nothing is touched.
func TestSettleRestoredPanelBackup(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	running := func(kind jobs.Kind) store.Job {
		j := store.Job{ID: uuid.NewString(), Kind: string(kind), State: job.Running, LockSet: store.StringList{"panel"}, Payload: "{}", CreatedAt: time.Now()}
		must(t, w.st.Jobs.Create(ctx, j))
		return j
	}
	local, err := w.orch.localDest(ctx)
	must(t, err)
	pb, other := running(kindPanelBackup), running(kindDeploy)
	run, err := w.orch.backups.Start(ctx, backup.KindPanel, nil, nil, local.ID, "manual")
	must(t, err)

	w.orch.settleRestored(ctx) // no marker
	if j, _ := w.st.Jobs.Get(ctx, pb.ID); j.State != job.Running {
		t.Fatalf("no marker: panel backup job is %s", j.State)
	}
	must(t, os.WriteFile(filepath.Join(w.orch.cfg.DataDir, installspec.RestoredMarker), nil, 0o600))
	w.orch.settleRestored(ctx)
	if j, _ := w.st.Jobs.Get(ctx, pb.ID); j.State != job.Done || j.Error != restoredNote {
		t.Errorf("panel backup job = %s %q", j.State, j.Error)
	}
	if j, _ := w.st.Jobs.Get(ctx, other.ID); j.State != job.Running {
		t.Errorf("deploy job = %s, want it left running for the runner", j.State)
	}
	if r, _ := w.orch.backups.GetRun(ctx, run.ID); r.Status != backup.Done || r.Error != restoredNote {
		t.Errorf("panel run = %s %q", r.Status, r.Error)
	}
}

// A backup run a restart left running fails at boot; a finished one stays.
func TestBackupRunsInterrupted(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	local, err := w.orch.localDest(ctx)
	must(t, err)
	open, err := w.orch.backups.Start(ctx, backup.KindPanel, nil, nil, local.ID, "manual")
	must(t, err)
	shut, err := w.orch.backups.Start(ctx, backup.KindPanel, nil, nil, local.ID, "manual")
	must(t, err)
	_, err = w.orch.backups.Finish(ctx, shut, "k", 1, nil)
	must(t, err)
	must(t, w.orch.backups.Interrupted(ctx))
	if r, _ := w.orch.backups.GetRun(ctx, open.ID); r.Status != backup.Failed || r.FinishedAt == nil {
		t.Errorf("left-running run = %s", r.Status)
	}
	if r, _ := w.orch.backups.GetRun(ctx, shut.ID); r.Status != backup.Done {
		t.Errorf("finished run = %s", r.Status)
	}
}

// The first boot after a restore deploys the controller's tiles that ran and
// lost their containers, once: a running one, a stopped one, a never
// deployed one and a worker's are left alone.
func TestRedeployAfterRestore(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	done := func(tl Tile, kind jobs.Kind, at time.Time) {
		t.Helper()
		must(t, w.st.Jobs.Create(ctx, store.Job{
			ID: uuid.NewString(), Kind: string(kind), State: job.Done, LockSet: store.StringList{tl.ID},
			Payload: "{}", CreatedAt: at,
		}))
	}
	old := time.Now().Add(-time.Hour)
	lost := w.tile(t, "lost", false)
	done(lost, kindDeploy, old)
	stopped := w.tile(t, "stopped", false)
	done(stopped, kindDeploy, old)
	done(stopped, kindStop, old.Add(time.Minute))
	never := w.tile(t, "never", false)
	up := w.tile(t, "up", true)
	done(up, kindDeploy, old)

	must(t, w.st.Nodes.Create(ctx, store.Node{
		ID: "w1", Name: "w1", Slug: "w1", Role: node.Worker, Tags: "[]", RangeCIDR: "10.230.0.0/16",
		Status: node.StatusOnline, CreatedAt: time.Now(),
	}))
	far := uuid.NewString()
	must(t, w.st.Environments.Create(ctx, store.Environment{
		NodeID: "w1", ID: far, StackID: w.stack, Name: "far", Slug: "far", Type: "static",
		Settings: "{}", Network: "f", FromKind: "branch", FromBranch: "main", CreatedAt: time.Now(),
	}))
	remote, err := w.orch.CreateTile(ctx, Tile{
		StackID: w.stack, EnvironmentID: far, Name: "remote", Kind: "image", ImageRef: "nginx:1", ContainerPort: 80,
	})
	must(t, err)
	done(remote, kindDeploy, old)

	marker := filepath.Join(w.orch.cfg.DataDir, installspec.RestoredMarker)
	must(t, os.WriteFile(marker, nil, 0o600))
	w.orch.redeployRestored(ctx)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("marker still there: %v", err)
	}
	deploys := func(tl Tile) int {
		js, err := w.orch.TileJobs(ctx, []string{tl.ID}, 10)
		must(t, err)
		n := 0
		for _, j := range js {
			if j.Kind == string(kindDeploy) && j.CreatedAt.After(old.Add(time.Minute)) {
				n++
			}
		}
		return n
	}
	if n := deploys(lost); n != 1 {
		t.Errorf("lost tile: %d new deploys, want 1", n)
	}
	for _, tl := range []Tile{stopped, never, up, remote} {
		if n := deploys(tl); n != 0 {
			t.Errorf("%s: %d new deploys, want 0", tl.Slug, n)
		}
	}
	w.orch.redeployRestored(ctx) // no marker: nothing more
	if n := deploys(lost); n != 1 {
		t.Errorf("second boot: %d deploys of the lost tile, want 1", n)
	}
}

// After a restore the lost volumes come back before their apps start: the
// tar restore is queued first and holds the tiles, the managed instance
// deploys before its dump is loaded, and the app's deploy waits behind both.
// A volume with no backup starts empty and says so; one that kept its data
// is left alone.
func TestRestoreVolumesBeforeDeploy(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	old := time.Now().Add(-time.Hour)
	ran := func(tl Tile) {
		t.Helper()
		must(t, w.st.Jobs.Create(ctx, store.Job{
			ID: uuid.NewString(), Kind: string(kindDeploy), State: job.Done, LockSet: store.StringList{tl.ID},
			Payload: "{}", CreatedAt: old,
		}))
	}
	app := w.tile(t, "app", false)
	ran(app)
	pg := store.Tile{
		ID: uuid.NewString(), StackID: w.stack, EnvironmentID: w.env, Name: "pg", Slug: "pg", Kind: tile.Managed,
		EnvJSON: "{}", BuildArgs: "{}", Replicas: 1, UpdatePolicy: "manual", CreatedAt: old, UpdatedAt: old,
	}
	must(t, w.st.Tiles.Create(ctx, pg))
	ran(pg)
	must(t, w.st.ManagedInstances.Create(ctx, store.ManagedInstance{ID: "mi", TileID: pg.ID, Engine: "postgres", CreatedAt: old}))
	must(t, w.st.BackupDests.Create(ctx, store.BackupDest{ID: "d", Kind: "s3", Name: "d", Endpoint: "e", Bucket: "b", AccessKey: "a", SecretKey: "s", CreatedAt: old}))
	mi := "mi"
	vol := func(slug string, inst *string, key string) store.Volume {
		t.Helper()
		v := store.Volume{ID: "v-" + slug, ScopeKind: "env", ScopeID: w.env, InstanceID: inst, Slug: slug, Name: "stackr-vol-" + slug, CreatedAt: old}
		must(t, w.st.Volumes.Create(ctx, v))
		if key != "" {
			must(t, w.st.BackupRuns.Create(ctx, store.BackupRun{ID: "r-" + slug, Kind: "volume", VolumeID: &v.ID, DestID: "d", Status: backup.Done, ObjectKey: key, CreatedAt: old}))
		}
		return v
	}
	vol("data", nil, "data.tar.gz.age")
	// a later pre-restore run is the empty volume a restore took: never picked
	vd := "v-data"
	must(t, w.st.BackupRuns.Create(ctx, store.BackupRun{ID: "r-pre", Kind: "volume", VolumeID: &vd, DestID: "d", Status: backup.Done, Trigger: backup.PreRestore, ObjectKey: "pre.tar.gz.age", CreatedAt: old.Add(time.Minute)}))
	vol("db", &mi, "db.sql.gz.age")
	vol("cache", nil, "")
	vol("kept", nil, "kept.tar.gz.age")
	w.fake.Volumes = append(w.fake.Volumes, docker.VolumeInfo{Name: "stackr-vol-kept", SizeBytes: 10})

	marker := filepath.Join(w.orch.cfg.DataDir, installspec.RestoredMarker)
	must(t, os.WriteFile(marker, nil, 0o600))
	w.orch.redeployRestored(ctx)

	js, err := w.st.Jobs.ListByState(ctx)
	must(t, err)
	slices.Reverse(js) // oldest first
	at := func(kind jobs.Kind, payload string) int {
		return slices.IndexFunc(js, func(j store.Job) bool {
			return j.Kind == string(kind) && j.CreatedAt.After(old) && strings.Contains(j.Payload, payload)
		})
	}
	data, db, kept := at(kindRestore, "v-data"), at(kindRestore, "v-db"), at(kindRestore, "v-kept")
	dpg, dapp := at(kindDeploy, pg.ID), at(kindDeploy, app.ID)
	switch {
	case data < 0 || db < 0 || dpg < 0 || dapp < 0:
		t.Fatalf("restore data %d, restore db %d, deploy pg %d, deploy app %d among %d jobs", data, db, dpg, dapp, len(js))
	case kept >= 0:
		t.Errorf("the volume that kept its data was restored")
	case at(kindRestore, "v-cache") >= 0:
		t.Errorf("a volume with no backup was restored")
	case data > dpg || dpg > db || db > dapp:
		t.Errorf("order: restore data %d, deploy pg %d, restore db %d, deploy app %d", data, dpg, db, dapp)
	case !strings.Contains(js[data].Payload, `"r-data"`):
		t.Errorf("data restores %s, want run r-data", js[data].Payload)
	case !slices.Contains(js[data].LockSet, app.ID) || !slices.Contains(js[db].LockSet, app.ID):
		t.Errorf("restores lock %v and %v, not the app %s", js[data].LockSet, js[db].LockSet, app.ID)
	}
	var p tileJob
	must(t, json.Unmarshal([]byte(js[dapp].Payload), &p))
	if !strings.Contains(p.Note, "no backup for cache; it starts empty") || !strings.Contains(p.Note, "kept holds data") {
		t.Errorf("deploy note = %q", p.Note)
	}
	evs, err := w.orch.SecurityLog(ctx, SecurityFilter{Action: auditEnvRestore, TargetID: w.env})
	must(t, err)
	if len(evs) != 1 || !strings.Contains(evs[0].Detail, "no backup for cache; it starts empty") {
		t.Errorf("security log = %+v", evs)
	}
}

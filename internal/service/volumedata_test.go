package service

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/backup"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// dataVol declares an env volume with a cap, a schedule and a Docker volume.
func (w *world) dataVol(t *testing.T) Volume {
	t.Helper()
	ctx := context.Background()
	v, _, err := w.orch.DeclareEnvVolume(ctx, w.env, "data")
	must(t, err)
	mb := int64(512)
	must(t, w.orch.SetVolumeCap(ctx, v.ID, &mb))
	_, err = w.orch.AddBackupSchedule(ctx, v.ID, ScheduleSpec{Cron: "@daily", Keep: 3})
	must(t, err)
	_, err = w.orch.volumes.Ensure(ctx, v)
	must(t, err)
	w.fake.Volumes = append(w.fake.Volumes, docker.VolumeInfo{Name: v.Name, SizeBytes: 10})
	w.fake.TarOut = []byte("tar bytes")
	return v
}

// mountedBy makes a tile that mounts the volume; running adds a live replica.
func (w *world) mountedBy(t *testing.T, name string, running bool) Tile {
	t.Helper()
	tl := w.tile(t, name, false)
	tl, _, err := w.orch.UpdateTile(context.Background(), tl.ID, func(x *Tile) error { x.Volumes = "data:/data"; return nil })
	must(t, err)
	if running {
		w.fake.Containers = append(w.fake.Containers, docker.Container{
			ID: "c-" + name, Name: "c-" + name, State: "running",
			Labels: map[string]string{tile.LabelTile: tl.ID, tile.LabelRole: "replica"},
		})
	}
	return tl
}

func (w *world) callsFrom(from int, methods ...string) []string {
	var out []string
	for _, c := range w.fake.Calls()[from:] {
		if slices.Contains(methods, c.Method) {
			out = append(out, c.Method+" "+strings.Join(c.Args, ","))
		}
	}
	return out
}

// sharedS3 is an off-box destination every org sees. Make the volume first:
// a destination that exists makes Declare add its default schedule.
func (w *world) offBoxS3(t *testing.T, name, url string) BackupDest {
	t.Helper()
	d, err := w.orch.CreateBackupDest(context.Background(), nil, BackupDestSpec{
		Name: name, Endpoint: url, Bucket: "b", AccessKey: "a", SecretKey: "s", Shared: true,
	})
	must(t, err)
	return d
}

func (w *world) deadS3(t *testing.T) BackupDest {
	t.Helper()
	_, srv := newFakeS3(t)
	d := w.offBoxS3(t, "dead", srv.URL)
	srv.Close()
	return d
}

func notFound(err error) bool { return errors.Is(err, errs.ErrNotFound) }

// A delete is immediate: row, Docker volume and schedules go with no orphan
// row; the run rows stay without a volume and their archive stays put.
func TestDeleteVolumeIsImmediate(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	s3, srv := newFakeS3(t)
	v := w.dataVol(t)
	d := w.offBoxS3(t, "offsite", srv.URL)
	ss, err := w.orch.BackupSchedules(ctx, v.ID)
	must(t, err)
	r, err := w.orch.backups.Start(ctx, backup.KindVolume, &v.ID, nil, d.ID, "manual")
	must(t, err)
	_, err = w.orch.backups.Finish(ctx, r, "stackr/o/v/manual/x.age", 1, nil)
	must(t, err)
	s3.objs["stackr/o/v/manual/x.age"] = []byte("archive")
	alerts := []string{staleKey(v.ID), "alert.backup.schedule." + ss[0].ID}
	for _, k := range alerts {
		must(t, w.st.Settings.Set(ctx, k, "1"))
	}
	before := len(w.fake.Calls())

	j, err := w.orch.DeleteVolume(ctx, v.ID, false)
	if err != nil || j.ID != "" {
		t.Fatalf("delete = %+v, %v; want done inline", j, err)
	}
	if _, err := w.orch.Volume(ctx, v.ID); !notFound(err) {
		t.Errorf("volume row after delete = %v, want gone (no orphan)", err)
	}
	if got := w.callsFrom(before, "RemoveVolume"); len(got) != 1 || got[0] != "RemoveVolume "+v.Name {
		t.Errorf("removed = %v", got)
	}
	if ss, _ := w.st.BackupSchedules.ListByVolume(ctx, v.ID); len(ss) != 0 {
		t.Errorf("%d schedules left", len(ss))
	}
	for _, k := range alerts {
		if _, ok, _ := w.st.Settings.Get(ctx, k); ok {
			t.Errorf("alert %s left behind", k)
		}
	}
	got, err := w.st.BackupRuns.Get(ctx, r.ID)
	if err != nil || got.VolumeID != nil || got.ObjectKey == "" {
		t.Errorf("run after delete = %+v, %v; want kept without a volume", got, err)
	}
	if _, ok := s3.objs["stackr/o/v/manual/x.age"]; !ok {
		t.Error("the archive left the destination")
	}
}

// Every refusal leaves the volume, its data and its schedules alone.
func TestDeleteVolumeRefusals(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	v := w.dataVol(t)
	intact := func(why string) {
		t.Helper()
		if _, err := w.orch.Volume(ctx, v.ID); err != nil {
			t.Errorf("%s: volume = %v", why, err)
		}
		if ss, _ := w.orch.BackupSchedules(ctx, v.ID); len(ss) != 1 {
			t.Errorf("%s: %d schedules", why, len(ss))
		}
		if got := w.callsFrom(0, "RemoveVolume"); len(got) != 0 {
			t.Errorf("%s: removed %v", why, got)
		}
	}

	w.mountedBy(t, "api", false)
	for _, bf := range []bool{false, true} {
		if _, err := w.orch.DeleteVolume(ctx, v.ID, bf); !conflict(err) {
			t.Errorf("mounted (backup %v) = %v", bf, err)
		}
	}
	intact("mounted")

	// a job that holds the volume
	_, err := w.orch.jobRows.Create(ctx, "backup", []string{"volume:" + v.ID}, "{}", nil, "")
	must(t, err)
	if _, err := w.orch.DeleteVolume(ctx, "nope", false); !notFound(err) {
		t.Errorf("unknown volume = %v", err)
	}
	// detach the mounter so the job is the refusal
	tl, err := w.orch.Tiles(ctx, w.env)
	must(t, err)
	for _, x := range tl {
		_, _, err := w.orch.UpdateTile(ctx, x.ID, func(t *Tile) error { t.Volumes = ""; return nil })
		must(t, err)
	}
	if _, err := w.orch.DeleteVolume(ctx, v.ID, false); !conflict(err) || !strings.Contains(err.Error(), "busy") {
		t.Errorf("busy = %v", err)
	}
	intact("busy")

	// an old copy of a move
	hold := moveHold
	ov := v
	ov.OrphanedAt = &hold
	must(t, w.st.Volumes.Update(ctx, ov))
	if _, err := w.orch.DeleteVolume(ctx, v.ID, false); !conflict(err) {
		t.Errorf("old copy = %v", err)
	}
	if got := w.callsFrom(0, "RemoveVolume"); len(got) != 0 {
		t.Errorf("old copy: removed %v", got)
	}
}

// An operated instance's volume cannot be deleted by any route.
func TestDeleteVolumeRefusesInstanceVolume(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	_, _, v := w.pg(t, "pg")
	for _, bf := range []bool{false, true} {
		if _, err := w.orch.DeleteVolume(ctx, v.ID, bf); !conflict(err) || !strings.Contains(err.Error(), "operated instance") {
			t.Errorf("instance volume (backup %v) = %v", bf, err)
		}
	}
	if _, err := w.orch.RemoveEnvVolume(ctx, w.env, v.ID, false); !conflict(err) {
		t.Errorf("RemoveEnvVolume = %v", err)
	}
	if _, err := w.orch.Volume(ctx, v.ID); err != nil {
		t.Errorf("volume = %v", err)
	}
	if got := w.callsFrom(0, "RemoveVolume"); len(got) != 0 {
		t.Errorf("removed %v", got)
	}
}

// On a forgotten node only the row goes, and a backup cannot be asked for.
func TestDeleteVolumeForgottenNodeRefusesBackupFirst(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := newPMFix(t)
	must(t, n.st.ForgottenNodes.Create(ctx, store.ForgottenNode{ID: "gone", Name: "h3", ForgottenAt: time.Now()}))
	must(t, n.st.Environments.SetNode(ctx, n.edge.ID, "gone"))
	must(t, n.st.Volumes.Create(ctx, store.Volume{ID: "v-lost", ScopeKind: "env", ScopeID: n.edge.ID, Slug: "data", Name: "data", CreatedAt: time.Now()}))
	if _, err := n.orch.DeleteVolume(ctx, "v-lost", true); err == nil || !strings.Contains(err.Error(), "forgotten") {
		t.Fatalf("backup first on a forgotten node = %v", err)
	}
	if _, err := n.st.Volumes.Get(ctx, "v-lost"); err != nil {
		t.Errorf("row went anyway: %v", err)
	}
}

// With a dead destination the job fails and nothing is deleted.
func TestDeleteVolumeBackupFirstFailureDeletesNothing(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	v := w.dataVol(t)
	w.deadS3(t)
	j, err := w.orch.DeleteVolume(ctx, v.ID, true)
	must(t, err)
	if got := w.wait(t, j.ID); got.State != "failed" {
		t.Fatalf("job = %s, want failed", got.State)
	}
	if _, err := w.orch.Volume(ctx, v.ID); err != nil {
		t.Errorf("volume after a failed backup = %v", err)
	}
	if ss, _ := w.orch.BackupSchedules(ctx, v.ID); len(ss) != 1 {
		t.Errorf("%d schedules", len(ss))
	}
	if got := w.callsFrom(0, "RemoveVolume"); len(got) != 0 {
		t.Errorf("removed %v", got)
	}
}

// A good backup is a manual run kept without the volume; then the volume goes.
func TestDeleteVolumeBackupFirst(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	s3, srv := newFakeS3(t)
	v := w.dataVol(t)
	d := w.offBoxS3(t, "offsite", srv.URL)
	j, err := w.orch.DeleteVolume(ctx, v.ID, true)
	must(t, err)
	if got := w.wait(t, j.ID); got.State != "done" {
		t.Fatalf("job = %s, want done", got.State)
	}
	if _, err := w.orch.Volume(ctx, v.ID); !notFound(err) {
		t.Errorf("volume = %v, want gone", err)
	}
	rs, err := w.st.BackupRuns.ListByDest(ctx, d.ID)
	must(t, err)
	if len(rs) != 1 || rs[0].Trigger != "manual" || rs[0].VolumeID != nil || len(s3.keys(rs[0].ObjectKey)) != 1 {
		t.Errorf("runs = %+v", rs)
	}
}

// Back up first goes where the volume's schedule last went when that is
// off-box, else the first off-box destination, else the local disk.
func TestBackupFirstDest(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	v := w.dataVol(t)
	d, err := w.orch.BackupFirstDest(ctx, v.ID)
	must(t, err)
	if d.Kind != backup.Local || destArg(d) != "" {
		t.Errorf("no off-box = %+v, want local", d)
	}
	_, srvA := newFakeS3(t)
	_, srvB := newFakeS3(t)
	a := w.offBoxS3(t, "first", srvA.URL)
	b := w.offBoxS3(t, "second", srvB.URL)
	if d, _ = w.orch.BackupFirstDest(ctx, v.ID); d.ID != a.ID {
		t.Errorf("first off-box = %s, want %s", d.ID, a.ID)
	}
	ss, _ := w.orch.BackupSchedules(ctx, v.ID)
	r, err := w.orch.backups.Start(ctx, backup.KindVolume, &v.ID, &ss[0].ID, b.ID, "schedule")
	must(t, err)
	_, err = w.orch.backups.Finish(ctx, r, "k.age", 1, nil)
	must(t, err)
	if d, _ = w.orch.BackupFirstDest(ctx, v.ID); d.ID != b.ID {
		t.Errorf("last scheduled = %s, want %s", d.ID, b.ID)
	}
}

// A wipe empties the data and keeps the volume, its cap, schedules and mounts.
func TestWipeVolume(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	v := w.dataVol(t)
	tl := w.mountedBy(t, "api", false) // mounted but stopped: no switch needed
	before := len(w.fake.Calls())
	j, err := w.orch.WipeVolume(ctx, v.ID, false, false)
	must(t, err)
	if got := w.wait(t, j.ID); got.State != "done" {
		t.Fatalf("job = %s", got.State)
	}
	if got := w.callsFrom(before, "UntarVolume", "Stop", "Start"); len(got) != 1 || got[0] != "UntarVolume "+v.Name {
		t.Errorf("calls = %v", got)
	}
	zr, err := gzip.NewReader(bytes.NewReader(w.fake.Untarred))
	must(t, err)
	b, err := io.ReadAll(zr)
	must(t, err)
	if len(b) != 1024 || len(bytes.Trim(b, "\x00")) != 0 {
		t.Errorf("wipe archive = %d bytes, want 1024 zero bytes (an empty tar)", len(b))
	}
	got, err := w.orch.Volume(ctx, v.ID)
	if err != nil || got.OrphanedAt != nil || got.MaxSizeMB == nil || *got.MaxSizeMB != 512 || got.Name != v.Name {
		t.Errorf("volume after wipe = %+v, %v", got, err)
	}
	if ss, _ := w.orch.BackupSchedules(ctx, v.ID); len(ss) != 1 {
		t.Errorf("%d schedules", len(ss))
	}
	if tt, _ := w.orch.tiles.Get(ctx, tl.ID); tt.Volumes != "data:/data" {
		t.Errorf("mounts = %q", tt.Volumes)
	}
}

func TestWipeVolumeRefusals(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	v := w.dataVol(t)
	w.mountedBy(t, "api", true)
	if _, err := w.orch.WipeVolume(ctx, v.ID, false, false); !conflict(err) || !strings.Contains(err.Error(), "in use by api") {
		t.Errorf("running mounter = %v", err)
	}
	if got := w.callsFrom(0, "UntarVolume", "Stop"); len(got) != 0 {
		t.Errorf("refused wipe touched %v", got)
	}
	if _, err := w.orch.WipeVolume(ctx, "nope", true, false); !notFound(err) {
		t.Errorf("unknown = %v", err)
	}

	// an instance's volume
	_, _, iv := w.pg(t, "pg")
	if _, err := w.orch.WipeVolume(ctx, iv.ID, true, false); !conflict(err) || !strings.Contains(err.Error(), "operated instance") {
		t.Errorf("instance volume = %v", err)
	}
	// a non-env scope
	ov := store.Volume{ID: uuid.NewString(), ScopeKind: "org", ScopeID: w.org, Slug: "shared", Name: "stackr-vol-org", CreatedAt: time.Now()}
	must(t, w.st.Volumes.Create(ctx, ov))
	if _, err := w.orch.WipeVolume(ctx, ov.ID, true, false); err == nil {
		t.Error("wiped an org volume")
	}
	// an orphan
	now := time.Now()
	orph := v
	orph.OrphanedAt = &now
	must(t, w.st.Volumes.Update(ctx, orph))
	if _, err := w.orch.WipeVolume(ctx, v.ID, true, false); !conflict(err) {
		t.Errorf("orphan = %v", err)
	}
	// a held volume: a live job
	must(t, w.st.Volumes.Update(ctx, v))
	_, err := w.orch.jobRows.Create(ctx, string(kindVolumeWipe), []string{"volume:" + v.ID}, "{}", nil, "")
	must(t, err)
	if _, err := w.orch.WipeVolume(ctx, v.ID, true, false); !conflict(err) || !strings.Contains(err.Error(), "busy") {
		t.Errorf("busy = %v", err)
	}
	if got := w.callsFrom(0, "UntarVolume", "Stop"); len(got) != 0 {
		t.Errorf("refused wipes touched %v", got)
	}
}

// With the switch the running mounters stop, the wipe runs, and only the
// replicas that ran start again.
func TestWipeVolumeStopsAndRestartsTiles(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	v := w.dataVol(t)
	w.mountedBy(t, "api", true)
	w.mountedBy(t, "idle", false)
	before := len(w.fake.Calls())
	j, err := w.orch.WipeVolume(ctx, v.ID, true, false)
	must(t, err)
	if got := w.wait(t, j.ID); got.State != "done" {
		t.Fatalf("job = %s", got.State)
	}
	want := []string{"Stop c-api", "UntarVolume " + v.Name, "Start c-api"}
	if got := w.callsFrom(before, "UntarVolume", "Stop", "Start"); !slices.Equal(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
}

// Paused and restarting replicas hold the volume too: without the switch they
// refuse the wipe, with it they stop, and only the restarting one starts again.
func TestWipeVolumeCountsPausedAndRestartingReplicas(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	v := w.dataVol(t)
	for _, name := range []string{"held", "loop"} {
		tl := w.mountedBy(t, name, false)
		state := map[string]string{"held": "paused", "loop": "restarting"}[name]
		w.fake.Containers = append(w.fake.Containers, docker.Container{
			ID: "c-" + name, Name: "c-" + name, State: state,
			Labels: map[string]string{tile.LabelTile: tl.ID, tile.LabelRole: "replica"},
		})
	}
	if _, err := w.orch.WipeVolume(ctx, v.ID, false, false); !conflict(err) || !strings.Contains(err.Error(), "in use by") {
		t.Fatalf("paused or restarting replica did not refuse: %v", err)
	}
	before := len(w.fake.Calls())
	j, err := w.orch.WipeVolume(ctx, v.ID, true, false)
	must(t, err)
	if got := w.wait(t, j.ID); got.State != "done" {
		t.Fatalf("job = %s", got.State)
	}
	got := w.callsFrom(before, "UntarVolume", "Stop", "Start")
	for _, want := range []string{"Stop c-held", "Stop c-loop", "Start c-loop"} {
		if !slices.Contains(got, want) {
			t.Errorf("calls %v miss %q", got, want)
		}
	}
	if slices.Contains(got, "Start c-held") {
		t.Errorf("paused replica started again: %v", got)
	}
}

// A failed backup cancels the wipe; the tiles it stopped start again.
func TestWipeVolumeBackupFirstFailure(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	v := w.dataVol(t)
	w.mountedBy(t, "api", true)
	w.deadS3(t)
	j, err := w.orch.WipeVolume(ctx, v.ID, true, true)
	must(t, err)
	got := w.wait(t, j.ID)
	if got.State != "failed" || !strings.Contains(got.Error, "nothing was wiped") {
		t.Fatalf("job = %s %q, want failed, nothing wiped", got.State, got.Error)
	}
	if calls := w.callsFrom(0, "UntarVolume"); len(calls) != 0 {
		t.Errorf("wiped anyway: %v", calls)
	}
	if calls := w.callsFrom(0, "Stop", "Start"); !slices.Equal(calls, []string{"Stop c-api", "Start c-api"}) {
		t.Errorf("tiles = %v, want stopped and started again", calls)
	}
}

// A good backup then the wipe, with a manual run kept.
func TestWipeVolumeBackupFirst(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	_, srv := newFakeS3(t)
	v := w.dataVol(t)
	w.offBoxS3(t, "offsite", srv.URL)
	j, err := w.orch.WipeVolume(ctx, v.ID, false, true)
	must(t, err)
	if got := w.wait(t, j.ID); got.State != "done" {
		t.Fatalf("job = %s %s", got.State, got.Error)
	}
	if rs, _ := w.orch.BackupRuns(ctx, v.ID); len(rs) != 1 || rs[0].Trigger != "manual" {
		t.Errorf("runs = %+v", rs)
	}
	tar := slices.Index(w.callsFrom(0, "TarVolume", "UntarVolume"), "UntarVolume "+v.Name)
	if tar != 1 {
		t.Errorf("backup then wipe expected, got %v", w.callsFrom(0, "TarVolume", "UntarVolume"))
	}
}

// If the tiles a wipe stopped do not start again, the job says so.
func TestWipeVolumeReportsTilesThatDoNotStart(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	v := w.dataVol(t)
	w.mountedBy(t, "api", true)
	w.fake.Err = map[string]error{"Start": errors.New("daemon says no")}
	j, err := w.orch.WipeVolume(ctx, v.ID, true, false)
	must(t, err)
	got := w.wait(t, j.ID)
	if got.State != "failed" || !strings.Contains(got.Error, "did not start again") || !strings.Contains(got.Error, "api") {
		t.Fatalf("job = %s %q, want a clear start failure naming api", got.State, got.Error)
	}
	if calls := w.callsFrom(0, "UntarVolume"); len(calls) != 1 {
		t.Errorf("wipe calls = %v", calls)
	}
}

// A removed volume (a stack file dropped it) comes back; the rest is refused.
func TestRestoreVolume(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	v := w.dataVol(t)
	if err := w.orch.RestoreVolume(ctx, v.ID); !conflict(err) {
		t.Errorf("live volume = %v", err)
	}
	if err := w.orch.RestoreVolume(ctx, "nope"); !notFound(err) {
		t.Errorf("unknown = %v", err)
	}
	_, err := w.orch.volumes.Orphan(ctx, v)
	must(t, err)
	must(t, w.orch.RestoreVolume(ctx, v.ID))
	got, err := w.orch.Volume(ctx, v.ID)
	if err != nil || got.OrphanedAt != nil {
		t.Fatalf("after restore = %+v, %v", got, err)
	}
	if vs, _ := w.orch.ListEnvVolumes(ctx, w.env); len(vs.Volumes) != 1 {
		t.Errorf("listed = %d", len(vs.Volumes))
	}

	hold := moveHold
	got.OrphanedAt = &hold
	must(t, w.st.Volumes.Update(ctx, got))
	if err := w.orch.RestoreVolume(ctx, v.ID); !conflict(err) {
		t.Errorf("old copy = %v", err)
	}
	must(t, w.st.Environments.Delete(ctx, w.env)) // its env goes: nothing to restore into
	if err := w.orch.RestoreVolume(ctx, v.ID); err == nil {
		t.Error("restored into a gone environment")
	}
}

func TestRestoreVolumeRefusesInstanceVolume(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	_, _, v := w.pg(t, "pg")
	_, err := w.orch.volumes.Orphan(ctx, v)
	must(t, err)
	if err := w.orch.RestoreVolume(ctx, v.ID); !conflict(err) {
		t.Errorf("instance orphan = %v", err)
	}
}

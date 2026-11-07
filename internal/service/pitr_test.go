package service

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	mflow "github.com/FyrmForge/stackr/internal/service/internal/flow/managed"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// bases is ss's base schedules: a dump schedule can land beside them at any
// moment (the backup watch adds one to an unscheduled volume).
func bases(ss []BackupSchedule) []BackupSchedule {
	var out []BackupSchedule
	for _, s := range ss {
		if s.Method == "base" {
			out = append(out, s)
		}
	}
	return out
}

func methodsOf(ss []BackupSchedule) []string {
	var out []string
	for _, s := range ss {
		out = append(out, s.Method)
	}
	slices.Sort(out)
	return out
}

// A new postgres instance archives (7 days) when its org can see an off-box
// destination, the daily dump's own rule; with only the local disk it does
// not. Deploying it declares the archive volume, which gets no backup of its
// own; the data volume gets the weekly base next to the daily dump.
func TestNewInstanceDefaultsToPITRWithAnOffBoxDestination(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	local, err := w.orch.CreateManagedTile(ctx, Tile{EnvironmentID: w.env, Name: "local"}, "postgres")
	must(t, err)
	if m, _ := w.orch.managed.GetByTile(ctx, local.ID); m.PITRDays != 0 {
		t.Fatalf("local disk only: pitr_days = %d", m.PITRDays)
	}
	_, srv := newFakeS3(t)
	w.sharedS3(t, srv.URL)
	pg, err := w.orch.CreateManagedTile(ctx, Tile{EnvironmentID: w.env, Name: "pg"}, "postgres")
	must(t, err)
	m, err := w.orch.managed.GetByTile(ctx, pg.ID)
	must(t, err)
	if m.PITRDays != DefaultPITRDays {
		t.Fatalf("off-box destination: pitr_days = %d", m.PITRDays)
	}
	c, err := w.orch.engines.Container(w.node(t, ctx), pg)
	must(t, err)
	if len(c.Binds) != 2 || !strings.Contains(strings.Join(c.Cmd, " "), "archive_mode=on") {
		t.Fatalf("container = %+v", c)
	}
	data, err := w.orch.dataVolume(ctx, pg)
	must(t, err)
	if ss, _ := w.orch.BackupSchedules(ctx, data.ID); !slices.Equal(methodsOf(ss), []string{"base", "dump"}) {
		t.Errorf("data volume schedules = %v", methodsOf(ss))
	}
	vs, _ := w.orch.Volumes(ctx, volume.Scope{Kind: "env", ID: w.env})
	n := 0
	for _, v := range vs {
		if IsArchiveVolume(v) {
			n++
			if ss, _ := w.orch.BackupSchedules(ctx, v.ID); len(ss) != 0 {
				t.Errorf("the archive volume has schedules: %v", methodsOf(ss))
			}
		}
	}
	if n != 1 {
		t.Errorf("archive volumes = %d", n)
	}
	// A base backup of an instance with PITR off is refused, never a tar of the volume.
	m.PITRDays = 0
	must(t, w.st.ManagedInstances.Update(ctx, m))
	if _, err := w.orch.subject(ctx, data, "base"); err == nil {
		t.Error("a base subject without PITR")
	}
}

func TestSetPITROnAndOff(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	pg, err := w.orch.CreateManagedTile(ctx, Tile{EnvironmentID: w.env, Name: "pg"}, "postgres")
	must(t, err)
	if _, err := w.orch.engines.Container(w.node(t, ctx), pg); err != nil { // declares the volumes
		t.Fatal(err)
	}
	data, err := w.orch.dataVolume(ctx, pg)
	must(t, err)
	if ss, _ := w.orch.BackupSchedules(ctx, data.ID); len(bases(ss)) != 0 {
		t.Fatalf("schedules before = %v", methodsOf(ss))
	}
	if _, _, err := w.orch.SetPITR(ctx, pg.ID, -1); err == nil {
		t.Error("a negative window")
	}
	m, _, err := w.orch.SetPITR(ctx, pg.ID, 3)
	must(t, err)
	ss, _ := w.orch.BackupSchedules(ctx, data.ID)
	ss = bases(ss)
	if m.PITRDays != 3 || len(ss) != 1 || ss[0].Method != "base" || ss[0].Cron != "@weekly" || ss[0].Keep != 0 {
		t.Fatalf("on: %d days, schedules %+v", m.PITRDays, ss)
	}
	if _, _, err := w.orch.SetPITR(ctx, pg.ID, 7); err != nil {
		t.Fatal(err)
	}
	if ss, _ := w.orch.BackupSchedules(ctx, data.ID); len(bases(ss)) != 1 {
		t.Errorf("a second schedule: %v", methodsOf(ss))
	}
	w.orch.savePITRState(ctx, m.ID, pitrState{Since: time.Now()})
	m, _, err = w.orch.SetPITR(ctx, pg.ID, 0)
	must(t, err)
	if m.PITRDays != 0 {
		t.Errorf("off: %d days", m.PITRDays)
	}
	w.orch.pitrTick(ctx) // no container runs: nothing archives any more, the bookkeeping goes
	if ss, _ := w.orch.BackupSchedules(ctx, data.ID); len(bases(ss)) != 0 {
		t.Errorf("off: schedules %v", methodsOf(ss))
	}
	if _, ok := w.orch.pitrState(ctx, m.ID); ok {
		t.Error("the shipper state outlived turning PITR off")
	}
	h, err := w.orch.PITRStatus(ctx, pg.ID)
	must(t, err)
	if h.On {
		t.Errorf("status = %+v", h)
	}
	if _, _, err := w.orch.SetPITR(ctx, w.env, 3); err == nil {
		t.Error("a non-tile")
	}
}

// sharedS3 is an off-box destination every org can use.
func (w *world) sharedS3(t *testing.T, endpoint string) BackupDest {
	t.Helper()
	d, err := w.orch.CreateBackupDest(context.Background(), nil, BackupDestSpec{
		Name: "offsite", Endpoint: endpoint, Bucket: "b", AccessKey: "a", SecretKey: "s", Shared: true,
	})
	must(t, err)
	return d
}

// One tick against an off-box destination: the guard passes on an empty
// prefix, the segments go as one bundle and the archive dir is emptied, the
// state row says so, and the first base backup is queued. A second tick with
// nothing new ships nothing. A foreign archive stops it.
func TestPITRTickShipsAndGuards(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	s3, srv := newFakeS3(t)
	w.sharedS3(t, srv.URL)
	pg, err := w.orch.CreateManagedTile(ctx, Tile{EnvironmentID: w.env, Name: "pg"}, "postgres")
	must(t, err)
	m, _ := w.orch.managed.GetByTile(ctx, pg.ID)
	if m.PITRDays == 0 {
		t.Fatal("not on by default")
	}
	_, err = w.orch.engines.Container(w.node(t, ctx), pg)
	must(t, err)
	data, err := w.orch.dataVolume(ctx, pg)
	must(t, err)
	w.fake.Containers = []docker.Container{{
		ID: "c-pg", Name: "pg", State: "running",
		Labels: map[string]string{tile.LabelTile: pg.ID, tile.LabelRole: "replica"},
	}}
	files := "000000010000000000000007\n000000010000000000000008\n"
	w.fake.ExecFn = func(cmd []string) string {
		switch {
		case cmd[0] == "ls":
			out := files
			files = ""
			return out
		case cmd[0] == "tar":
			return "TARBYTES"
		case cmd[0] == "du":
			return "64\t" + mflow.PGArchiveDir + "\n"
		case slices.Contains(cmd, "pg_stat_archiver"):
			return "0\n"
		}
		return ""
	}
	org := w.org
	root := "stackr/" + org + "/" + data.ID + "/pitr"
	w.orch.pitrTick(ctx)
	want := root + "/wal/000000010000000000000007-000000010000000000000008.tar.gz.age"
	if got := s3.keys(root + "/wal/"); !slices.Equal(got, []string{want}) {
		t.Fatalf("bundles = %v, want %s", got, want)
	}
	st, ok := w.orch.pitrState(ctx, m.ID)
	if !ok || !st.Guarded || st.LastSegment != "000000010000000000000008" || st.Waiting != 0 || st.LastCleanAt.IsZero() ||
		st.Bundles != 1 || st.Failing || st.Error != "" {
		t.Fatalf("state = %+v", st)
	}
	var rm bool
	for _, c := range w.fake.Calls() {
		if c.Method == "ExecStream" && len(c.Args) > 1 && c.Args[1] == "rm" {
			rm = strings.Contains(strings.Join(c.Args, " "), "/000000010000000000000008")
		}
	}
	if !rm {
		t.Error("the shipped files were not removed")
	}
	h, err := w.orch.PITRStatus(ctx, pg.ID)
	must(t, err)
	if !h.On || h.Bundles != 1 || h.Waiting != 0 || h.To == nil || h.Dest != "offsite" || h.Local {
		t.Errorf("health = %+v", h)
	}

	// idle: nothing new, nothing shipped, still clean
	w.orch.pitrTick(ctx)
	if got := s3.keys(root + "/wal/"); len(got) != 1 {
		t.Errorf("bundles after an idle tick = %v", got)
	}

	// a prefix that holds objects we have no run for stops a new state
	other := pitrKey(m.ID)
	must(t, w.st.Settings.Delete(ctx, other))
	s3.objs[root+"/wal/000000010000000000000001-000000010000000000000001.tar.gz.age"] = []byte("x")
	files = "000000010000000000000009\n"
	w.orch.pitrTick(ctx)
	if got := s3.keys(root + "/wal/"); len(got) != 2 {
		t.Errorf("a foreign archive was written into: %v", got)
	}
	st, _ = w.orch.pitrState(ctx, m.ID)
	if st.Guarded || !strings.HasPrefix(st.Error, "the archive at") {
		t.Errorf("state = %+v", st)
	}
	if !w.orch.hasProblem(ctx, "alert.pitr.lag."+m.ID) {
		t.Error("no problem for a refused archive")
	}
}

func TestPITRLagAndDisk(t *testing.T) {
	now := time.Now()
	clean := func(ago time.Duration) pitrState {
		return pitrState{Since: now.Add(-48 * time.Hour), LastCleanAt: now.Add(-ago)}
	}
	for _, c := range []struct {
		name string
		st   pitrState
		sev  string
	}{
		{"caught up", clean(time.Minute), ""},
		{"nine minutes", clean(9 * time.Minute), ""},
		{"eleven minutes", clean(11 * time.Minute), SevWarning},
		{"two hours", clean(2 * time.Hour), SevCritical},
		{"archive_command failing", func() pitrState { s := clean(time.Minute); s.Failing = true; return s }(), SevCritical},
		{"never clean, just seen", pitrState{Since: now.Add(-time.Minute)}, ""},
		{"never clean, seen long ago", pitrState{Since: now.Add(-30 * time.Minute)}, SevWarning},
		{"refused", pitrState{Since: now, Error: "the archive at x already holds objects"}, SevCritical},
	} {
		if sev, msg := pitrLag("pg", c.st, now); sev != c.sev || (sev != "" && msg == "") {
			t.Errorf("%s: %q %q", c.name, sev, msg)
		}
	}
	for _, c := range []struct {
		name          string
		dir           int64
		free, minFree uint64
		problem       bool
	}{
		{"small", 10 << 20, 100 << 30, 5 << 30, false},
		{"over a GB", 2 << 30, 100 << 30, 5 << 30, true},
		{"node nearly full", 10 << 20, 1 << 30, 5 << 30, true},
		{"free unknown", 10 << 20, 0, 5 << 30, false},
	} {
		if got := pitrDisk("pg", c.dir, c.free, c.minFree); (got != "") != c.problem {
			t.Errorf("%s: %q", c.name, got)
		}
	}
}

// node is ctx on the controller, as a deploy runs it.
func (w *world) node(t *testing.T, ctx context.Context) context.Context {
	t.Helper()
	ctx, err := w.orch.withController(ctx)
	must(t, err)
	return ctx
}

var _ = store.Volume{}

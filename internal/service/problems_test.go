package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// A failing scheduled backup opens a problem with no webhook set; a reminder
// keeps its since; the next good run clears it. The panel's the same.
func TestBackupFailureOpensProblem(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	o := w.orch
	backupSchedule(t, w, "s1")
	for _, tt := range []struct{ key, subject, want, link string }{
		{"schedule.s1", "volume x", "Backup of volume x failing: s3 down", "/acme/shop/dev"},
		{"panel", "the panel database", "Panel backup failing: s3 down", "/-/admin/backups"},
	} {
		o.backupResult(ctx, tt.key, "", tt.subject, errors.New("s3 down"))
		ps, err := o.Problems(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(ps) != 1 || ps[0].Message != tt.want || ps[0].Severity != SevCritical || ps[0].Link != tt.link {
			t.Fatalf("%s: %+v", tt.key, ps)
		}
		since := ps[0].Since
		must(t, o.store.Settings.Set(ctx, "alert.backup."+tt.key, time.Now().Add(-backupRemind-time.Hour).UTC().Format(time.RFC3339)))
		o.backupResult(ctx, tt.key, "", tt.subject, errors.New("s3 still down"))
		if ps, _ = o.Problems(ctx); len(ps) != 1 || !ps[0].Since.Equal(since) || !strings.HasSuffix(ps[0].Message, "s3 still down") {
			t.Errorf("%s reminder: %+v, since %v", tt.key, ps, since)
		}
		o.backupResult(ctx, tt.key, "", tt.subject, nil)
		if ps, _ = o.Problems(ctx); len(ps) != 0 {
			t.Errorf("%s after success: %+v", tt.key, ps)
		}
	}
}

// A volume schedule's problem links to the Storage row of the tile that
// mounts the volume, never to the admin Backups page, which lists panel
// backups only.
func TestScheduleProblemLinksTileStorage(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	backupSchedule(t, w, "s1")
	tl := w.tile(t, "web", false)
	tl.Volumes = "data-s1:/data"
	must(t, w.st.Tiles.Update(ctx, tl))
	w.orch.backupResult(ctx, "schedule.s1", "", "volume data-s1", errors.New("s3 down"))
	ps, err := w.orch.Problems(ctx)
	must(t, err)
	if len(ps) != 1 || ps[0].Link != "/acme/shop/dev?drawer="+tl.ID+"&tab=settings&open=storage" || ps[0].LinkText != "Open storage" {
		t.Errorf("%+v", ps)
	}
}

func backupSchedule(t *testing.T, w *world, id string) {
	t.Helper()
	ctx := context.Background()
	vol := store.Volume{ID: "v-" + id, ScopeKind: "env", ScopeID: w.env, Slug: "data-" + id, Name: "data", CreatedAt: time.Now()}
	must(t, w.st.Volumes.Create(ctx, vol))
	must(t, w.st.BackupSchedules.Create(ctx, store.BackupSchedule{ID: id, VolumeID: vol.ID, Method: "tar",
		Cron: "0 3 * * *", Timezone: "UTC", Keep: 3, Mode: "live", CreatedAt: time.Now()}))
}

func hasRow(t *testing.T, o *Orchestrator, key string) bool {
	t.Helper()
	_, ok, err := o.store.Settings.Get(context.Background(), key)
	must(t, err)
	return ok
}

// A failing schedule's problem goes when the schedule does: deleted through
// the service, or gone some other way (its volume or tile deleted), when
// the banner next reads it.
func TestBackupProblemGoesWithItsSchedule(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	o := w.orch
	backupSchedule(t, w, "s1")
	backupSchedule(t, w, "s2")
	for _, id := range []string{"s1", "s2"} {
		o.backupResult(ctx, "schedule."+id, "", "volume "+id, errors.New("s3 down"))
	}
	must(t, o.DeleteBackupSchedule(ctx, "s1"))
	if hasRow(t, o, "problem.backup.schedule.s1") || hasRow(t, o, "alert.backup.schedule.s1") {
		t.Error("deleting the schedule kept its problem")
	}
	must(t, w.st.BackupSchedules.Delete(ctx, "s2")) // as a volume cascade would
	if ps, _ := o.Problems(ctx); len(ps) != 0 {
		t.Errorf("problems = %+v", ps)
	}
	if hasRow(t, o, "problem.backup.schedule.s2") || hasRow(t, o, "alert.backup.schedule.s2") {
		t.Error("a gone schedule's rows were only hidden")
	}
}

// A check reconciles its problem on every run: an alert already firing with
// no problem row (an upgrade, or a write that failed) gets one on the next
// failing run without a second webhook, and a healthy run removes it.
func TestChecksReconcileTheirProblem(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	h := newHook(t)
	alertsTo(t, w.orch, h, "")
	o := w.orch
	n := store.Node{ID: "w1", Name: "h3"}
	must(t, o.store.Settings.Set(ctx, diskAlertKey(n.ID), "1"))
	must(t, o.store.Settings.Set(ctx, clockAlertKey(n.ID), "1"))
	must(t, o.store.Settings.Set(ctx, "alert.cert.root", "14"))
	o.diskAlert(ctx, n, 1<<20, 100<<30)
	o.clockAlert(ctx, n, time.Hour, true)
	o.certAlert(ctx, "root", "ctl", "cluster root certificate", time.Now().Add(10*24*time.Hour), time.Now())
	if !hasRow(t, o, "problem.disk.w1") || !hasRow(t, o, "problem.clock.w1") || !hasRow(t, o, "problem.cert.root") {
		t.Error("a firing alert got no problem")
	}
	if got := h.events(t, o); len(got) != 0 {
		t.Errorf("events = %v", got)
	}
	must(t, o.store.Settings.Delete(ctx, diskAlertKey(n.ID))) // a problem left with no alert row
	o.diskAlert(ctx, n, 50<<30, 100<<30)
	o.clockAlert(ctx, n, 0, true)
	must(t, o.store.Settings.Delete(ctx, "alert.cert.root"))
	o.certAlert(ctx, "root", "ctl", "cluster root certificate", time.Now().Add(90*24*time.Hour), time.Now())
	if hasRow(t, o, "problem.disk.w1") || hasRow(t, o, "problem.clock.w1") || hasRow(t, o, "problem.cert.root") {
		t.Error("a healthy check kept its problem")
	}
}

// Turning observability off clears its disk warning.
func TestObserveOffClearsItsProblem(t *testing.T) {
	ctx := context.Background()
	o := newWorld(t).orch
	must(t, o.store.Settings.Set(ctx, observeAlertKey, "1"))
	o.openProblem(ctx, observeAlertKey, SevWarning, "low.")
	o.observe.on.Store(false)
	o.observeDiskAlert(ctx, store.Node{Name: "ctl"}, 1<<30)
	if hasRow(t, o, observeAlertKey) || hasRow(t, o, "problem.observe.disk") {
		t.Error("observability off kept its disk problem")
	}
}

// Problems lists an offline worker from its row, critical first, then
// oldest first; a row about a node that is gone is left out.
func TestProblemsOrder(t *testing.T) {
	ctx := context.Background()
	o := newWorld(t).orch
	must(t, o.store.Nodes.Create(ctx, store.Node{ID: "w1", Name: "h3", Slug: "h3", Role: node.Worker, Tags: "[]",
		Status: node.StatusOffline, CreatedAt: time.Now()}))
	o.openProblem(ctx, "alert.disk.w1", SevWarning, "h3 is low on disk.")
	o.openProblem(ctx, "alert.disk.gone", SevWarning, "a forgotten node.")
	o.openProblem(ctx, "alert.cert.root", SevCritical, "root cert.")
	ps, err := o.Problems(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range ps {
		got = append(got, p.Key+" "+p.Link)
	}
	want := []string{"node.w1 ?drawer=node:w1", "cert.root /-/admin/keys", "disk.w1 ?drawer=node:w1"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %v, want %v", got, want)
	}
	if hasRow(t, o, "problem.disk.gone") {
		t.Error("a forgotten node's problem row was only hidden")
	}
	h3 := store.Node{ID: "w1", Name: "h3"}
	o.diskAlert(ctx, h3, 1<<20, 100<<30)  // low: marks the alert row
	o.diskAlert(ctx, h3, 50<<30, 100<<30) // recovered: the problem goes with it
	if ps, _ = o.Problems(ctx); len(ps) != 2 {
		t.Errorf("after clear: %+v", ps)
	}
}

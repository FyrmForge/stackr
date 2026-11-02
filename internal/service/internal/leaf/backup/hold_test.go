package backup_test

import (
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/backup"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// Deleting an org still takes its own destinations and their runs with it;
// the trigger only guards a destination deleted on its own.
func TestOrgDeleteTakesItsDestinationRuns(t *testing.T) {
	st := servicetest.Store(t)
	l := leaf(st)
	org := seedOrg(t, st)
	d, err := l.Create(ctx, &org, s3("mine", false), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Start(ctx, backup.KindPanel, nil, nil, d.ID, "manual"); err != nil {
		t.Fatal(err)
	}
	if err := st.Orgs.Delete(ctx, org); err != nil {
		t.Fatalf("org delete with a destination that holds runs: %v", err)
	}
}

// The pruning hold lifts once a run other than the current one has finished
// well after the restore.
func TestFirstSince(t *testing.T) {
	st := servicetest.Store(t)
	l := leaf(st)
	org := seedOrg(t, st)
	vol := seedVolume(t, st, org)
	local, _ := l.EnsureLocal(ctx, "/b")
	p := backup.Prefix(org, vol, "s")
	since := time.Now().Add(-time.Hour)
	first, _ := l.Start(ctx, backup.KindVolume, &vol, nil, local.ID, "cron")
	first, _ = l.Finish(ctx, first, p+"/a", 1, nil)
	if !l.FirstSince(ctx, p, since, first.ID) {
		t.Error("the first backup since the restore was not held")
	}
	second, _ := l.Start(ctx, backup.KindVolume, &vol, nil, local.ID, "cron")
	second, _ = l.Finish(ctx, second, p+"/b", 1, nil)
	if l.FirstSince(ctx, p, since, second.ID) {
		t.Error("the second backup was still held")
	}
}

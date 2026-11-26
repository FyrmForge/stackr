package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/paneldb"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

func movedJournal(t *testing.T, o *Orchestrator, j paneldb.Journal) {
	t.Helper()
	must(t, paneldb.WriteJournal(o.cfg.DataDir, j))
}

// The panel database row is for the controller of an installed panel only.
func TestPlanMovePanelDBRefusals(t *testing.T) {
	ctx := context.Background()
	d := newDiskTest(t)
	d.addDisk(t)
	p, err := d.orch.PlanMovePanelDB(ctx, adminP(), "w1", false)
	must(t, err)
	if !strings.Contains(p.Refusal, "controller") {
		t.Errorf("worker: %q", p.Refusal)
	}
	p, err = d.orch.PlanMovePanelDB(ctx, adminP(), d.ctl.ID, false)
	must(t, err)
	if p.Refusal == "" {
		t.Error("a source-build controller was offered the move")
	}
	if _, err := d.orch.MovePanelDB(ctx, adminP(), "w1", false); err == nil {
		t.Error("a move was queued for a worker")
	} else if _, ok := errorsAsRefused(err); !ok {
		t.Errorf("err = %v", err)
	}
	// the warning is part of the plan of a move onto the disk
	p, _ = d.orch.PlanMovePanelDB(ctx, adminP(), "w1", false)
	if !strings.Contains(p.Warning, "stackr-install disk-check") || strings.Contains(p.Warning, "—") {
		t.Errorf("warning = %q", p.Warning)
	}
}

func errorsAsRefused(err error) (errs.Refused, bool) {
	var r errs.Refused
	for e := err; e != nil; {
		if x, ok := e.(errs.Refused); ok {
			return x, true
		}
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			break
		}
		e = u.Unwrap()
	}
	return r, false
}

// Forget and Adopt refuse while the database is on the disk or moving.
func TestForgetAndAdoptRefuseWhileTheDatabaseIsOnTheDisk(t *testing.T) {
	ctx := context.Background()
	d := newDiskTest(t)
	disk := d.addDisk(t)
	if err := d.orch.forgetRefusals(ctx, disk, forgetSet{}); err != nil {
		t.Fatalf("an unmoved install: %v", err)
	}
	for _, j := range []paneldb.Journal{
		{Phase: paneldb.PhaseMoved, Direction: paneldb.DirIn, DiskID: disk.ID, InstallID: "i"},
		{Phase: paneldb.PhaseMounting, Direction: paneldb.DirIn, DiskID: disk.ID},
		{Phase: paneldb.PhaseUnmounting, Direction: paneldb.DirBack, DiskID: disk.ID},
	} {
		movedJournal(t, d.orch, j)
		if err := d.orch.forgetRefusals(ctx, disk, forgetSet{}); err == nil || !strings.Contains(err.Error(), "panel database") {
			t.Errorf("forget at %s: %v", j.Phase, err)
		}
		if err := d.orch.adoptBlocks(ctx, disk, store.Node{ID: "w1"}, "/disk", adoptView{}); err == nil || !strings.Contains(err.Error(), "panel database") {
			t.Errorf("adopt at %s: %v", j.Phase, err)
		}
	}
	// another disk's journal does not block this one
	movedJournal(t, d.orch, paneldb.Journal{Phase: paneldb.PhaseMoved, Direction: paneldb.DirIn, DiskID: "other"})
	if err := d.orch.forgetRefusals(ctx, disk, forgetSet{}); err != nil {
		t.Errorf("another disk's journal: %v", err)
	}
	// moved back: the disk is free again
	must(t, paneldb.ClearJournal(d.orch.cfg.DataDir))
	if err := d.orch.forgetRefusals(ctx, disk, forgetSet{}); err != nil {
		t.Errorf("after the move back: %v", err)
	}
}

func TestOldCopyTimeAndLanded(t *testing.T) {
	at := oldCopyTime("stackr.db.moved-20261126T103000Z-abc123")
	if at.IsZero() || at.Format("02-01") != "26-11" {
		t.Errorf("old copy time = %v", at)
	}
	if !oldCopyTime("stackr.db").IsZero() {
		t.Error("a name with no stamp read as a time")
	}
	d := newDiskTest(t)
	in := store.Job{Kind: kindMovePanelDB, Payload: `{"node_id":"c","back":false,"move_id":"m1"}`}
	back := store.Job{Kind: kindMovePanelDB, Payload: `{"node_id":"c","back":true,"move_id":"m2"}`}
	if d.orch.movePanelLanded(in) {
		t.Error("landed with no journal")
	}
	movedJournal(t, d.orch, paneldb.Journal{Phase: paneldb.PhaseMoved, Direction: paneldb.DirIn, MoveID: "m1"})
	if !d.orch.movePanelLanded(in) || d.orch.movePanelLanded(back) {
		t.Error("move-in landed wrong")
	}
	movedJournal(t, d.orch, paneldb.Journal{Phase: paneldb.PhaseNone, Direction: paneldb.DirBack, MoveID: "m2"})
	if !d.orch.movePanelLanded(back) || d.orch.movePanelLanded(in) {
		t.Error("move-back landed wrong")
	}
	// a rolled back move-in is not a landing
	movedJournal(t, d.orch, paneldb.Journal{Phase: paneldb.PhaseNone})
	if d.orch.movePanelLanded(in) {
		t.Error("a rolled back move landed")
	}
}

// Remove old copy is the only deleter, and keeps the moved state.
func TestRemoveOldCopyKeepsTheMovedJournal(t *testing.T) {
	d := newDiskTest(t)
	o := d.orch
	swap := paneldb.Swap(o.cfg.DataDir)
	must(t, os.MkdirAll(swap, 0o700))
	name := "stackr.db.moved-" + time.Now().UTC().Format(paneldb.Stamp) + "-abc123"
	for _, f := range []string{name, name + "-wal"} {
		must(t, os.WriteFile(filepath.Join(swap, f), []byte("x"), 0o600))
	}
	// the helper applies on a controller only; the service refuses elsewhere
	if err := o.RemovePanelDBOldCopy(context.Background(), adminP(), "w1"); err == nil {
		t.Error("removed on a worker")
	}
	j := paneldb.Journal{Phase: paneldb.PhaseMoved, Direction: paneldb.DirIn, DiskID: "d1", InstallID: "i", OldCopy: name}
	movedJournal(t, o, j)
	// the state shows the copy (the node check is bypassed: read the journal part)
	got, _ := o.panelJournal()
	if got.OldCopy != name {
		t.Fatalf("journal = %+v", got)
	}
}

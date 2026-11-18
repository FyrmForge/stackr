package service

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// DL: the commit moves exactly the envs the operation recorded (and locked),
// not one that appeared on the old node since.
func TestAdoptCommitMovesOnlyTheRecordedEnvs(t *testing.T) {
	ctx := context.Background()
	w := newAdoptWorld(t)
	d := w.step2(t)
	late, _ := w.scratch(t, "late")
	must(t, w.st.Environments.SetNode(ctx, late.ID, "old"))
	w.step3(t, d)
	w.step4(t, d)
	got, err := w.st.Environments.Get(ctx, late.ID)
	must(t, err)
	if got.NodeID != "old" || w.envNode(t) != "w1" {
		t.Errorf("late env on %q, recorded env on %q; want old and w1", got.NodeID, w.envNode(t))
	}
}

// DL: a job whose view reaches outside its lock set refuses before it records
// anything; the next press queues a fresh job with the right locks.
func TestAdoptRefusedWhenTheViewIsOutsideTheLockSet(t *testing.T) {
	w := newAdoptWorld(t)
	r := &jobs.Run{Log: io.Discard, Job: store.Job{LockSet: store.StringList{"disk:" + w.row.ID}}}
	err := w.orch.runDiskAdopt(context.Background(), r, diskAdoptJob{DiskID: w.row.ID, NodeID: "w1", Path: "/disk"})
	if _, ok := errs.IsConflict(err); !ok || !strings.Contains(err.Error(), "Press Adopt again") {
		t.Fatalf("err = %v, want a conflict", err)
	}
	w.unchanged(t)
}

// DL: the first target died after the claim and before the commit: another
// node takes the adopt over, and the claim accepts the dead target as owner.
func TestAdoptCanBeRetargetedWhenTheFirstTargetIsGone(t *testing.T) {
	ctx := context.Background()
	w := newAdoptWorld(t)
	must(t, w.st.Nodes.Create(ctx, store.Node{ID: "w2", Name: "w2", Slug: "w2", Role: node.Worker, Tags: "[]", RangeCIDR: "10.210.0.0/16",
		Status: node.StatusOnline, CreatedAt: time.Now()}))
	d, err := w.orch.diskLeaf.BeginAdopt(ctx, w.row.ID, "w2", "/disk", []string{w.edge.ID})
	must(t, err)
	w.step3(t, d)
	if w.stampOwner(t) != "w2" {
		t.Fatalf("stamp owner %s", w.stampOwner(t))
	}
	if err := w.pressErr(t); !errors.Is(err, errs.ErrRefused) {
		t.Fatalf("while w2 is alive: %v, want refused", err)
	}
	must(t, w.st.Nodes.Delete(ctx, "w2"))
	w.done(t)
	if w.envNode(t) != "w1" || w.stampOwner(t) != "w1" || w.get(t).NodeID != "w1" {
		t.Errorf("env on %s, stamp owner %s, disk on %s", w.envNode(t), w.stampOwner(t), w.get(t).NodeID)
	}
}

// DL: an object of a claimed name bound to another folder on the new node
// would fail step 6 as a ghost after the commit: refused before step 2, and
// the discard clears it.
func TestAdoptRefusedWhenAClaimedNameIsBoundElsewhereOnTheNewNode(t *testing.T) {
	ctx := context.Background()
	w := newAdoptWorld(t)
	must(t, w.wFake.CreateVolume(ctx, w.data.Name, "local", map[string]string{"type": "none", "o": "bind", "device": "/elsewhere/data"}, nil))
	err := w.pressErr(t)
	if !errors.Is(err, errs.ErrRefused) || !strings.Contains(err.Error(), "bound to another folder") {
		t.Fatalf("err = %v", err)
	}
	w.unchanged(t)
	must(t, w.orch.DiscardAdoptCopy(ctx, adminP(), "w1", "/disk", w.row.ID, w.data.ID, w.data.Slug))
	w.done(t)
}

// An object bound at <elsewhere>/<diskID>/<name> is not the disk's folder: it
// is reported and refuses the adopt like any other foreign bind.
func TestAdoptRefusedWhenBoundUnderAnotherMountWithTheSameTail(t *testing.T) {
	ctx := context.Background()
	w := newAdoptWorld(t)
	must(t, w.wFake.CreateVolume(ctx, w.data.Name, "local", map[string]string{"type": "none", "o": "bind", "device": "/elsewhere/" + w.row.ID + "/" + w.data.Name}, nil))
	err := w.pressErr(t)
	if !errors.Is(err, errs.ErrRefused) || !strings.Contains(err.Error(), "bound to another folder") {
		t.Fatalf("err = %v", err)
	}
	w.unchanged(t)
}

// A copy is not the only thing in the way: the old node is back. The plan does
// not offer the discard and the discard verb refuses, so nothing is deleted for nothing.
func TestAdoptDiscardOfferedOnlyWhenTheCopyIsTheOnlyRefusal(t *testing.T) {
	ctx := context.Background()
	w := newAdoptWorld(t)
	w.wFake.Edit(func() {
		w.wFake.Volumes = append(w.wFake.Volumes, docker.VolumeInfo{Name: w.data.Name, SizeBytes: 9})
	})
	p, err := w.orch.PlanDiskAdopt(ctx, "w1", "/disk")
	must(t, err)
	if !p.CopiesOnly {
		t.Fatalf("copies are the only refusal: %+v", p)
	}
	must(t, w.st.Nodes.Create(ctx, store.Node{ID: "old", Name: "oldbox", Slug: "old", Role: node.Worker, Tags: "[]", RangeCIDR: "10.211.0.0/16",
		Status: node.StatusOnline, CreatedAt: time.Now()}))
	p, err = w.orch.PlanDiskAdopt(ctx, "w1", "/disk")
	must(t, err)
	if p.CopiesOnly || !strings.Contains(p.Refusal, "online") {
		t.Fatalf("plan = %+v", p)
	}
	if err := w.orch.DiscardAdoptCopy(ctx, adminP(), "w1", "/disk", w.row.ID, w.data.ID, w.data.Slug); !errors.Is(err, errs.ErrRefused) {
		t.Fatalf("discard = %v, want refused", err)
	}
	if !slices.ContainsFunc(w.wFake.Volumes, func(v docker.VolumeInfo) bool { return v.Name == w.data.Name }) {
		t.Error("the copy was removed")
	}
}

// An object bound to the wrong folder with no folder row is still a ghost for the drawer.
func TestVolumeStorageShowsABoundObjectWithNoFolderRow(t *testing.T) {
	ctx := context.Background()
	d := newDiskTest(t)
	d.addDisk(t)
	v := d.vol(t, "data")
	must(t, d.wFake.CreateVolume(ctx, v.Name, "local", map[string]string{"type": "none", "o": "bind", "device": "/elsewhere/data"}, nil))
	_, _ = d.orch.volumes.Ensure(ctx, v) // the gate decides and remembers the ghost
	s, err := d.orch.VolumeStorage(ctx, v.ID)
	must(t, err)
	if s.Ghost != "bound" || s.Device != "/elsewhere/data" {
		t.Fatalf("storage = %+v", s)
	}
}

// The Stored on card disables Move to disk with the plan's refusal while the node has not read its disk.
func TestVolumeMoveInfoDisablesWhenTheDiskIsUnread(t *testing.T) {
	ctx := context.Background()
	d := newDiskTest(t)
	d.addDisk(t)
	v := d.vol(t, "plain")
	m, err := d.orch.VolumeMoveInfo(ctx, v.ID)
	must(t, err)
	if !m.Offer || !strings.Contains(m.Why, "not read its disk") {
		t.Fatalf("info = %+v", m)
	}
}

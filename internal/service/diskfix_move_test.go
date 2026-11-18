package service

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/opmember"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// DL: the post-copy check must prove the destination: a folder that vanished
// (unknown) must stop the move before the swap, the old copy stays live.
func TestMoveAbortsWhenTheCopyFolderCannotBeProven(t *testing.T) {
	r := newMoveRig(t)
	r.wFake.OnUntar = func(name string) { _ = os.RemoveAll(r.folder(r.row, name)) }
	j := r.move(t, r.v)
	if j.State != job.Failed || !strings.Contains(j.Error, "missing or empty") {
		t.Fatalf("move: %s %q, want failed", j.State, j.Error)
	}
	r.wantUntouched(t, r.v)
}

// swappedRig is a move that crashed right after the swap, its container gone.
func swappedRig(t *testing.T) *moveRig {
	t.Helper()
	r := newMoveRig(t)
	r.crash(t, "swap", r.v)
	r.wFake.Containers = nil
	return r
}

// DL: the resume must not deploy onto a target folder that is gone: the tile
// would start empty while the old copy waits on its orphan row.
func TestMoveResumeRefusedWhenTheTargetFolderVanished(t *testing.T) {
	r := swappedRig(t)
	target := moveTarget(r.v.ID)
	must(t, os.RemoveAll(r.folder(r.row, target)))
	before := countCalls(r.wFake, "Create(")
	_, err := r.orch.MoveVolumeToDisk(context.Background(), adminP(), r.v.ID)
	if _, ok := errors.AsType[errs.Refused](err); !ok || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("err = %v, want refused: missing", err)
	}
	if n := countCalls(r.wFake, "Create("); n != before {
		t.Errorf("a deploy ran: %v", callNames(r.wFake))
	}
	if _, ok := r.orphanNamed(t, r.v.Name); !ok {
		t.Error("the old copy's orphan row is gone")
	}
}

// DL: the same with a folder that is empty while the old copy holds data.
func TestMoveResumeRefusedWhenTheTargetFolderIsEmptyAndTheOldCopyHoldsData(t *testing.T) {
	r := swappedRig(t)
	must(t, os.Remove(r.folder(r.row, moveTarget(r.v.ID))+"/file"))
	before := countCalls(r.wFake, "Create(")
	_, err := r.orch.MoveVolumeToDisk(context.Background(), adminP(), r.v.ID)
	if _, ok := errors.AsType[errs.Refused](err); !ok || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("err = %v, want refused: empty", err)
	}
	if n := countCalls(r.wFake, "Create("); n != before {
		t.Errorf("a deploy ran: %v", callNames(r.wFake))
	}
}

// A volume already named for the disk but with no folder (after a Forget) is a
// fresh move to the other target name, not a dead end.
func TestMoveOfAPlainVolumeThatCarriesTheTargetNameGoesToTheOtherName(t *testing.T) {
	r := newMoveRig(t)
	base := moveTarget(r.v.ID)
	v := r.get(t, r.v.ID)
	v.Name = base
	must(t, r.st.Volumes.Update(context.Background(), v))
	r.wFake.Volumes = []docker.VolumeInfo{{Name: base, SizeBytes: 100}}
	j := r.move(t, v)
	if j.State != job.Done {
		t.Fatalf("move: %s %q", j.State, j.Error)
	}
	if got := r.get(t, v.ID).Name; got != base+"2" {
		t.Errorf("name = %q, want %q", got, base+"2")
	}
	r.bound(t, r.wFake, r.row, base+"2")
}

// DL: the single verb on a volume that is fully moved and running does nothing.
func TestMoveOfAFullyMovedVolumeIsRefusedAndRedeploysNothing(t *testing.T) {
	r := newMoveRig(t)
	if j := r.move(t, r.v); j.State != job.Done {
		t.Fatalf("move: %s %q", j.State, j.Error)
	}
	r.wFake.Containers = []docker.Container{{ID: "n-db", Name: "db", State: "running",
		Labels: map[string]string{tile.LabelTile: r.tl.ID, tile.LabelRole: "replica"}}}
	before := countCalls(r.wFake, "Create(")
	_, err := r.orch.MoveVolumeToDisk(context.Background(), adminP(), r.v.ID)
	if _, ok := errors.AsType[errs.Refused](err); !ok || !strings.Contains(err.Error(), "Already on the disk") {
		t.Fatalf("err = %v, want refused: already on the disk", err)
	}
	if n := countCalls(r.wFake, "Create("); n != before {
		t.Errorf("a redeploy ran")
	}
}

// DL: a primary that failed over off this node must not have its volume moved.
func TestMoveRefusedForARelocatedPrimary(t *testing.T) {
	ctx := context.Background()
	r := newMoveRig(t)
	m, err := r.orch.operated.Create(ctx, r.tl.ID, "postgres", "stackr", "")
	must(t, err)
	v := r.v
	v.InstanceID = &m.ID
	must(t, r.st.Volumes.Update(ctx, v))
	must(t, r.st.OperatedMembers.Create(ctx, store.OperatedMember{ID: "m-new", InstanceID: m.ID, Role: opmember.Primary, NodeID: "w2",
		Volume: "elsewhere", State: opmember.Running, Phase: opmember.Streaming, CreatedAt: time.Now(), UpdatedAt: time.Now()}))
	_, err = r.orch.MoveVolumeToDisk(ctx, adminP(), v.ID)
	if _, ok := errors.AsType[errs.Refused](err); !ok || !strings.Contains(err.Error(), "after a failover") {
		t.Fatalf("err = %v, want refused: after a failover", err)
	}
	r.wantUntouched(t, v)
}

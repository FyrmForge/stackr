package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/disk"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/opmember"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Phase 2 review round 1 fixes.

// instanceWorld is a forgetWorld whose volume belongs to a Postgres instance
// tile "pg" that has no volumes line.
func instanceWorld(t *testing.T) (*forgetWorld, Tile) {
	t.Helper()
	ctx := context.Background()
	w := newForgetWorld(t, false, false)
	pg := w.diskTile(t, "pg", "")
	m, err := w.orch.operated.Create(ctx, pg.ID, "postgres", "stackr", "")
	must(t, err)
	v := w.v
	v.InstanceID = &m.ID
	must(t, w.st.Volumes.Update(ctx, v))
	w.wFake.Containers = append(w.wFake.Containers, docker.Container{ID: "c-pg", Name: "pg", State: "running",
		Labels: map[string]string{tile.LabelTile: pg.ID, tile.LabelRole: "replica"}})
	return w, pg
}

// DL: an instance's data volume is owned by its tile, which has no volumes
// line; Forget and Check disk must still find it.
func TestDiskTilesFindsAnInstanceTileWithNoVolumesLine(t *testing.T) {
	w, pg := instanceWorld(t)
	ts, err := w.orch.diskTiles(context.Background(), w.edge, map[string]bool{w.v.Name: true})
	must(t, err)
	if len(ts) != 1 || ts[0].ID != pg.ID {
		t.Fatalf("diskTiles = %+v, want the instance tile", ts)
	}
}

func TestForgetRemovesTheInstanceTileContainer(t *testing.T) {
	w, _ := instanceWorld(t)
	w.lose(t)
	if j := w.forget(t); j.State != "done" && w.state(t) != disk.StateForgetting {
		t.Logf("forget: %s %s", j.State, j.Error)
	}
	if !sawCall(w.wFake, "StopRemove(c-pg") && !sawCall(w.wFake, "Remove(c-pg") {
		t.Errorf("the instance container was not removed: %v", callNames(w.wFake))
	}
}

// DL: a primary that failed over onto this disk's node is refused before any
// change: forget would remove it and nothing would restore it.
func TestForgetRefusedForARelocatedPrimaryOnTheDisk(t *testing.T) {
	ctx := context.Background()
	w := newForgetWorld(t, false, false)
	w.lose(t)
	m, err := w.orch.operated.Create(ctx, w.devT.ID, "postgres", "stackr", "")
	must(t, err)
	must(t, w.st.OperatedMembers.Create(ctx, store.OperatedMember{ID: "m-new", InstanceID: m.ID, Role: opmember.Primary, NodeID: "w1",
		Volume: w.v.Name, State: opmember.Running, Phase: opmember.Streaming, CreatedAt: time.Now(), UpdatedAt: time.Now()}))
	_, err = w.orch.ForgetNodeDisk(ctx, adminP(), w.row.ID, "w1")
	if !errors.Is(err, errs.ErrRefused) || !strings.Contains(err.Error(), "after a failover") {
		t.Fatalf("err = %v", err)
	}
	if w.state(t) != disk.StateMissing || sawCall(w.wFake, "StopRemove(") || sawCall(w.wFake, "RemoveVolume(") {
		t.Errorf("state %s, calls %v", w.state(t), callNames(w.wFake))
	}
}

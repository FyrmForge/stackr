package service

import (
	"context"
	"io"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Deleting the tile of a failed-over instance orphans the data volume where
// the data lives now (the failover node), so the retention sweep and the
// panel's orphan list cover it.
func TestDeleteAfterFailoverOrphansTheRelocatedDataVolume(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newFailRig(t, false)
	o := r.n.orch
	_, err := doSwitchover(o, ctx, adminP(), r.inst.TileID, "w1")
	must(t, err)
	p, old := r.members()
	must(t, o.members.Delete(ctx, old)) // the standby goes first: a delete refuses while any exist
	tl := r.tile()
	e, err := o.envs.Get(ctx, tl.EnvironmentID)
	must(t, err)
	rel, away := o.relocated(ctx, tl.ID)
	if !away {
		t.Fatal("not relocated")
	}
	if _, err := o.promote.Remove(ctx, e, []store.Tile{tl}, io.Discard); err != nil {
		t.Fatal(err)
	}
	must(t, o.dropRelocated(ctx, tl, rel))
	orphans, err := o.store.Volumes.ListOrphaned(ctx)
	must(t, err)
	var found bool
	for _, v := range orphans {
		if v.Name == p.Volume && v.NodeID == "w1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the failover node's data volume %q is not an orphan on w1", p.Volume)
	}
}

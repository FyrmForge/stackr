package service

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/opmember"
)

// An old primary whose node was offline at the failover has no network block
// on its row; when the node returns the rejoin reads it from the node, keeps
// it, lets it replicate in and builds its flow before the rewind.
func TestRejoinFillsMissingSubnet(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newFailRig(t, true)
	o := r.n.orch
	must(t, setNode(o, "w1", node.StatusOffline))
	r.ageLease(2 * time.Minute)
	r.lsn[r.n.ctl.ID] = "0/5000100"
	if _, err := doPromote(o, ctx, adminP(), r.inst.TileID, r.n.ctl.ID, true, "", false); err != nil {
		t.Fatal(err)
	}
	_, old := r.members()
	_, err := o.members.SetSubnet(ctx, old, "") // as the swap leaves it when the node cannot be asked
	must(t, err)
	must(t, setNode(o, "w1", node.StatusOnline))
	o.resumeFailovers(ctx)
	_, old = r.members()
	if old.Subnet != "10.209.4.0/24" || old.Fenced || old.Phase != opmember.Starting {
		t.Fatalf("after its node returned: %+v", old)
	}
	if !slices.Contains(o.replSubnets(ctx, r.inst.ID, ""), "10.209.4.0/24") {
		t.Error("the new primary does not let the block replicate")
	}
	r.order("w1 create stackr-rewind", "w1 create stackr-member")
}

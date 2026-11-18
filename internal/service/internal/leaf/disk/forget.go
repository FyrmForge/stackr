package disk

import (
	"context"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// MayForget says whether d may be forgotten. A row whose node is gone may
// always go (it is metadata only), except an adopt whose transaction has
// committed: that one is resumed, not forgotten. With a live node only a
// missing, adding or already forgetting row goes; a ready or adopting disk
// never does.
func MayForget(d store.NodeDisk, nodeGone bool) error {
	switch {
	case d.State == StateAdopting && d.OpNodeID != "" && d.NodeID == d.OpNodeID:
		return errs.Refusedf("This disk is being adopted. Resume the adopt instead.")
	case nodeGone:
		return nil
	case d.State == StateMissing, d.State == StateAdding, d.State == StateForgetting:
		return nil
	case d.State == StateAdopting:
		return errs.Refusedf("This disk is being adopted.")
	}
	return errs.Refusedf("This disk is ready. Only a missing or half-added disk can be forgotten.")
}

// BeginForget sets the row forgetting (the retry key of the operation). A
// forgetting row stays as it is. It re-reads the row, so a state another call
// changed since is judged as it is now.
func (l *Leaf) BeginForget(ctx context.Context, d store.NodeDisk) (store.NodeDisk, error) {
	cur, err := l.disks.Get(ctx, d.ID)
	if err != nil {
		return d, err
	}
	if err := MayForget(cur, false); err != nil {
		return cur, err
	}
	if cur.State == StateForgetting {
		return cur, nil
	}
	cur.State = StateForgetting
	return cur, l.disks.Update(ctx, cur)
}

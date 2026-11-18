package disk

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/FyrmForge/stackr/internal/hostdisk"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Adopt: a disk whose node is gone is handed to another node. The row says
// adopting from BeginAdopt until FinishAdopt, which is what makes every
// ordinary caller refuse in between (see op.go).

// BeginAdopt records the operation in one UPDATE: state adopting, the new
// node and path, the old node, and the exact env set the adopt works on. A row
// already adopting for the same target is returned as it is (its env set
// stays); one adopting for another node or path is refused. It re-reads the row.
func (l *Leaf) BeginAdopt(ctx context.Context, diskID, nodeID, mount string, envIDs []string) (store.NodeDisk, error) {
	cur, err := l.disks.Get(ctx, diskID)
	if err != nil {
		return cur, err
	}
	switch cur.State {
	case StateReady, StateMissing:
	case StateAdopting:
		if cur.OpNodeID != nodeID || cur.OpMountPath != mount {
			return cur, errs.Refusedf("This disk is already being adopted for another node or path. Finish that adopt first.")
		}
		return cur, nil
	default:
		return cur, errs.Refusedf("This disk cannot be adopted while it is %s.", cur.State)
	}
	ids, _ := json.Marshal(slices.Clone(envIDs))
	cur.State, cur.OpNodeID, cur.OpFromNodeID, cur.OpMountPath, cur.OpEnvIDs = StateAdopting, nodeID, cur.NodeID, mount, string(ids)
	if envIDs == nil {
		cur.OpEnvIDs = "[]"
	}
	return cur, l.disks.Update(ctx, cur)
}

// Claim has the new node's host rewrite the stamp's owner: the stamp must
// carry this disk's id and an owner that is the old node or already the new
// one. Idempotent. From here the old node's verify fails with foreign_stamp.
func (l *Leaf) Claim(ctx context.Context, d store.NodeDisk) error {
	res, err := l.host.Disk(ctx, d.OpNodeID, hostdisk.Request{
		Op: "claim", Kind: kindOf(d), DiskID: d.ID, Owner: d.OpNodeID, From: d.OpFromNodeID, Mount: d.OpMountPath,
	})
	if err != nil {
		return err
	}
	if res.Error != nil {
		return res.Error
	}
	return nil
}

// Retarget points an adopt whose transaction has not run at another node and
// path: its first target is gone, so it can never commit. The old node, the
// env set and the state stay. The caller has judged that the first target is gone.
func (l *Leaf) Retarget(ctx context.Context, d store.NodeDisk, nodeID, mount string) (store.NodeDisk, error) {
	cur, err := l.disks.Get(ctx, d.ID)
	if err != nil {
		return cur, err
	}
	if cur.State != StateAdopting || cur.NodeID == cur.OpNodeID {
		return cur, errs.Refusedf("This disk is not in an adopt that can be retargeted.")
	}
	cur.OpNodeID, cur.OpMountPath = nodeID, mount
	return cur, l.disks.Update(ctx, cur)
}

// ClaimFrom is Claim for a stamp owned by from, which is not the row's old
// node: the first adopt's target, which died after the claim.
func (l *Leaf) ClaimFrom(ctx context.Context, d store.NodeDisk, from string) error {
	d.OpFromNodeID = from
	return l.Claim(ctx, d)
}

// Repoint is the disk's part of the adopt transaction, in tx: the row moves
// to the new node and path (the state stays adopting) and every folder row
// whose name is not in claimed is deleted; the folders stay on the disk.
// It returns the names whose rows went.
func Repoint(ctx context.Context, tx store.Tx, d store.NodeDisk, claimed map[string]bool) (store.NodeDisk, []string, error) {
	if d.State != StateAdopting || d.OpNodeID == "" {
		return d, nil, errors.New("disk: repoint of a row that is not adopting")
	}
	rows, err := tx.DiskFolders.ListByDisk(ctx, d.ID)
	if err != nil {
		return d, nil, err
	}
	var dropped []string
	for _, f := range rows {
		if claimed[f.Name] {
			continue
		}
		if err := tx.DiskFolders.Delete(ctx, f.ID); err != nil {
			return d, nil, err
		}
		dropped = append(dropped, f.Name)
	}
	d.NodeID, d.MountPath = d.OpNodeID, d.OpMountPath
	return d, dropped, tx.NodeDisks.Update(ctx, d)
}

// FinishAdopt sets the row ready and clears the operation, in one UPDATE.
func (l *Leaf) FinishAdopt(ctx context.Context, d store.NodeDisk) error {
	cur, err := l.disks.Get(ctx, d.ID)
	if err != nil {
		return err
	}
	if cur.State != StateAdopting {
		return errs.Refusedf("This disk is not being adopted.")
	}
	cur.State, cur.OpNodeID, cur.OpFromNodeID, cur.OpMountPath, cur.OpEnvIDs = StateReady, "", "", "", ""
	return l.disks.Update(ctx, cur)
}

// AbortAdopt puts an adopt back that changed nothing but its own row: the
// stamp was refused, so nothing moved. Only a row whose transaction has not
// run goes back, to missing (the old node's state is not known any more).
func (l *Leaf) AbortAdopt(ctx context.Context, d store.NodeDisk) error {
	cur, err := l.disks.Get(ctx, d.ID)
	if err != nil {
		return err
	}
	if cur.State != StateAdopting || cur.NodeID == cur.OpNodeID {
		return nil
	}
	cur.State, cur.OpNodeID, cur.OpFromNodeID, cur.OpMountPath, cur.OpEnvIDs = StateMissing, "", "", "", ""
	return l.disks.Update(ctx, cur)
}

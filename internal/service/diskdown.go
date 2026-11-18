package service

import (
	"context"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/disk"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
)

// EnvDiskDown reports the node whose disk is missing when the env has a
// volume with a folder on it, the name for the env banner. An env with no
// disk volume, a node with no disk, a disk that is ready or still being
// added, or any lookup that fails, says nothing.
func (o *Orchestrator) EnvDiskDown(ctx context.Context, envID string) (string, bool) {
	e, err := o.store.Environments.Get(ctx, envID)
	if err != nil || e.NodeID == "" {
		return "", false
	}
	d, has, err := o.nodeDisk(ctx, e.NodeID)
	if err != nil || !has || d.State != disk.StateMissing {
		return "", false
	}
	fs, err := o.diskLeaf.Folders(ctx, d)
	if err != nil || len(fs) == 0 {
		return "", false
	}
	vs, err := o.volumes.List(ctx, volume.Scope{Kind: "env", ID: e.ID})
	if err != nil {
		return "", false
	}
	for _, v := range vs {
		for _, f := range fs {
			if v.Name == f.Name && v.OrphanedAt == nil {
				n, err := o.store.Nodes.Get(ctx, e.NodeID)
				if err != nil {
					return "", false
				}
				return n.Name, true
			}
		}
	}
	return "", false
}

// nodeAndDisk is the node an env runs on and whether it has a ready disk, for
// a plan's create rows: a new volume lands on that disk. Both are empty when
// the node cannot be told.
func (o *Orchestrator) nodeAndDisk(ctx context.Context, envID string) (string, bool) {
	n, err := o.nodeOfEnvID(ctx, envID)
	if err != nil {
		return "", false
	}
	d, has, err := o.nodeDisk(ctx, n.ID)
	return n.Name, err == nil && has && d.State == disk.StateReady
}

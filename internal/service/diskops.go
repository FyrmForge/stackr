package service

import (
	"context"
	"encoding/json"

	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/disk"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// The operation context (disk.WithOp), seen from the service. Adopt and Forget
// disk leave their disk row in adopting or forgetting, which every caller
// reads as "not ready". Only the job that owns the operation sees through it:
// adopting answers as the node's ready disk, forgetting as no disk at all.

// opMode is the operation ctx runs on d: disk.StateAdopting,
// disk.StateForgetting, or "" when there is none for this disk and state.
func opMode(ctx context.Context, d store.NodeDisk) string {
	m, _ := disk.OpOf(ctx, d)
	return m
}

// diskInOp is the disk row with this id and whether it is in adopting or
// forgetting: the resume lookup. ok is false for any other state.
func (o *Orchestrator) diskInOp(ctx context.Context, id string) (d store.NodeDisk, ok bool, err error) {
	if d, err = o.store.NodeDisks.Get(ctx, id); err != nil {
		return d, false, err
	}
	return d, d.State == disk.StateAdopting || d.State == disk.StateForgetting, nil
}

// opEnvIDs are the envs an adopt works on (op_env_ids, a JSON array).
func opEnvIDs(d store.NodeDisk) ([]string, error) {
	if d.OpEnvIDs == "" {
		return nil, nil
	}
	var ids []string
	return ids, json.Unmarshal([]byte(d.OpEnvIDs), &ids)
}

// ownedJob runs h with no operation context, whatever the caller held when it
// enqueued the job: a handler makes its own with disk.WithOp.
func ownedJob(h jobs.Handler) jobs.Handler {
	return func(ctx context.Context, r *jobs.Run) error {
		return h(disk.WithoutOp(ctx), r)
	}
}

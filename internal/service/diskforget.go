package service

import (
	"context"

	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
)

// Forget disk, and the two Discard verbs of a ghost volume. Work package 2F
// fills these in; until then every verb refuses.

// diskForgetJob is the disk-forget payload.
type diskForgetJob struct {
	DiskID string `json:"disk_id"`
	By     string `json:"by,omitempty"`
	KeyID  string `json:"key_id,omitempty"`
}

// ForgetNodeDisk on a forgetting row resumes it.
func (o *Orchestrator) ForgetNodeDisk(ctx context.Context, by *Principal, diskID, confirm string) (Job, error) {
	return Job{}, notBuilt()
}

func (o *Orchestrator) DiscardPlainCopy(ctx context.Context, by *Principal, volumeID, confirm string) error {
	return notBuilt()
}

func (o *Orchestrator) DiscardBoundObject(ctx context.Context, by *Principal, volumeID, confirm string) error {
	return notBuilt()
}

func (o *Orchestrator) runDiskForget(ctx context.Context, r *jobs.Run, p diskForgetJob) error {
	return notBuilt()
}

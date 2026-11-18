package service

import (
	"context"

	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
)

// Adopt: a disk whose node is gone is handed to another node. Work package 2A
// fills these in; until then every verb refuses.

// DiskAdoptPlan is what the adopt dialog shows before the confirm.
type DiskAdoptPlan struct{}

// diskAdoptJob is the disk-adopt payload.
type diskAdoptJob struct {
	DiskID string `json:"disk_id"`
	NodeID string `json:"node_id"` // the new node
	Path   string `json:"path"`
	By     string `json:"by,omitempty"`
	KeyID  string `json:"key_id,omitempty"`
}

func (o *Orchestrator) PlanDiskAdopt(ctx context.Context, nodeID, path string) (DiskAdoptPlan, error) {
	return DiskAdoptPlan{}, notBuilt()
}

// AdoptDisk on an adopting row resumes it.
func (o *Orchestrator) AdoptDisk(ctx context.Context, by *Principal, nodeID, diskID, path, confirm string) (Job, error) {
	return Job{}, notBuilt()
}

func (o *Orchestrator) runDiskAdopt(ctx context.Context, r *jobs.Run, p diskAdoptJob) error {
	return notBuilt()
}

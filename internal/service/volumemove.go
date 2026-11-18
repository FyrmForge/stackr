package service

import (
	"context"

	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
)

// Move to disk: one volume, or one group of Move all, copied onto the node's
// disk. Work package 2M fills these in; until then every verb refuses.

// volumeMoveItem is one volume of a move: its derived target and the name it
// has now, kept in the payload because no label identifies the old copy.
type volumeMoveItem struct {
	VolumeID string `json:"volume_id"`
	Target   string `json:"target"`
	OldName  string `json:"old_name"`
}

// volumeMoveJob is the volume-to-disk payload: a group (one for the single verb).
type volumeMoveJob struct {
	NodeID  string           `json:"node_id"`
	Volumes []volumeMoveItem `json:"volumes"`
	By      string           `json:"by,omitempty"`
	KeyID   string           `json:"key_id,omitempty"`
}

// MoveVolumeToDisk is the single verb (route verb tile.write).
func (o *Orchestrator) MoveVolumeToDisk(ctx context.Context, by *Principal, volumeID string) (Job, error) {
	return Job{}, notBuilt()
}

func (o *Orchestrator) runVolumeToDisk(ctx context.Context, r *jobs.Run, p volumeMoveJob) error {
	return notBuilt()
}

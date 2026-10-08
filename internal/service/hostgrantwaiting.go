package service

import (
	"context"
	"encoding/json"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
)

// HostGrantsWaiting (server admin) is how many stacks have a job parked on
// elevated access: the admin rail's badge. One query over the waiting jobs.
func (o *Orchestrator) HostGrantsWaiting(ctx context.Context) (int, error) {
	waiting, err := o.jobRows.List(ctx, job.Waiting)
	if err != nil {
		return 0, err
	}
	stacks := map[string]bool{}
	for _, j := range waiting {
		var p struct {
			HostAccess *hostAccess `json:"host_access"`
		}
		if json.Unmarshal([]byte(j.Payload), &p) == nil && p.HostAccess != nil {
			stacks[p.HostAccess.Stack] = true
		}
	}
	return len(stacks), nil
}

package service

import (
	"context"
	"encoding/json"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
)

// RolloutStop is the env's newest promote when it failed, until a promote
// after it does not or the tile it stopped at deploys again. A superseded,
// cancelled or skipped one says nothing; a queued or running one is a
// retry under way. A later release on the env ends it too.
func (o *Orchestrator) RolloutStop(ctx context.Context, envID string) (RolloutStop, bool) {
	js, err := o.store.Jobs.ListTouching(ctx, []string{"env:" + envID}, string(kindPromote), 10) // newest first
	if err != nil {
		return RolloutStop{}, false
	}
	for _, j := range js {
		switch j.State {
		case job.Superseded, job.Cancelled, job.Skipped:
			continue
		case job.Failed:
			st := RolloutStop{JobID: j.ID, Error: j.Error, At: j.CreatedAt}
			if j.FinishedAt != nil {
				st.At = *j.FinishedAt
			}
			var p struct {
				StoppedAt   string   `json:"stopped_at"`
				NotDeployed []string `json:"not_deployed"`
			}
			_ = json.Unmarshal([]byte(j.Payload), &p)
			st.Tile, st.Pending = p.StoppedAt, p.NotDeployed
			if j.ReleaseID != nil {
				if r, err := o.releases.Get(ctx, *j.ReleaseID); err == nil {
					st.Release = r.Number
				}
			}
			// a later release on the env (a push, a rollback, another promote) retires it
			if e, err := o.envs.Get(ctx, envID); err == nil && e.ReleaseID != nil {
				if r, err := o.releases.Get(ctx, *e.ReleaseID); err == nil && r.Number > st.Release {
					return RolloutStop{}, false
				}
			}
			if t, err := o.tiles.GetBySlug(ctx, envID, st.Tile); err == nil && j.FinishedAt != nil {
				if d, ok, _ := o.jobRows.LastDone(ctx, t.ID, string(kindDeploy)); ok && d.FinishedAt != nil && d.FinishedAt.After(*j.FinishedAt) {
					return RolloutStop{}, false // the tile it stopped at deployed since
				}
			}
			return st, true
		}
		return RolloutStop{}, false
	}
	return RolloutStop{}, false
}

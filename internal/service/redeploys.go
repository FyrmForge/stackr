package service

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
)

// redeploys is how a plan apply finds out what became of the deploys its
// cascade change queued: the apply puts a collector on its ctx, o.redeploy
// notes each queued job in it, and the apply waits for them at the end.
type redeploys struct {
	mu   sync.Mutex
	list []Redeploy
}

type redeploysKey struct{}

func withRedeploys(ctx context.Context) (context.Context, *redeploys) {
	q := &redeploys{}
	return context.WithValue(ctx, redeploysKey{}, q), q
}

func noteRedeploy(ctx context.Context, r Redeploy) {
	if q, ok := ctx.Value(redeploysKey{}).(*redeploys); ok {
		q.mu.Lock()
		q.list = append(q.list, r)
		q.mu.Unlock()
	}
}

// redeployPoll is how often an apply looks at the deploys it queued.
var redeployPoll = 100 * time.Millisecond

// await waits for the redeploys the apply queued and fails when any failed,
// naming them, so an apply never reads done over failed deploys. A parked
// deploy (waiting on a param) is not waited for.
// ponytail: it holds a job worker while it waits, so with one worker (the
// deploys could never start) it only lists what it queued; two applies
// waiting at once on two workers stall until the 30 minute cap.
func (q *redeploys) await(ctx context.Context, o *Orchestrator, log io.Writer) error {
	q.mu.Lock()
	list := q.list
	q.mu.Unlock()
	if len(list) == 0 {
		return nil
	}
	if n, err := o.settings.Int(ctx, "workers"); err == nil && n < 2 {
		_, _ = fmt.Fprintf(log, "%s queued; with one job worker this apply cannot wait for them\n", plural(len(list), "redeploy"))
		return nil
	}
	_, _ = fmt.Fprintf(log, "waiting for %s\n", plural(len(list), "redeploy"))
	var failed []string
	for _, rd := range list {
		j, err := o.jobRows.Get(ctx, rd.Job)
		for ; err == nil && (j.State == job.Queued || j.State == job.Running); j, err = o.jobRows.Get(ctx, rd.Job) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(redeployPoll):
			}
		}
		if err != nil {
			return err
		}
		if j.State == job.Failed {
			failed = append(failed, fmt.Sprintf("%s/%s: %s", rd.Env, rd.Tile, j.Error))
			_, _ = fmt.Fprintf(log, "redeploy %s/%s failed: %s\n", rd.Env, rd.Tile, j.Error)
		}
	}
	if len(failed) == 0 {
		return nil
	}
	return fmt.Errorf("%s failed: %s", plural(len(failed), "redeploy"), strings.Join(failed, "; "))
}

// reachedByRedeploy says whether a cascade or param change redeploys t: it
// has containers, or a deploy that worked was followed by one that failed
// and left it with none. A tile that never ran, a run-to-completion tile
// and a slice are not redeployed.
func (o *Orchestrator) reachedByRedeploy(ctx context.Context, t Tile) (bool, error) {
	cs, err := o.tiles.Replicas(ctx, t)
	if err != nil || len(cs) > 0 {
		return len(cs) > 0, err
	}
	if t.Kind == tile.Slice || tile.RunToCompletion(t.Kind) {
		return false, nil
	}
	j, ok, err := o.jobRows.Last(ctx, t.ID)
	if err != nil || !ok || j.Kind != string(kindDeploy) || j.State != job.Failed {
		return false, err
	}
	_, ok, err = o.jobRows.LastDone(ctx, t.ID, string(kindDeploy))
	return ok, err
}

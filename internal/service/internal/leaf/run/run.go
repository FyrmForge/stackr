// Package run owns runs: one row per run of a cron or function tile, and
// the run's log file under <dir>/<tile_id>/<run_id>.log (its world object).
// Keep-last-50 pruning, the row's lifecycle (queued → running → ok | failed
// | cancelled) and the next tick of a schedule. The container is the flow's.
package run

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/robfig/cron/v3"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Statuses.
const (
	Queued    = "queued"
	Running   = "running"
	OK        = "ok"
	Failed    = "failed"
	Cancelled = "cancelled"
)

// Triggers.
const (
	Schedule = "schedule"
	Manual   = "manual"
	Deploy   = "deploy"
)

// Keep is how many runs a tile keeps; LogCap how much of a run's log.
const (
	Keep   = 50
	LogCap = 1 << 20
)

type Leaf struct {
	runs store.RunStore
	dir  string // $DATA_DIR/runs
	// mu makes each read-modify-write of a row atomic: the enqueuer's
	// SetJob and the worker's Begin/Finish race on the same row.
	// ponytail: one lock for every run; per-row if it ever shows up.
	mu sync.Mutex
}

func New(runs store.RunStore, dir string) *Leaf { return &Leaf{runs: runs, dir: dir} }

// Start writes a queued run and prunes the tile past Keep (row and log).
func (l *Leaf) Start(ctx context.Context, tileID string, releaseID *string, trigger string) (store.Run, error) {
	r := store.Run{
		ID:        uuid.NewString(),
		TileID:    tileID,
		ReleaseID: releaseID,
		Trigger:   trigger,
		Status:    Queued,
		CreatedAt: time.Now().UTC(),
	}
	if err := l.runs.Create(ctx, r); err != nil {
		return r, err
	}
	return r, l.prune(ctx, tileID)
}

// SetJob records the job that carries the run.
func (l *Leaf) SetJob(ctx context.Context, id, jobID string) (store.Run, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	r, err := l.runs.Get(ctx, id)
	if err != nil {
		return r, err
	}
	r.JobID = jobID
	return r, l.runs.Update(ctx, r)
}

// Begin moves a queued run to running; a run closed meanwhile (stopped
// while queued) comes back as it is.
func (l *Leaf) Begin(ctx context.Context, id string) (store.Run, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	r, err := l.runs.Get(ctx, id)
	if err != nil || Done(r) {
		return r, err
	}
	now := time.Now().UTC()
	r.Status, r.StartedAt = Running, &now
	return r, l.runs.Update(ctx, r)
}

// Finish closes a run. exit nil = the container never exited (timeout,
// cancel, no container). A run already closed stays as it is.
func (l *Leaf) Finish(ctx context.Context, id string, exit *int, status, reason string) (store.Run, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	r, err := l.runs.Get(ctx, id)
	if err != nil || Done(r) {
		return r, err
	}
	now := time.Now().UTC()
	r.ExitCode = exit
	r.Status = status
	r.Reason = reason
	r.FinishedAt = &now
	return r, l.runs.Update(ctx, r)
}

// Done: the run is closed.
func Done(r store.Run) bool { return r.Status != Queued && r.Status != Running }

// Get is the run if it belongs to tileID; another tile's run is not found.
// tileID "" skips the check (the worker, which holds only the run id).
func (l *Leaf) Get(ctx context.Context, tileID, id string) (store.Run, error) {
	r, err := l.runs.Get(ctx, id)
	if err == nil && tileID != "" && r.TileID != tileID {
		return store.Run{}, errs.ErrNotFound
	}
	return r, err
}

// List is the tile's runs, newest first; limit ≤ 0 = all kept.
func (l *Leaf) List(ctx context.Context, tileID string, limit int) ([]store.Run, error) {
	rs, err := l.runs.ListByTile(ctx, tileID)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(rs, func(a, b store.Run) int { return b.CreatedAt.Compare(a.CreatedAt) })
	if limit > 0 && len(rs) > limit {
		rs = rs[:limit]
	}
	return rs, nil
}

// Last is the tile's newest run.
func (l *Leaf) Last(ctx context.Context, tileID string) (store.Run, bool, error) {
	rs, err := l.List(ctx, tileID, 1)
	if err != nil || len(rs) == 0 {
		return store.Run{}, false, err
	}
	return rs[0], true, nil
}

// Active is the tile's queued or running run, if any.
func (l *Leaf) Active(ctx context.Context, tileID string) (store.Run, bool, error) {
	rs, err := l.List(ctx, tileID, 0)
	i := slices.IndexFunc(rs, func(r store.Run) bool { return !Done(r) })
	if i < 0 {
		return store.Run{}, false, err
	}
	return rs[i], true, err
}

// Interrupted fails every run left running (the process died under it);
// boot calls it before the job runner starts.
func (l *Leaf) Interrupted(ctx context.Context) error {
	rs, err := l.runs.ListByStatus(ctx, Running)
	for _, r := range rs {
		if _, err := l.Finish(ctx, r.ID, nil, Failed, "stackrd restarted while this run was going"); err != nil {
			return err
		}
	}
	return err
}

// Next is the schedule's next tick after from. Pure.
func Next(expr string, from time.Time) (time.Time, error) {
	s, err := cron.ParseStandard(expr)
	if err != nil {
		return time.Time{}, err
	}
	return s.Next(from), nil
}

// prune drops the tile's runs past Keep, oldest first, with their logs.
func (l *Leaf) prune(ctx context.Context, tileID string) error {
	rs, err := l.List(ctx, tileID, 0)
	if err != nil || len(rs) <= Keep {
		return err
	}
	for _, r := range rs[Keep:] {
		if !Done(r) {
			continue // a run still going keeps its row and log
		}
		if err := l.runs.Delete(ctx, r.ID); err != nil && !errors.Is(err, errs.ErrNotFound) {
			return err
		}
		if err := os.Remove(l.LogPath(r)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// DropTile removes the tile's log directory; the rows go with the tile
// (ON DELETE CASCADE).
func (l *Leaf) DropTile(tileID string) error { return os.RemoveAll(filepath.Join(l.dir, tileID)) }

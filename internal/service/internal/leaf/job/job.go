// Package job owns the jobs table: the states, what they mean, and the lock
// set. flow/jobs is the only caller that moves a row between states.
package job

import (
	"context"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

const (
	Queued     = "queued"
	Running    = "running"
	Waiting    = "waiting" // parked on an unset param; resumes itself
	Done       = "done"
	Failed     = "failed"
	Superseded = "superseded" // a newer job took over; not a failure
	Cancelled  = "cancelled"  // someone pressed stop
)

// Live is "still going to happen". A parked job is live.
func Live(state string) bool {
	switch state {
	case Queued, Running, Waiting:
		return true
	}
	return false
}

// Terminal is the exact complement of Live, never its own list.
func Terminal(state string) bool { return !Live(state) }

// Cancellable without touching a running context.
func Cancellable(state string) bool { return state == Queued || state == Waiting }

// LockSet is the one function that turns the tiles a job touches into its
// lock: sorted, deduplicated. A priority lane later changes only this.
func LockSet(tileIDs ...string) []string {
	s := slices.Clone(tileIDs)
	slices.Sort(s)
	return slices.Compact(s)
}

// Overlaps reports whether two lock sets share a tile.
func Overlaps(a, b []string) bool {
	for _, t := range a {
		if slices.Contains(b, t) {
			return true
		}
	}
	return false
}

// Supersedes is the Railway rule's other half, next to the lock key: a newer
// job replaces an older one only when it is the same kind and redoes every
// tile of it (DECIDE 16). Anything else queues behind.
func Supersedes(newer, older store.Job) bool {
	if newer.Kind != older.Kind {
		return false
	}
	for _, t := range older.LockSet {
		if !slices.Contains(newer.LockSet, t) {
			return false
		}
	}
	return true
}

type Leaf struct{ jobs store.JobStore }

func New(jobs store.JobStore) *Leaf { return &Leaf{jobs: jobs} }

func (l *Leaf) Get(ctx context.Context, id string) (store.Job, error) { return l.jobs.Get(ctx, id) }

func (l *Leaf) List(ctx context.Context, states ...string) ([]store.Job, error) {
	js, err := l.jobs.ListByState(ctx, states...)
	slices.SortFunc(js, func(a, b store.Job) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return js, err
}

// Create writes a queued job; logPath names its output file.
func (l *Leaf) Create(ctx context.Context, kind string, lock []string, payload string, releaseID *string, logDir string) (store.Job, error) {
	id := uuid.NewString()
	j := store.Job{
		ID: id, Kind: kind, State: Queued, ReleaseID: releaseID, LockSet: LockSet(lock...),
		Payload: payload, LogPath: logDir + "/" + id + ".log", CreatedAt: time.Now().UTC(),
	}
	return j, l.jobs.Create(ctx, j)
}

// Start marks a job running.
func (l *Leaf) Start(ctx context.Context, j store.Job) (store.Job, error) {
	now := time.Now().UTC()
	j.State, j.StartedAt, j.WaitingParam, j.Error = Running, &now, nil, ""
	return j, l.jobs.Update(ctx, j)
}

// Finish writes a terminal state, with the reason when there is one.
func (l *Leaf) Finish(ctx context.Context, j store.Job, state, reason string) error {
	now := time.Now().UTC()
	j.State, j.Error, j.FinishedAt = state, reason, &now
	return l.jobs.Update(ctx, j)
}

// Park sets a job waiting on a param.
func (l *Leaf) Park(ctx context.Context, j store.Job, param string) error {
	j.State, j.WaitingParam = Waiting, &param
	return l.jobs.Update(ctx, j)
}

// Requeue puts a waiting job back in line, at its original age.
func (l *Leaf) Requeue(ctx context.Context, j store.Job) error {
	j.State, j.WaitingParam = Queued, nil
	return l.jobs.Update(ctx, j)
}

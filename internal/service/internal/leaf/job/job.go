// Package job owns the jobs table: the states, what they mean, and the lock
// set. flow/jobs is the only caller that moves a row between states.
package job

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
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

// History is the newest jobs touching any of tileIDs (one tile, or every
// tile of an env), newest first, at most limit.
func (l *Leaf) History(ctx context.Context, tileIDs []string, limit int) ([]store.Job, error) {
	return l.jobs.ListTouching(ctx, tileIDs, "", limit)
}

// Last is the newest job touching the tile, in any state: what the
// orchestrator layers over tile.State (DECIDE 8). ok=false: none yet.
func (l *Leaf) Last(ctx context.Context, tileID string) (store.Job, bool, error) {
	js, err := l.jobs.ListTouching(ctx, []string{tileID}, "", 1)
	if err != nil || len(js) == 0 {
		return store.Job{}, false, err
	}
	return js[0], true, nil
}

// LastDone is the tile's newest finished job of kind; with the deploy kind
// that is "what does this tile run" (its release_id).
// ponytail: scans the newest 100 of that kind; a tile with 100 failures in a
// row reads as never deployed. Push the state into the query if that bites.
func (l *Leaf) LastDone(ctx context.Context, tileID, kind string) (store.Job, bool, error) {
	js, err := l.jobs.ListTouching(ctx, []string{tileID}, kind, 100)
	for _, j := range js {
		if j.State == Done {
			return j, true, err
		}
	}
	return store.Job{}, false, err
}

// MaxChunk caps one poll's slice of the log.
const MaxChunk = 64 << 10

// Log is one poll's slice of a job's log. Next is the offset to ask for
// next; End means the job is terminal and the whole log has been read, so
// the client can stop polling.
type Log struct {
	Chunk []byte
	Next  int64
	End   bool
}

// Poll is the job row plus its log from offset (B25: every job is a row to
// poll). A log that does not exist yet reads as empty.
func (l *Leaf) Poll(ctx context.Context, id string, offset int64) (store.Job, Log, error) {
	j, err := l.jobs.Get(ctx, id)
	if err != nil {
		return j, Log{}, err
	}
	out := Log{Next: offset}
	f, err := os.Open(j.LogPath)
	if errors.Is(err, fs.ErrNotExist) {
		out.End = Terminal(j.State)
		return j, out, nil
	}
	if err != nil {
		return j, out, err
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, MaxChunk)
	n, err := f.ReadAt(buf, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return j, out, err
	}
	out.Chunk, out.Next = buf[:n], offset+int64(n)
	// The state was read before the file: a terminal job has written its last line.
	out.End = Terminal(j.State) && errors.Is(err, io.EOF)
	return j, out, nil
}

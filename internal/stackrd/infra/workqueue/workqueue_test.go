package workqueue

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// waitFor polls until cond or the deadline. The runner is asynchronous by
// design, so a test that reads the row once reads it too early.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// A job runs, and its outcome lands on the row. The whole point of the table:
// the answer survives the thing that asked for it.
func TestAJobRunsAndRecordsWhatHappened(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	q := New(store)

	ran := make(chan string, 4)
	q.Register("ok", func(ctx context.Context, j *Job) error {
		var p struct {
			Who string `json:"who"`
		}
		require.NoError(t, j.Payload(&p))
		j.SetStep(ctx, "halfway")
		ran <- p.Who
		return nil
	}, KindOpts{})
	q.Register("boom", func(context.Context, *Job) error {
		return errors.New("it broke")
	}, KindOpts{})
	q.Register("panics", func(context.Context, *Job) error {
		panic("oh no")
	}, KindOpts{})

	q.Start(ctx)

	id, err := q.Enqueue(ctx, "ok", "stack-1", map[string]string{"who": "alice"})
	require.NoError(t, err)
	assert.Equal(t, "alice", <-ran, "the payload reaches the handler")

	waitFor(t, "the job to finish", func() bool {
		w, _ := store.GetWorkItem(ctx, id)
		return w != nil && w.Status == "done"
	})
	w, err := store.GetWorkItem(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "halfway", w.Step, "SetStep writes through to the row")
	assert.Equal(t, 1, w.Attempts)
	assert.True(t, w.StartedAt.Valid && w.FinishedAt.Valid)

	bad, err := q.Enqueue(ctx, "boom", "", nil)
	require.NoError(t, err)
	waitFor(t, "the failure to land", func() bool {
		w, _ := store.GetWorkItem(ctx, bad)
		return w != nil && w.Status == "error"
	})
	w, _ = store.GetWorkItem(ctx, bad)
	assert.Equal(t, "it broke", w.Error, "the reason is on the row, not only in a log")

	// A handler that panics must not take the panel down with it, and the row
	// is the only place anyone would find out.
	boom, err := q.Enqueue(ctx, "panics", "", nil)
	require.NoError(t, err)
	waitFor(t, "the panic to be recorded", func() bool {
		w, _ := store.GetWorkItem(ctx, boom)
		return w != nil && w.Status == "error"
	})
	w, _ = store.GetWorkItem(ctx, boom)
	assert.Contains(t, w.Error, "panic")

	// An unregistered kind is refused at enqueue: a row nothing can run would
	// sit queued forever.
	_, err = q.Enqueue(ctx, "nope", "", nil)
	assert.Error(t, err)
}

// Two pushes a second apart must not both apply. The older waiting item is
// superseded by the newer one, per kind and key.
func TestEnqueueSupersedesTheOlderWaitingItem(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	q := New(store)
	q.Register("slow", func(context.Context, *Job) error { return nil }, KindOpts{})

	first, err := q.Enqueue(ctx, "slow", "stack-1", nil)
	require.NoError(t, err)
	second, err := q.Enqueue(ctx, "slow", "stack-1", nil)
	require.NoError(t, err)
	other, err := q.Enqueue(ctx, "slow", "stack-2", nil)
	require.NoError(t, err)

	w, _ := store.GetWorkItem(ctx, first)
	assert.Equal(t, "superseded", w.Status, "the older item for the same stack steps aside")
	w, _ = store.GetWorkItem(ctx, second)
	assert.Equal(t, "queued", w.Status)
	w, _ = store.GetWorkItem(ctx, other)
	assert.Equal(t, "queued", w.Status, "a different key is different work")

	// An empty key means "never dedupe": two of these are two real jobs.
	a, _ := q.Enqueue(ctx, "slow", "", nil)
	_, _ = q.Enqueue(ctx, "slow", "", nil)
	w, _ = store.GetWorkItem(ctx, a)
	assert.NotEqual(t, "superseded", w.Status)
}

// The reason the queue is a table. A row left running by a process that is
// gone is either run again or failed on purpose, per kind, instead of sitting
// there forever looking busy.
func TestBootRecoveryDealsWithWhatTheLastRunLeft(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)

	// Enqueue and claim without ever running, which is exactly the state a
	// crash leaves behind.
	seed := New(store)
	seed.Register("convergent", func(context.Context, *Job) error { return nil }, KindOpts{})
	seed.Register("one-shot", func(context.Context, *Job) error { return nil }, KindOpts{})
	conv, err := seed.Enqueue(ctx, "convergent", "", nil)
	require.NoError(t, err)
	once, err := seed.Enqueue(ctx, "one-shot", "", nil)
	require.NoError(t, err)
	gone, err := seed.Enqueue(ctx, "convergent", "", nil)
	require.NoError(t, err)
	for _, id := range []string{conv, once, gone} {
		claimed, cerr := store.ClaimWorkItem(ctx, id)
		require.NoError(t, cerr)
		require.True(t, claimed)
	}

	cleaned := false
	q := New(store)
	q.Register("convergent", func(context.Context, *Job) error { return nil },
		KindOpts{OnRestart: Requeue})
	q.Register("one-shot", func(context.Context, *Job) error { return nil }, KindOpts{
		OnRestart:   Fail,
		RestartFail: "a half-copied volume cannot be resumed",
		Cleanup:     func(context.Context, *Job) { cleaned = true },
	})
	// "gone" is deliberately left registered so both convergent rows requeue;
	// the unregistered case is checked below with its own kind.
	q.recover(ctx)

	w, _ := store.GetWorkItem(ctx, conv)
	assert.Equal(t, "queued", w.Status, "convergent work runs again from the top")
	assert.True(t, w.StartedAt.Valid == false, "a requeued row has not started")

	w, _ = store.GetWorkItem(ctx, once)
	assert.Equal(t, "error", w.Status, "work that cannot be resumed blind is failed")
	assert.Equal(t, "a half-copied volume cannot be resumed", w.Error,
		"the message has to say what to do, not just that the panel restarted")
	assert.True(t, cleaned, "Cleanup runs before the row is closed")
}

// A kind nobody registers any more cannot be run or reasoned about, and left
// running it would hide from every listing.
func TestRecoveryFailsWorkWithNoHandlerLeft(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	seed := New(store)
	seed.Register("retired", func(context.Context, *Job) error { return nil }, KindOpts{})
	id, err := seed.Enqueue(ctx, "retired", "", nil)
	require.NoError(t, err)
	claimed, err := store.ClaimWorkItem(ctx, id)
	require.NoError(t, err)
	require.True(t, claimed)

	New(store).recover(ctx)
	w, _ := store.GetWorkItem(ctx, id)
	assert.Equal(t, "error", w.Status)
	assert.Contains(t, w.Error, "retired")
}

// Claiming is the lock. Two claimers race on one UPDATE and exactly one wins,
// which is what stops the same job running twice.
func TestOnlyOneClaimerWins(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	q := New(store)
	q.Register("k", func(context.Context, *Job) error { return nil }, KindOpts{})
	id, err := q.Enqueue(ctx, "k", "", nil)
	require.NoError(t, err)

	first, err := store.ClaimWorkItem(ctx, id)
	require.NoError(t, err)
	second, err := store.ClaimWorkItem(ctx, id)
	require.NoError(t, err)
	assert.True(t, first)
	assert.False(t, second, "a running item cannot be claimed again")
}

// Cancel stops work in flight, and the row says so rather than reading as a
// failure somebody has to investigate.
func TestCancelStopsARunningJob(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	q := New(store)
	started := make(chan struct{})
	q.Register("long", func(ctx context.Context, j *Job) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}, KindOpts{})
	q.Start(ctx)

	id, err := q.Enqueue(ctx, "long", "", nil)
	require.NoError(t, err)
	<-started
	require.NoError(t, q.Cancel(ctx, id))
	waitFor(t, "the cancel to land", func() bool {
		w, _ := store.GetWorkItem(ctx, id)
		return w != nil && w.Status == "cancelled"
	})
}

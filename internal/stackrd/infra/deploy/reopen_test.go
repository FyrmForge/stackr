package deploy

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// The boot sweep errors everything the last process was driving, because with
// an in-memory queue nothing would ever retry it. On the work queue something
// does, so a requeued item has to put its deployment row back where run()
// expects it.
//
// What must NOT be put back is anything a person decided in the meantime: a
// cancel, or a supersede by a newer deploy. Reopening one of those would ship
// a commit somebody had already replaced.
func TestReopenOnlyRevivesWhatTheRestartInterrupted(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	e := &Engine{store: store, rows: storeRows{s: store}}
	// A deployment references a real tile; SeedStack gives us one.
	seed := testdb.SeedStack(t, store, true)

	mk := func(id, status, errMsg string) string {
		require.NoError(t, store.CreateDeployment(ctx, &repo.Deployment{
			ID: id, TileID: seed.Tile.ID, Status: status, Error: errMsg,
			Trigger: "manual", CreatedAt: time.Now().UTC(),
		}))
		return id
	}
	statusOf := func(id string) string {
		d, err := store.GetDeployment(ctx, id)
		require.NoError(t, err)
		return d.Status
	}

	swept := mk("d-swept", "error", repo.InterruptedMsg)
	stillRunning := mk("d-running", "running", "")
	cancelled := mk("d-cancelled", "cancelled", "superseded by a newer deploy")
	realFailure := mk("d-failed", "error", "build: exit status 1")
	finished := mk("d-done", "done", "")

	for _, id := range []string{swept, stillRunning, cancelled, realFailure, finished} {
		require.NoError(t, e.reopen(ctx, id))
	}

	assert.Equal(t, "queued", statusOf(swept), "the sweep's own message means the restart did it")
	assert.Equal(t, "queued", statusOf(stillRunning), "a row left running has no process behind it")
	assert.Equal(t, "cancelled", statusOf(cancelled), "a decision is not reopened")
	assert.Equal(t, "error", statusOf(realFailure), "a genuine build failure is not retried behind anyone's back")
	assert.Equal(t, "done", statusOf(finished))

	// The revived row must carry no stale failure, or the UI shows a reason
	// for a deploy that is about to run.
	d, err := store.GetDeployment(ctx, swept)
	require.NoError(t, err)
	assert.Empty(t, d.Error)
	assert.False(t, d.FinishedAt.Valid)

	assert.NoError(t, e.reopen(ctx, "no-such-deployment"), "a missing row is not an error")
}

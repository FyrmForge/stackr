package jobs

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/workqueue"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// Stop finds a run's work item by the run id. A run still waiting never
// starts, so its row is closed as stopped right there; a run that is over
// answers false.
func TestStopAWaitingRun(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, false)
	q := workqueue.New(store) // never started, so the item stays queued
	s := NewService(store, nil, nil, nil).WithWork(q)

	assert.False(t, s.Stop(ctx, "nobody"), "stopped a run that does not exist")

	run, err := s.StartApp(ctx, seed.Tile.ID, TriggerManual, "tester")
	require.NoError(t, err)
	require.True(t, s.Stop(ctx, run.ID), "Stop found no run to cancel")

	got, err := store.GetCronRun(ctx, run.ID)
	require.NoError(t, err)
	assert.Equal(t, "stopped", got.Status)
	assert.True(t, got.FinishedAt.Valid, "the waiting run's row was left open")
	assert.False(t, s.Stop(ctx, run.ID), "a stopped run is still stoppable")
}

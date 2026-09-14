package sqlite_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// Dedupe used to be "supersede everything but me", and ids are UUIDs, so two
// enqueues landing together each superseded the other: both rows ended up
// superseded and neither job ever ran. Older only, by rowid, and the later
// enqueue is the one that survives.
func TestSupersedeOnlyDropsOlderQueuedItems(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)

	now := time.Now().UTC()
	for _, id := range []string{"w1", "w2"} {
		require.NoError(t, store.CreateWorkItem(ctx, &repo.WorkItem{
			ID: id, Kind: "apply", DedupeKey: "stack1", Status: "queued", CreatedAt: now,
		}), "create %s", id)
	}

	// Both enqueues run their own supersede, in either order.
	require.NoError(t, store.SupersedeQueuedWorkItems(ctx, "apply", "stack1", "w2"))
	require.NoError(t, store.SupersedeQueuedWorkItems(ctx, "apply", "stack1", "w1"))

	first, err := store.GetWorkItem(ctx, "w1")
	require.NoError(t, err)
	second, err := store.GetWorkItem(ctx, "w2")
	require.NoError(t, err)
	assert.Equal(t, "superseded", first.Status, "the earlier row should be dropped")
	assert.Equal(t, "queued", second.Status,
		"the later row was superseded by the earlier one, so nothing would run")
}

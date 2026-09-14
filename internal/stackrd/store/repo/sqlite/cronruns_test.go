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

// A run row is inserted before the work starts, so the panel can show it while
// it is happening; FinishCronRun closes it without touching what it recorded.
func TestCronRunOpensThenFinishes(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)

	r := &repo.CronRun{ID: "run1", Ref: "app:tile1", Status: "running",
		Trigger: "manual web", Actor: "admin@test.com", StartedAt: time.Now().UTC()}
	require.NoError(t, store.CreateCronRun(ctx, r), "create")

	got, err := store.ListCronRuns(ctx, "app:tile1", 10)
	require.NoError(t, err, "list")
	require.Len(t, got, 1, "want one run")
	assert.True(t, got[0].Running(), "a run with no finished_at must read as running")
	assert.Equal(t, "manual web", got[0].Trigger, "trigger = %q", got[0].Trigger)
	assert.Equal(t, "admin@test.com", got[0].Actor, "actor = %q", got[0].Actor)

	r.Status, r.Output = "ok", "done\n"
	r.FinishedAt.Time, r.FinishedAt.Valid = time.Now().UTC(), true
	require.NoError(t, store.FinishCronRun(ctx, r), "finish")

	got, err = store.ListCronRuns(ctx, "app:tile1", 10)
	require.NoError(t, err, "list 2")
	require.Len(t, got, 1, "finish inserted a second row")
	assert.False(t, got[0].Running(), "still reads as running after finish")
	assert.Equal(t, "ok", got[0].Status, "status = %q", got[0].Status)
	assert.Equal(t, "done\n", got[0].Output, "output = %q", got[0].Output)
	assert.Equal(t, "manual web", got[0].Trigger, "finish stomped the trigger")
}

// The panel header asks OpenCronRun whether a cron is mid-run: tiles.status
// never says "running" for one. A closed row must not answer, or the header
// says running forever and offers a Stop for work that is over.
func TestOpenCronRun(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)

	open, err := store.OpenCronRun(ctx, "app:tile1")
	require.NoError(t, err, "open on an empty table")
	assert.Nil(t, open, "no runs, yet one came back")

	r := &repo.CronRun{ID: "run1", Ref: "app:tile1", Status: "running", StartedAt: time.Now().UTC()}
	require.NoError(t, store.CreateCronRun(ctx, r), "create")

	open, err = store.OpenCronRun(ctx, "app:tile1")
	require.NoError(t, err, "open")
	require.NotNil(t, open, "an open row was not found")
	assert.Equal(t, "run1", open.ID, "id = %q", open.ID)

	all, err := store.ListOpenCronRuns(ctx)
	require.NoError(t, err, "list open")
	require.Len(t, all, 1, "the canvas query missed the open run")

	// Another tile's open run must not answer for this one.
	other := &repo.CronRun{ID: "run2", Ref: "app:tile2", Status: "running", StartedAt: time.Now().UTC()}
	require.NoError(t, store.CreateCronRun(ctx, other), "create other")

	r.Status = "stopped"
	r.FinishedAt.Time, r.FinishedAt.Valid = time.Now().UTC(), true
	require.NoError(t, store.FinishCronRun(ctx, r), "finish")

	open, err = store.OpenCronRun(ctx, "app:tile1")
	require.NoError(t, err, "open after finish")
	assert.Nil(t, open, "a finished run still reads as open")

	all, err = store.ListOpenCronRuns(ctx)
	require.NoError(t, err, "list open 2")
	require.Len(t, all, 1, "want only the other tile's run")
	assert.Equal(t, "run2", all[0].ID, "id = %q", all[0].ID)
}

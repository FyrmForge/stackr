package orgconf

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/workqueue"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// A queued apply that fails leaves the reason on the plan row. That is the
// whole reason the apply moved off the request: Apply writes the row on two of
// its exit paths and returns bare on the rest, and inline the rest were only
// ever seen as a flash message on a response that a dead request never sent.
func TestAFailedApplyLandsOnThePlanRow(t *testing.T) {
	ctx := context.Background()
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false) // not config-managed: Load refuses
	now := time.Now().UTC()

	cp := &repo.ConfigPlan{
		ID:        "plan-1",
		StackID:   seed.Org.ID, // the org id rides StackID on an org plan
		Summary:   "1 change",
		Status:    "pending",
		Error:     "the previous run's message",
		CreatedAt: now,
	}
	require.NoError(t, s.CreateOrgConfigPlan(ctx, cp))

	q := workqueue.New(s, testdb.WorkItems{Store: s})
	RegisterApply(q, &Runner{Store: s})
	q.Start(ctx)

	id, err := EnqueueApply(ctx, q, seed.Org, cp)
	require.NoError(t, err)

	var item *repo.WorkItem
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		item, _ = s.GetWorkItem(ctx, id)
		if item != nil && item.Done() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	require.NotNil(t, item)
	require.True(t, item.Done(), "the job should have finished")
	assert.Equal(t, "error", item.Status)

	got, err := s.GetOrgConfigPlan(ctx, cp.ID)
	require.NoError(t, err)
	assert.NotEqual(t, "the previous run's message", got.Error,
		"the row still carries the previous run's message")
	assert.Contains(t, got.Error, "not bound")
	// And the row is decided, not left pending for ever waiting on a run that
	// already happened. The wizard reads this to tell "applied, move on" from
	// "failed, stay and show why".
	assert.Equal(t, "error", got.Status)
}

// Two approvals of one plan are one apply: the dedupe key is the plan, the
// same as the stack side's.
func TestOneApplyPerPlan(t *testing.T) {
	ctx := context.Background()
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)

	cp := &repo.ConfigPlan{
		ID: "plan-2", StackID: seed.Org.ID, Status: "pending",
		CreatedAt: time.Now().UTC(),
	}
	require.NoError(t, s.CreateOrgConfigPlan(ctx, cp))

	q := workqueue.New(s, testdb.WorkItems{Store: s})
	RegisterApply(q, &Runner{Store: s}) // registered, never started: rows only

	first, err := EnqueueApply(ctx, q, seed.Org, cp)
	require.NoError(t, err)
	second, err := EnqueueApply(ctx, q, seed.Org, cp)
	require.NoError(t, err)

	older, err := s.GetWorkItem(ctx, first)
	require.NoError(t, err)
	assert.Equal(t, "superseded", older.Status, "the earlier queued apply is dropped")
	newer, err := s.GetWorkItem(ctx, second)
	require.NoError(t, err)
	assert.Equal(t, "queued", newer.Status)
}

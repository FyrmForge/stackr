package sqlite_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// "Newest plan" is what the config panel and the plan API both read, and it
// means the last row inserted. It deliberately does not mean "highest
// created_at": that column is TEXT and holds more than one timestamp format
// (Go's writes on insert, CURRENT_TIMESTAMP on the status updates), so a string
// compare across formats can order two rows backwards. Plan ids are random
// uuids, so they are no tiebreak either.
//
// Both failure modes are wired in below: the ids descend, and the timestamps
// run *backwards* against insert order, so anything that sorts on created_at or
// falls back to id returns the oldest row instead of the newest.
func TestConfigPlanOrderingIsInsertOrder(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, true)

	now := time.Now().UTC()
	for i, id := range []string{"ccc", "bbb", "aaa"} {
		p := &repo.ConfigPlan{ID: id, StackID: seed.Stack.ID, Status: "pending",
			Summary: id, CreatedAt: now.Add(-time.Duration(i) * time.Hour)}
		require.NoError(t, store.CreateConfigPlan(ctx, p), "create %s", id)
	}

	latest, err := store.LatestConfigPlan(ctx, seed.Stack.ID)
	require.NoError(t, err, "latest")
	require.NotNil(t, latest, "latest = %v, want the last-inserted plan (aaa)", latest)
	require.Equal(t, "aaa", latest.ID, "latest = %v, want the last-inserted plan (aaa)", latest)

	list, err := store.ListConfigPlans(ctx, seed.Stack.ID, 10)
	require.NoError(t, err, "list")
	want := []string{"aaa", "bbb", "ccc"}
	require.Len(t, list, len(want))
	for i, id := range want {
		require.Equal(t, id, list[i].ID, "list[%d]", i)
	}
}

// A plan the operator never decided on must not stay at the top of the list
// once a newer one replaces it. Only 'applied' and 'rejected' are decisions.
// An 'error' row was surviving: the config file failed to parse, the operator
// fixed it and planned again, and the red row from the broken commit kept
// sitting above the good one looking current (the compose_inline error on
// 2026-09-11). 'clean' had the same shape, two "no changes" rows side by side.
func TestSupersedeReplacesEveryUndecidedPlan(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, true)
	now := time.Now().UTC()

	mk := func(id, envSlug, status string) {
		require.NoError(t, store.CreateConfigPlan(ctx, &repo.ConfigPlan{
			ID: id, StackID: seed.Stack.ID, EnvSlug: envSlug, Status: status, CreatedAt: now}))
	}
	status := func(id string) string {
		list, err := store.ListConfigPlans(ctx, seed.Stack.ID, 50)
		require.NoError(t, err)
		for _, p := range list {
			if p.ID == id {
				return p.Status
			}
		}
		t.Fatalf("plan %s missing", id)
		return ""
	}

	mk("undecided-pending", "", "pending")
	mk("undecided-clean", "", "clean")
	mk("undecided-error", "", "error")
	mk("decided-applied", "", "applied")
	mk("decided-rejected", "", "rejected")
	// Another scope entirely: an env plan is not replaced by a stack one.
	mk("other-scope", "production", "pending")

	require.NoError(t, store.SupersedePendingPlans(ctx, seed.Stack.ID, ""))

	for _, id := range []string{"undecided-pending", "undecided-clean", "undecided-error"} {
		require.Equal(t, "superseded", status(id), "%s should have been replaced", id)
	}
	require.Equal(t, "applied", status("decided-applied"), "an applied plan is history, not a draft")
	require.Equal(t, "rejected", status("decided-rejected"), "a rejected plan is history, not a draft")
	require.Equal(t, "pending", status("other-scope"), "a production plan survives a stack-scoped replan")
}

// The org banner counts stacks whose NEWEST plan is waiting. A stack with an old
// error plan and a newer applied one has nothing to decide, and a stack with
// three pending plans is still one stack.
func TestCountStacksAwaitingPlan(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, true)
	now := time.Now().UTC()
	other := &repo.Stack{ID: "st2", OrgID: seed.Org.ID, Name: "Two", Slug: "two", CreatedAt: now}
	require.NoError(t, store.CreateStack(ctx, other))
	mk := func(id, stack, status string) {
		require.NoError(t, store.CreateConfigPlan(ctx, &repo.ConfigPlan{ID: id, StackID: stack, Status: status, CreatedAt: now}))
	}

	n, err := store.CountStacksAwaitingPlan(ctx, seed.Org.ID)
	require.NoError(t, err)
	require.Equal(t, 0, n, "no plans, nothing waiting")

	mk("a1", seed.Stack.ID, "error")
	mk("a2", seed.Stack.ID, "applied") // moved past the error
	mk("b1", other.ID, "pending")
	mk("b2", other.ID, "pending")
	n, err = store.CountStacksAwaitingPlan(ctx, seed.Org.ID)
	require.NoError(t, err)
	require.Equal(t, 1, n, "only the stack whose newest plan is pending, counted once")

	mk("a3", seed.Stack.ID, "error")
	n, _ = store.CountStacksAwaitingPlan(ctx, seed.Org.ID)
	require.Equal(t, 2, n, "a fresh error plan puts the stack back in")
}

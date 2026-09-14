package sqlite_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// Staged changes replay oldest-first and later entries win (stackconf's apply
// walks the list in order), so this ordering is load-bearing: a mis-sorted row
// applies a stale edit over a newer one. Same trap as config_plans, created_at
// is TEXT and format-sensitive, so the timestamps here run backwards against
// insert order, and sorting on created_at returns the rows reversed.
func TestStagedChangesListInInsertOrder(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, true)

	now := time.Now().UTC()
	for i, id := range []string{"first", "second", "third"} {
		sc := &repo.StagedChange{ID: id, StackID: seed.Stack.ID, EnvID: seed.Env.ID,
			TileSlug: "app", Summary: id, Payload: "{}",
			CreatedAt: now.Add(-time.Duration(i) * time.Hour)}
		require.NoError(t, store.CreateStagedChange(ctx, sc), "create %s", id)
	}

	want := []string{"first", "second", "third"}
	for name, list := range map[string]func() ([]repo.StagedChange, error){
		"by env":   func() ([]repo.StagedChange, error) { return store.ListStagedByEnv(ctx, seed.Env.ID) },
		"by stack": func() ([]repo.StagedChange, error) { return store.ListStagedByStack(ctx, seed.Stack.ID) },
	} {
		got, err := list()
		require.NoError(t, err, "%s", name)
		require.Len(t, got, len(want), "%s", name)
		for i, id := range want {
			require.Equal(t, id, got[i].ID, "%s: [%d]", name, i)
		}
	}
}

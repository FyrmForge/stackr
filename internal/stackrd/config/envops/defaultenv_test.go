package envops

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// The default environment is the file's bottom rung (position 0), not whatever
// row is oldest. Applying production before staging existed used to make
// production the default, so it took the bare hostname and every later plan
// failed with "already routes to another service".
func TestDefaultEnvIDFollowsTheFileNotTheClock(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, true)
	now := time.Now().UTC()

	// seed's env is the oldest. Give it position 1 and add a younger env at
	// position 0, which is what a file listing staging first produces.
	seed.Env.Position = 1
	require.NoError(t, store.UpdateEnvironment(ctx, seed.Env))
	staging := &repo.Environment{ID: "env-staging", StackID: seed.Stack.ID, Name: "Staging",
		Slug: "staging", Type: "static", Position: 0, CreatedAt: now.Add(time.Hour)}
	require.NoError(t, store.CreateEnvironment(ctx, staging))
	// The home env holds stack-scoped instances and is never routed to, so it
	// must never win the position tie even though it sits at 0.
	if home, _ := store.GetEnvironmentBySlug(ctx, seed.Stack.ID, repo.HomeSlug); home != nil {
		home.Position = 0
		require.NoError(t, store.UpdateEnvironment(ctx, home))
	}

	o := Ops{Store: store}
	got, err := o.defaultEnvID(ctx, seed.Stack.ID)
	require.NoError(t, err)
	assert.Equal(t, staging.ID, got, "the file's bottom rung is the default, whatever was created first")
}

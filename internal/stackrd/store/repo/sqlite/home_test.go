package sqlite_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// A stack is born with its home; the ladder never lists it.
func TestHomeEnvironment(t *testing.T) {
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, false)
	ctx := context.Background()

	home, err := store.HomeEnvironment(ctx, seed.Stack.ID)
	require.NoError(t, err)
	require.NotNil(t, home, "CreateStack must create the home")
	assert.Equal(t, repo.HomeSlug, home.Slug)
	assert.Equal(t, repo.HomeSlug, home.Type)

	envs, err := store.ListEnvironmentsByStack(ctx, seed.Stack.ID)
	require.NoError(t, err)
	for _, e := range envs {
		assert.NotEqual(t, repo.HomeSlug, e.Slug, "the home is on the ladder: %+v", envs)
	}
	assert.Len(t, envs, 1)

	// Reserved: the unique (stack, slug) index refuses a second "stack".
	err = store.CreateEnvironment(ctx, &repo.Environment{ID: "x", StackID: seed.Stack.ID, Name: "Stack", Slug: repo.HomeSlug, Type: "static"})
	assert.Error(t, err)
}

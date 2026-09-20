package envops_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// Variables have no foreign key to environments, so nothing in the database
// removes an env's rows when the env goes, only Teardown's own loop does. A PR
// env mints its own generated secrets, so without that loop every closed PR
// leaves a live secret behind under an owner id that no longer resolves, and
// they accumulate silently for as long as the panel runs.
//
// This is the half of teardown that needs no docker: RT and PX are left nil.
func TestTeardownDeletesTheEnvsVariables(t *testing.T) {
	ctx := context.Background()
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)
	now := time.Now().UTC()

	pr := &repo.Environment{ID: "env-pr3", StackID: seed.Stack.ID, Name: "pr-3", Slug: "pr-3",
		Type: "ephemeral", BaseEnvID: seed.Env.ID, CreatedAt: now}
	require.NoError(t, s.CreateEnvironment(ctx, pr), "create env")
	// The PR env's own minted secret, plus a row on the env it was built from,
	// the second one is the check that teardown deletes by owner, not by name.
	for _, v := range []repo.Variable{
		{OwnerID: pr.ID, Name: "PREVIEW_TOKEN", Value: "minted", Secret: true},
		{OwnerID: pr.ID, Name: "LOG_LEVEL", Value: "debug"},
		{OwnerID: seed.Env.ID, Name: "PREVIEW_TOKEN", Value: "production", Secret: true},
	} {
		v.OwnerKind, v.CreatedAt, v.UpdatedAt = repo.OwnerEnv, now, now
		require.NoError(t, s.UpsertVariable(ctx, &v), "seed var %s", v.Name)
	}

	require.NoError(t, ops(t, s).Teardown(ctx, seed.Stack, pr), "teardown")

	left, err := s.ListVariables(ctx, repo.OwnerEnv, pr.ID)
	require.NoError(t, err, "list torn-down env vars")
	assert.Len(t, left, 0, "teardown left variable(s) owned by a deleted env: %+v", left)

	kept, err := s.ListVariables(ctx, repo.OwnerEnv, seed.Env.ID)
	require.NoError(t, err, "list surviving env vars")
	if assert.Len(t, kept, 1, "teardown reached past its own env: %+v", kept) {
		assert.Equal(t, "production", kept[0].Value, "teardown reached past its own env: %+v", kept)
	}

	env, err := s.GetEnvironment(ctx, pr.ID)
	if err == nil && env != nil {
		t.Error("environment row survived its teardown")
	}
}

// The pool's free list is computed from live rows, so deleting an environment
// whose overlay could not be drained hands that network, with the old
// tenant's services still on it, to whatever claims next. The release has to
// gate the row delete, and the operator has to see the error: nothing
// self-heals, netpool.Sweep runs at boot only.
//
// The runtime is real and points at a socket that is not there, which is the
// only way to fail a drain without docker: every call through it errors.
func TestTeardownKeepsTheEnvWhenTheOverlayWillNotRelease(t *testing.T) {
	ctx := context.Background()
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)

	require.NoError(t, s.SetEnvironmentNetwork(ctx, seed.Env.ID, "stkr-net-07"), "claim an overlay")
	env, err := s.GetEnvironment(ctx, seed.Env.ID)
	require.NoError(t, err)

	t.Setenv("DOCKER_HOST", "unix:///nonexistent/stackr-test.sock")
	rt, err := runtime.New()
	require.NoError(t, err, "runtime")

	err = opsRT(t, s, rt).Teardown(ctx, seed.Stack, env)
	require.Error(t, err, "teardown reported success with an undrained overlay")
	assert.Contains(t, err.Error(), "stkr-net-07", "the error does not name the overlay")

	left, gerr := s.GetEnvironment(ctx, seed.Env.ID)
	require.NoError(t, gerr)
	require.NotNil(t, left, "the env row was deleted even though its overlay is still busy")
	assert.Equal(t, "stkr-net-07", left.Network,
		"the overlay was freed in the database while its services are still on it")
}

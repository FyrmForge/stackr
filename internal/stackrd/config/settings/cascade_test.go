package settings_test

import (
	"context"
	"testing"

	"github.com/FyrmForge/stackr/internal/stackrd/config/settings"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The org sits between the server and the stack. Without it the same overrides
// were retyped per stack and drifted.
func TestOrgSitsBetweenServerAndStack(t *testing.T) {
	ctx := context.Background()
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)

	set := func(n int) string {
		return settings.Settings{MemLimitMB: &n}.JSON()
	}
	sv, err := s.GetServer(ctx, "local")
	require.NoError(t, err)
	require.NotNil(t, sv, "the seed has a local server row")
	sv.Settings = set(256)
	require.NoError(t, s.UpdateServer(ctx, sv))

	// Nothing above the server yet: every level reads its value.
	assert.Equal(t, 256, settings.ForOrg(ctx, s, seed.Org.ID).MemLimitMB)
	assert.Equal(t, 256, settings.ForStack(ctx, s, seed.Stack.ID).MemLimitMB)
	assert.Equal(t, 256, settings.ForTile(ctx, s, seed.Tile).MemLimitMB)

	seed.Org.Settings = set(512)
	require.NoError(t, s.UpdateOrg(ctx, seed.Org))
	assert.Equal(t, 512, settings.ForOrg(ctx, s, seed.Org.ID).MemLimitMB)
	assert.Equal(t, 512, settings.ForStack(ctx, s, seed.Stack.ID).MemLimitMB,
		"the stack should inherit the org, not jump to the server")
	assert.Equal(t, 512, settings.ForTile(ctx, s, seed.Tile).MemLimitMB)

	seed.Stack.Settings = set(1024)
	require.NoError(t, s.UpdateStack(ctx, seed.Stack))
	assert.Equal(t, 512, settings.ForOrg(ctx, s, seed.Org.ID).MemLimitMB, "the org is above the stack")
	assert.Equal(t, 1024, settings.ForStack(ctx, s, seed.Stack.ID).MemLimitMB)

	seed.Env.Settings = set(2048)
	require.NoError(t, s.UpdateEnvironment(ctx, seed.Env))
	assert.Equal(t, 2048, settings.ForTile(ctx, s, seed.Tile).MemLimitMB)
}

// Levels names each rung, which is what lets a settings page say where an
// inherited value came from instead of always blaming the server.
func TestLevelsNamesEachRung(t *testing.T) {
	ctx := context.Background()
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)

	got, err := settings.Levels(ctx, s, "", "", seed.Env.ID)
	require.NoError(t, err)
	kinds := make([]string, 0, len(got))
	for _, l := range got {
		kinds = append(kinds, l.Kind)
	}
	assert.Equal(t, []string{"server", "org", "stack", "env"}, kinds,
		"levels = %+v, want the whole chain derived from the env alone", got)

	// Naming only the org stops there: there is no stack to walk down to.
	got, err = settings.Levels(ctx, s, seed.Org.ID, "", "")
	require.NoError(t, err)
	require.Len(t, got, 2, "levels = %+v", got)
	assert.Equal(t, "org", got[1].Kind)
}

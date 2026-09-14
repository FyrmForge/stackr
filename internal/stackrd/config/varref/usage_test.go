package varref_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/config/varref"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

func TestScopeVarsUsedPicksTheTwoPartFormOnly(t *testing.T) {
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)
	tile := seed.Tile.ID

	setVar(t, s, repo.OwnerTile, tile, "A", "${{ stack.vars.REGION }}", false)
	setVar(t, s, repo.OwnerTile, tile, "B", "x${{ stack.vars.BUCKET }}y${{ org.vars.SENTRY }}z", false)
	setVar(t, s, repo.OwnerTile, tile, "C", "${{ stack.pg.host }}", false) // singleton, not a variable
	setVar(t, s, repo.OwnerTile, tile, "D", "${{ tile.api.url }}", false)  // another env's tile
	setVar(t, s, repo.OwnerTile, tile, "E", "${{ nonsense }}", false)      // malformed: skipped, not fatal
	setVar(t, s, repo.OwnerTile, tile, "F", "plain value", false)

	got := varref.ScopeVarsUsed(context.Background(), s, "stack", []string{tile, "missing-tile"})
	require.Equal(t, map[string]bool{"REGION": true, "BUCKET": true}, got,
		"stack scope, want REGION+BUCKET")
	require.Equal(t, map[string]bool{"SENTRY": true},
		varref.ScopeVarsUsed(context.Background(), s, "org", []string{tile}),
		"org scope, want SENTRY")
}

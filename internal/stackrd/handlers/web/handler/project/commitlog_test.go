package project

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/gitlog"
	"github.com/FyrmForge/stackr/internal/stackrd/service"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// Chips land on the commit each env runs, plans or failed on; commits older
// than the list get their own dimmed rows. The fetch itself is stubbed through
// the cache, so this is placement only.
func TestCommitLogPlacesChips(t *testing.T) {
	ctx := context.Background()
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, true)
	now := time.Now().UTC()

	staging := &repo.Environment{ID: "env2", StackID: seed.Stack.ID, Name: "Staging", Slug: "staging", Type: "static", CreatedAt: now.Add(time.Second)}
	require.NoError(t, s.CreateEnvironment(ctx, staging))
	seed.Tile.SourceType, seed.Tile.GitURL = "git", "https://example.com/r.git"
	require.NoError(t, s.UpdateTile(ctx, seed.Tile))
	stTile := &repo.Tile{ID: "tile2", StackID: seed.Stack.ID, EnvironmentID: staging.ID, Name: "app", Slug: "app",
		Kind: "service", SourceType: "git", GitURL: "https://example.com/r.git", Status: "idle", CreatedAt: now, UpdatedAt: now}
	require.NoError(t, s.CreateTile(ctx, stTile))

	deps := []repo.Deployment{
		{ID: "d1", TileID: seed.Tile.ID, Status: "done", CommitSHA: "aaaaaaa1", CreatedAt: now.Add(-3 * time.Minute)},
		{ID: "d2", TileID: seed.Tile.ID, Status: "error", CommitSHA: "aaaaaaa0", CreatedAt: now.Add(-time.Minute)},
		{ID: "d3", TileID: stTile.ID, Status: "done", CommitSHA: "old00001", CreatedAt: now.Add(-time.Hour)},
	}
	for i := range deps {
		require.NoError(t, s.CreateDeployment(ctx, &deps[i]))
	}
	require.NoError(t, s.CreateConfigPlan(ctx, &repo.ConfigPlan{ID: "cp1", StackID: seed.Stack.ID, EnvSlug: "staging",
		CommitSHA: "aaaaaaa0", Status: "pending", Plan: "{}", CreatedAt: now}))

	logCache.Store(seed.Stack.ID, logCacheEntry{checked: time.Now(), at: time.Now(), branch: "master", source: "github", commits: []gitlog.Commit{
		{SHA: "aaaaaaa0", Message: "head"},
		{SHA: "aaaaaaa1", Message: "one back"},
	}})
	h := &handler{store: s, envs: service.NewEnvironmentService(s, nil, nil, nil, service.NewGateService(s)), vars: service.NewVariableService(s, nil, nil, nil)}
	log := h.commitLog(ctx, seed.Stack)

	require.Len(t, log.Rows, 2)
	states := func(row logRow) (out []string) {
		for _, c := range row.Chips {
			out = append(out, c.Env.Slug+":"+c.State)
		}
		return out
	}
	assert.ElementsMatch(t, []string{"prod:failed", "staging:plan"}, states(log.Rows[0]), "head: prod's newest deploy failed there, staging has a pending row")
	assert.Equal(t, []string{"prod:runs"}, states(log.Rows[1]))
	assert.Equal(t, 1, log.Rows[1].Chips[0].Behind, "one commit behind the head")
	require.Len(t, log.Older, 1, "staging runs a commit older than the list")
	assert.Equal(t, "old00001", log.Older[0].Commit.SHA)
	assert.Equal(t, []string{"staging:runs"}, states(log.Older[0]))
}

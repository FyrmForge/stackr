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

// window is a log of n commits, newest first, named c0..c(n-1).
func window(n int) commitLog {
	l := commitLog{Runs: map[string]string{}, Built: map[string]bool{}}
	for i := 0; i < n; i++ {
		l.Rows = append(l.Rows, logRow{Commit: gitlog.Commit{SHA: sha(i)}})
	}
	return l
}

func sha(i int) string { return string(rune('a'+i)) + "0000000" }

// The dialogue lists what the promote carries: everything newer than what the
// env runs, up to and including the commit picked.
func TestPromoteSpan(t *testing.T) {
	l := window(5)

	rows, count, back := promoteSpan(l, sha(2), sha(0))
	assert.False(t, back)
	assert.Equal(t, 2, count, "env on commit 3 of 5, promoting the head carries 2")
	require.Len(t, rows, 2)
	assert.Equal(t, []string{sha(0), sha(1)}, []string{rows[0].Commit.SHA, rows[1].Commit.SHA})

	// A commit older than what the env runs is a rollback, and the same span
	// is what it removes.
	rows, count, back = promoteSpan(l, sha(1), sha(3))
	assert.True(t, back)
	assert.Equal(t, 2, count)
	require.Len(t, rows, 2)

	// Nothing deployed yet: no lower bound, so the window is what is shown.
	rows, count, back = promoteSpan(l, "", sha(0))
	assert.False(t, back)
	assert.Equal(t, 5, count)
	assert.Len(t, rows, 5)

	// The env runs a commit past the window. The rows are what the log has;
	// the count is the real distance, from the connector's compare.
	l.Older = append(l.Older, logRow{Commit: gitlog.Commit{SHA: "old00001"}, Behind: 40, HaveBehind: true})
	l.Runs["e1"] = "old00001"
	rows, count, _ = promoteSpan(l, "old00001", sha(0))
	assert.Len(t, rows, 5, "only the window is listed")
	assert.Equal(t, 40, count, "but the count is the whole gap")
}

// A pending plan is worth warning about when the promote would sail past it:
// its commit is the one being promoted or an older one, so the config it asks
// for is left behind. A plan on a newer commit is a different subject.
func TestPlanWaiting(t *testing.T) {
	l := window(5)
	plans := []repo.ConfigPlan{{ID: "cp1", EnvSlug: "staging", Status: "pending", CommitSHA: sha(2)}}

	assert.NotNil(t, planWaiting(l, plans, "staging", sha(2)), "the plan's own commit")
	assert.NotNil(t, planWaiting(l, plans, "staging", sha(0)), "promoting past the plan leaves its config unapplied")
	assert.Nil(t, planWaiting(l, plans, "staging", sha(3)), "the plan is newer than the commit promoted")
	assert.Nil(t, planWaiting(l, plans, "production", sha(2)), "another environment's rung")

	plans[0].Status = "applied"
	assert.Nil(t, planWaiting(l, plans, "staging", sha(2)))
}

// Promote is offered on the rungs above the default env, and only where the
// env does not already run the commit. The default env builds on push.
func TestReleaseViewTargets(t *testing.T) {
	ctx := context.Background()
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, true)
	now := time.Now().UTC()

	staging := &repo.Environment{ID: "env2", StackID: seed.Stack.ID, Name: "Staging", Slug: "staging",
		Type: "static", CreatedAt: now.Add(time.Second)}
	require.NoError(t, s.CreateEnvironment(ctx, staging))
	seed.Tile.SourceType, seed.Tile.GitURL = "git", "https://example.com/r.git"
	require.NoError(t, s.UpdateTile(ctx, seed.Tile))
	stTile := &repo.Tile{ID: "tile2", StackID: seed.Stack.ID, EnvironmentID: staging.ID, Name: "app", Slug: "app",
		Kind: "service", SourceType: "git", GitURL: "https://example.com/r.git", Status: "idle", CreatedAt: now, UpdatedAt: now}
	require.NoError(t, s.CreateTile(ctx, stTile))
	require.NoError(t, s.CreateDeployment(ctx, &repo.Deployment{ID: "d1", TileID: stTile.ID,
		Status: "done", CommitSHA: sha(1), CreatedAt: now}))

	logCache.Store(seed.Stack.ID, logCacheEntry{checked: time.Now(), at: time.Now(), branch: "master",
		source: "github", commits: []gitlog.Commit{{SHA: sha(0)}, {SHA: sha(1)}}})
	h := &handler{store: s, envs: service.NewEnvironmentService(s, nil, nil, nil, service.NewGateService(s)), vars: service.NewVariableService(s, nil, nil, nil), stacks: service.NewStackService(s, nil, nil, service.NewEnvironmentService(s, nil, nil, nil, service.NewGateService(s)), nil, service.NewGateService(s), nil), tiles: service.NewTileService(s, nil, nil, nil, nil, nil, service.NewGateService(s)), orgs: service.NewOrgService(s)}
	v := h.releaseView(ctx, seed.Stack)

	require.Len(t, v.Rows, 2)
	targets := func(r releaseRow) (out []string) {
		for _, t := range r.Targets {
			out = append(out, t.Env.Slug)
		}
		return out
	}
	assert.Equal(t, []string{"staging"}, targets(v.Rows[0]), "the head is not on staging yet")
	assert.Empty(t, targets(v.Rows[1]), "staging already runs this one")
	assert.True(t, v.Rows[1].Built, "staging deployed it, so the image exists")
	assert.False(t, v.Rows[0].Built)
}

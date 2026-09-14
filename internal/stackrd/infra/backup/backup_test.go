package backup

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

func TestValidate(t *testing.T) {
	base := func() *repo.Backup {
		return &repo.Backup{Kind: repo.BackupVolume, ContainerMode: repo.ModePause, Cron: "0 3 * * *", KeepLatest: 3}
	}
	cases := []struct {
		name    string
		mutate  func(*repo.Backup)
		wantErr bool
	}{
		{"valid", func(*repo.Backup) {}, false},
		{"unknown kind", func(b *repo.Backup) { b.Kind = "snapshot" }, true},
		{"unknown mode", func(b *repo.Backup) { b.ContainerMode = "freeze" }, true},
		{"mode ignored for dumps", func(b *repo.Backup) { b.Kind, b.ContainerMode = repo.BackupDump, "" }, false},
		{"no schedule", func(b *repo.Backup) { b.Cron = "" }, true},
		{"bad cron", func(b *repo.Backup) { b.Cron = "every tuesday" }, true},
		{"bad timezone", func(b *repo.Backup) { b.Timezone = "Mars/Olympus" }, true},
		{"good timezone", func(b *repo.Backup) { b.Timezone = "Europe/London" }, false},
		{"negative keep", func(b *repo.Backup) { b.KeepLatest = -1 }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := base()
			tc.mutate(b)
			err := Validate(b)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// The prefix is derived, never supplied: prune deletes everything under it
// beyond the keep count, so two schedules must never share one, and it must
// carry the org so one tenant's tree cannot be aimed at another's.
func TestPrefixIsDerivedAndUnique(t *testing.T) {
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, false)
	ctx := context.Background()
	s := &Service{store: store}

	b1 := &repo.Backup{ID: "b1", TileID: sql.NullString{String: seed.Tile.ID, Valid: true}, Kind: repo.BackupVolume}
	b2 := &repo.Backup{ID: "b2", TileID: sql.NullString{String: seed.Tile.ID, Valid: true}, Kind: repo.BackupDump}

	p1, p2 := s.prefixFor(ctx, b1), s.prefixFor(ctx, b2)
	require.NotEqual(t, p1, p2, "two schedules on one tile share a prefix (%q); pruning one would delete the other's archives", p1)
	assert.Contains(t, p1, seed.Org.ID, "prefix %q does not identify the org and tile it belongs to", p1)
	assert.Contains(t, p1, seed.Tile.ID, "prefix %q does not identify the org and tile it belongs to", p1)
	panelPrefix := s.prefixFor(ctx, &repo.Backup{ID: "bp", Kind: repo.BackupStackr})
	assert.NotContains(t, panelPrefix, seed.Org.ID, "the panel database landed under an org's prefix: %q", panelPrefix)
}

// A destination belonging to another org must not be usable, and the refusal
// must look like "not found"; an id that 403s is an id confirmed to exist.
func TestResolveDestination(t *testing.T) {
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, false)
	ctx := context.Background()
	now := time.Now().UTC()

	other := &repo.Org{ID: "org-other", Name: "Other", Slug: "other", CreatedAt: now}
	require.NoError(t, store.CreateOrg(ctx, other), "create other org")
	mine := &repo.BackupDestination{ID: "mine", OrgID: sql.NullString{String: seed.Org.ID, Valid: true},
		Name: "mine", Endpoint: "https://s3", Bucket: "b", CreatedAt: now}
	theirs := &repo.BackupDestination{ID: "theirs", OrgID: sql.NullString{String: other.ID, Valid: true},
		Name: "theirs", Endpoint: "https://s3", Bucket: "b", CreatedAt: now}
	global := &repo.BackupDestination{ID: "global", Name: "global", Endpoint: "https://s3", Bucket: "b", Shared: true, CreatedAt: now}
	// A server-wide destination nobody has shared: present, and invisible to
	// every org, because it carries the credentials the archive is written with.
	hidden := &repo.BackupDestination{ID: "hidden", Name: "hidden", Endpoint: "https://s3", Bucket: "b", CreatedAt: now}
	for _, d := range []*repo.BackupDestination{mine, theirs, global, hidden} {
		require.NoError(t, store.CreateBackupDestination(ctx, d), "seed destination")
	}

	_, err := ResolveDestination(ctx, store, seed.Org.ID, mine.ID)
	assert.NoError(t, err, "own destination refused")
	_, err = ResolveDestination(ctx, store, seed.Org.ID, global.ID)
	assert.NoError(t, err, "server-wide destination refused")
	_, err = ResolveDestination(ctx, store, seed.Org.ID, theirs.ID)
	require.Error(t, err, "another org's destination was accepted")
	assert.ErrorContains(t, err, "not found", "refusal leaks that the id exists: %q", err)
	_, err = ResolveDestination(ctx, store, seed.Org.ID, hidden.ID)
	require.Error(t, err, "an unshared server-wide destination was accepted")
	assert.ErrorContains(t, err, "not found", "refusal leaks that the id exists: %q", err)
}

// One claim per backup id, held by a run or a whole restore. Without it two
// restores of the same volume run `rm -rf` and untar over each other; easy to
// trigger, because a detached restore outlives the browser request that
// started it and invites a second click.
func TestOneRunPerBackupAtATime(t *testing.T) {
	s := &Service{running: map[string]bool{}}
	require.True(t, s.begin("b1"), "first claim refused")
	assert.False(t, s.begin("b1"), "a second run claimed a backup already in progress")
	assert.True(t, s.begin("b2"), "an unrelated backup was blocked")
	s.end("b1")
	assert.True(t, s.begin("b1"), "claim not released")
}

package sqlite_test

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

// The backup tables are new in 028. sqlx binds by name, so a column missing
// from an INSERT or an UPDATE is silent data loss rather than an error, this
// round-trips every one of them in both directions.
func TestBackupsRoundTrip(t *testing.T) {
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, false)
	ctx := context.Background()
	now := time.Now().UTC()

	dest := &repo.BackupDestination{
		ID: "d1", OrgID: sql.NullString{String: seed.Org.ID, Valid: true}, Name: "offsite",
		Endpoint: "https://s3.example.com", Region: "eu-west-1", Bucket: "b",
		AccessKey: "ak", SecretKey: "sk", CreatedAt: now,
	}
	require.NoError(t, store.CreateBackupDestination(ctx, dest), "create destination")
	got, err := store.GetBackupDestination(ctx, dest.ID)
	require.NoError(t, err, "get destination")
	require.NotNil(t, got, "get destination")
	assert.False(t, got.SecretKey != "sk" || got.Region != "eu-west-1" || got.Global(),
		"destination did not round-trip: %+v", got)

	// An update with a blank secret must keep the stored one, the form never
	// renders the secret back, so a plain save would otherwise wipe it.
	got.SecretKey = ""
	got.Bucket = "b2"
	require.NoError(t, store.UpdateBackupDestination(ctx, got), "update destination")
	again, _ := store.GetBackupDestination(ctx, dest.ID)
	assert.False(t, again == nil || again.SecretKey != "sk" || again.Bucket != "b2",
		"blank secret on update lost the credential: %+v", again)

	b := &repo.Backup{
		ID: "b1", TileID: sql.NullString{String: seed.Tile.ID, Valid: true}, DestinationID: dest.ID,
		Kind: repo.BackupVolume, ContainerMode: repo.ModePause, Cron: "0 3 * * *",
		Timezone: "Europe/London", KeepLatest: 5, Enabled: true, CreatedAt: now,
	}
	require.NoError(t, store.CreateBackup(ctx, b), "create backup")
	gotB, err := store.GetBackup(ctx, b.ID)
	require.NoError(t, err, "get backup")
	require.NotNil(t, gotB, "get backup")
	assert.False(t, gotB.Timezone != "Europe/London" || gotB.ContainerMode != repo.ModePause || gotB.KeepLatest != 5,
		"backup did not round-trip: %+v", gotB)
	assert.Equal(t, "CRON_TZ=Europe/London 0 3 * * *", gotB.Schedule(), "Schedule()")
	gotB.ContainerMode, gotB.KeepLatest, gotB.Enabled = repo.ModeStop, 9, false
	require.NoError(t, store.UpdateBackup(ctx, gotB), "update backup")
	afterUpdate, _ := store.GetBackup(ctx, b.ID)
	assert.False(t, afterUpdate == nil || afterUpdate.ContainerMode != repo.ModeStop || afterUpdate.KeepLatest != 9 || afterUpdate.Enabled,
		"backup update lost columns: %+v", afterUpdate)

	run := &repo.BackupRun{ID: "r1", BackupID: b.ID, Trigger: "manual", Status: "running", CreatedAt: now}
	require.NoError(t, store.CreateBackupRun(ctx, run), "create run")
	run.Status, run.ObjectKey, run.SizeBytes = "done", "stackr/org1/k.tar.gz", 4096
	run.FinishedAt = sql.NullTime{Time: now, Valid: true}
	require.NoError(t, store.UpdateBackupRun(ctx, run), "update run")
	runs, err := store.ListBackupRuns(ctx, b.ID, 10)
	require.NoError(t, err, "list runs")
	require.Len(t, runs, 1, "list runs")
	assert.False(t, runs[0].Status != "done" || runs[0].SizeBytes != 4096 || runs[0].Trigger != "manual",
		"run did not round-trip: %+v", runs[0])
}

// The panel's own database is not a tile, so its schedule carries a NULL
// tile_id. A NOT NULL column (or an empty string against the foreign key)
// would make that row impossible to insert.
func TestPanelBackupHasNoTile(t *testing.T) {
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, false)
	ctx := context.Background()

	dest := &repo.BackupDestination{ID: "d1", Name: "global", Endpoint: "https://s3", Bucket: "b", CreatedAt: time.Now().UTC()}
	require.NoError(t, store.CreateBackupDestination(ctx, dest), "create destination")
	assert.True(t, dest.Global(), "a destination with no org should be server-wide")
	b := &repo.Backup{ID: "b1", DestinationID: dest.ID, Kind: repo.BackupStackr,
		Cron: "0 4 * * *", KeepLatest: 7, Enabled: true, CreatedAt: time.Now().UTC()}
	require.NoError(t, store.CreateBackup(ctx, b), "create panel backup")
	got, _ := store.GetBackup(ctx, b.ID)
	assert.False(t, got == nil || got.TileID.Valid, "panel backup should carry no tile: %+v", got)
	// And a tile-scoped listing must not pick it up.
	bs, _ := store.ListBackupsByTile(ctx, seed.Tile.ID)
	assert.Len(t, bs, 0, "panel backup leaked into a tile listing: %+v", bs)
}

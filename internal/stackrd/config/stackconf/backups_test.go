package stackconf

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const backupFile = `version: 1
stack: s
environments:
  prod:
    tiles:
      db:
        type: managed
        engine: postgres
        backup:
          dest: ${{ org.backups.offsite }}
          schedule: "0 3 * * *"
          tz: Europe/Berlin
          keep: 14
`

// The destination is a reference, never a literal: it carries the bucket
// credentials and the file is in git.
func TestBackupDestMustBeAReference(t *testing.T) {
	for _, bad := range []string{
		"dest-id-1234",
		"${{ org.vars.OFFSITE }}",
		"${{ stack.backups.offsite }}",
		"prefix ${{ org.backups.offsite }}",
	} {
		_, err := Load([]byte(`version: 1
stack: s
environments:
  prod:
    tiles:
      db: {type: managed, engine: postgres, backup: {dest: "`+bad+`", schedule: "0 3 * * *"}}
`), nil)
		require.Error(t, err, "dest %q was accepted", bad)
	}
}

// kind is derived, so mode: on a managed database is a setting that could only
// ever do nothing. Say so rather than accept it.
func TestBackupModeIsVolumeOnly(t *testing.T) {
	_, err := Load([]byte(`version: 1
stack: s
environments:
  prod:
    tiles:
      db: {type: managed, engine: postgres, backup: {dest: "${{ org.backups.o }}", schedule: "0 3 * * *", mode: pause}}
`), nil)
	require.ErrorContains(t, err, "mode")

	r, err := Load([]byte(`version: 1
stack: s
environments:
  prod:
    tiles:
      web: {image: nginx, backup: {dest: "${{ org.backups.o }}", schedule: "0 3 * * *", mode: stop}}
`), nil)
	require.NoError(t, err, "a volume tile may set mode")
	assert.Equal(t, "stop", r.Envs["prod"].Tiles["web"].Backup.Mode)

	_, err = Load([]byte(`version: 1
stack: s
environments:
  prod:
    tiles:
      web: {image: nginx, backup: {dest: "${{ org.backups.o }}", schedule: "0 3 * * *", mode: freeze}}
`), nil)
	require.ErrorContains(t, err, "pause, stop or live")
}

// A declared schedule lands on the tile; dropping the block takes it away. The
// file owns it, so there is no third state where the panel's row survives a
// file that stopped mentioning it.
func TestApplyBackupsWritesAndRemoves(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, true) // env "prod"

	dest := &repo.BackupDestination{ID: "d1", OrgID: sql.NullString{String: seed.Org.ID, Valid: true},
		Name: "offsite", Endpoint: "https://s3", Bucket: "b", CreatedAt: time.Now().UTC()}
	require.NoError(t, store.CreateBackupDestination(ctx, dest))
	db := &repo.Tile{ID: "db1", StackID: seed.Stack.ID, EnvironmentID: seed.Env.ID,
		Name: "db", Slug: "db", Kind: "managed", Engine: "postgres", Status: "idle",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	require.NoError(t, store.CreateTile(ctx, db))

	r, err := Load([]byte(backupFile), nil)
	require.NoError(t, err)
	a := Applier{Planner: Planner{Store: store}}
	require.NoError(t, a.applyBackups(ctx, seed.Stack, r, "", nil))

	bs, err := store.ListBackupsByTile(ctx, db.ID)
	require.NoError(t, err)
	require.Len(t, bs, 1, "backups = %+v", bs)
	assert.Equal(t, repo.BackupDump, bs[0].Kind, "a managed database is dumped, not tarred")
	assert.Equal(t, "d1", bs[0].DestinationID)
	assert.Equal(t, "0 3 * * *", bs[0].Cron)
	assert.Equal(t, "Europe/Berlin", bs[0].Timezone)
	assert.Equal(t, 14, bs[0].KeepLatest)
	assert.True(t, bs[0].Enabled)

	// Same file again: updated in place, not duplicated.
	require.NoError(t, a.applyBackups(ctx, seed.Stack, r, "", nil))
	bs, _ = store.ListBackupsByTile(ctx, db.ID)
	require.Len(t, bs, 1, "a second apply duplicated the schedule: %+v", bs)

	// The file stops declaring it.
	bare, err := Load([]byte("version: 1\nstack: s\nenvironments:\n  prod:\n    tiles:\n      db: {type: managed, engine: postgres}\n"), nil)
	require.NoError(t, err)
	require.NoError(t, a.applyBackups(ctx, seed.Stack, bare, "", nil))
	bs, _ = store.ListBackupsByTile(ctx, db.ID)
	assert.Empty(t, bs, "dropping the block should remove the schedule: %+v", bs)
}

// A destination the org cannot reach is a plan error, not a schedule that
// silently writes nowhere.
func TestPlanBackupsRefusesAnUnknownDestination(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, true)

	r, err := Load([]byte(backupFile), nil)
	require.NoError(t, err)
	p := &Plan{}
	Planner{Store: store}.planBackups(ctx, seed.Stack, r, p, "", nil)
	require.Len(t, p.Errors, 1, "errors = %v", p.Errors)
	assert.Contains(t, p.Errors[0], "offsite")

	// A server-wide destination the admin has not shared is equally out of
	// reach: it carries the bucket credentials.
	hidden := &repo.BackupDestination{ID: "g1", Name: "offsite", Endpoint: "https://s3",
		Bucket: "b", CreatedAt: time.Now().UTC()}
	require.NoError(t, store.CreateBackupDestination(ctx, hidden))
	p = &Plan{}
	global, err := Load([]byte(`version: 1
stack: s
environments:
  prod:
    tiles:
      db: {type: managed, engine: postgres, backup: {dest: "${{ stackr.backups.offsite }}", schedule: "0 3 * * *"}}
`), nil)
	require.NoError(t, err)
	Planner{Store: store}.planBackups(ctx, seed.Stack, global, p, "", nil)
	require.Len(t, p.Errors, 1, "an unshared server-wide destination should not resolve: %v", p.Errors)

	hidden.Shared = true
	require.NoError(t, store.UpdateBackupDestination(ctx, hidden))
	p = &Plan{}
	Planner{Store: store}.planBackups(ctx, seed.Stack, global, p, "", nil)
	assert.Empty(t, p.Errors, "a shared server-wide destination should resolve: %v", p.Errors)
}

package sqlite_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// Columns added to tiles/domains after the fact are the easy ones to wire into
// the INSERT and forget in the UPDATE (or vice versa); sqlx binds by name, so
// a missing one is silent data loss, not an error. This round-trips both
// directions for every column added on 2026-07-31.
func TestNewColumnsRoundTrip(t *testing.T) {
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, false)
	ctx := context.Background()

	// tiles.max_size_mb; written by CreateTile via tileCols, read back by the
	// SELECT * helpers, then changed through UpdateTile.
	vol := *seed.Tile
	vol.ID = "vol-1"
	vol.Slug = "vol"
	vol.Name = "vol"
	vol.Kind = "volume"
	vol.MaxSizeMB = 512
	require.NoError(t, store.CreateTile(ctx, &vol), "create")
	got, err := store.GetTile(ctx, vol.ID)
	require.NoError(t, err, "get")
	require.NotNil(t, got, "get")
	assert.Equal(t, 512, got.MaxSizeMB, "max_size_mb not stored on create")
	got.MaxSizeMB = 1024
	require.NoError(t, store.UpdateTile(ctx, got), "update")
	again, _ := store.GetTile(ctx, vol.ID)
	assert.False(t, again == nil || again.MaxSizeMB != 1024, "max_size_mb not stored on update: %+v", again)

	// tiles runtime fields (2026-08-17): user, shm_size_mb, privileged,
	// devices, restart_policy; same silent-loss failure mode.
	svc := *seed.Tile
	svc.ID = "svc-rt"
	svc.Slug = "svc-rt"
	svc.Name = "svc-rt"
	svc.User = "1000:1000"
	svc.ShmSizeMB = 128
	svc.Privileged = true
	svc.Devices = "/dev/kmsg"
	svc.RestartPolicy = "always"
	require.NoError(t, store.CreateTile(ctx, &svc), "create svc")
	rt, err := store.GetTile(ctx, svc.ID)
	require.NoError(t, err, "get svc")
	require.NotNil(t, rt, "get svc")
	assert.Equal(t, "1000:1000", rt.User, "user not stored on create")
	assert.Equal(t, 128, rt.ShmSizeMB, "shm_size_mb not stored on create")
	assert.True(t, rt.Privileged, "privileged not stored on create")
	assert.Equal(t, "/dev/kmsg", rt.Devices, "devices not stored on create")
	assert.Equal(t, "always", rt.RestartPolicy, "restart_policy not stored on create")
	// 040: healthcheck knobs + the function trigger, same failure mode.
	rt.HealthcheckIntervalS, rt.HealthcheckTimeoutS = 10, 5
	rt.HealthcheckRetries, rt.HealthcheckStartPeriodS = 4, 30
	rt.RunOnDeploy = true
	rt.DependsOn = "db:healthy\ninit:completed"
	rt.Files = "config/loki.yml:/etc/loki/config.yml"
	rt.Storage = "nas-media/tv:/tv:ro"
	rt.User, rt.ShmSizeMB, rt.Privileged, rt.Devices, rt.RestartPolicy = "999", 64, false, "/dev/dri", ""
	require.NoError(t, store.UpdateTile(ctx, rt), "update svc")
	rt2, _ := store.GetTile(ctx, svc.ID)
	require.NotNil(t, rt2)
	assert.Equal(t, "999", rt2.User, "user not stored on update")
	assert.Equal(t, 64, rt2.ShmSizeMB, "shm_size_mb not stored on update")
	assert.False(t, rt2.Privileged, "privileged not stored on update")
	assert.Equal(t, "/dev/dri", rt2.Devices, "devices not stored on update")
	assert.Equal(t, "", rt2.RestartPolicy, "restart_policy not stored on update")
	assert.True(t, rt2.HealthcheckIntervalS == 10 && rt2.HealthcheckTimeoutS == 5 &&
		rt2.HealthcheckRetries == 4 && rt2.HealthcheckStartPeriodS == 30,
		"healthcheck knobs not stored on update: %+v", rt2)
	assert.True(t, rt2.RunOnDeploy, "run_on_deploy not stored on update")
	assert.Equal(t, "db:healthy\ninit:completed", rt2.DependsOn, "depends_on not stored on update")
	assert.Equal(t, "config/loki.yml:/etc/loki/config.yml", rt2.Files, "files not stored on update")
	assert.Equal(t, "nas-media/tv:/tv:ro", rt2.Storage, "storage not stored on update")

	// domains.auto; the flag the proxy uses to decide what goes behind the
	// session check. A domain that loses it silently becomes public.
	d := &repo.Domain{ID: "d-1", TileID: seed.Tile.ID, Host: "a-b-c.example.com", Path: "/", ContainerPort: 80, HTTPS: true, Auto: true}
	require.NoError(t, store.CreateDomain(ctx, d), "create domain")
	ds, err := store.ListDomainsByTile(ctx, seed.Tile.ID)
	require.NoError(t, err, "list domains")
	require.NotEmpty(t, ds, "list domains")
	assert.True(t, ds[0].Auto, "domains.auto not stored; every preview URL would render as hand-attached")

	// orgs.avatar_path / users.avatar_path; UpdateOrg and UpdateUser are the
	// only writers, and both had to be widened by hand.
	org := seed.Org
	org.AvatarPath = "orgs/x-1234.png"
	require.NoError(t, store.UpdateOrg(ctx, org), "update org")
	back, _ := store.GetOrg(ctx, org.ID)
	assert.False(t, back == nil || back.AvatarPath != "orgs/x-1234.png", "orgs.avatar_path not stored: %+v", back)

	u := &repo.User{ID: "u-1", Email: "a@b.c", Name: "A", Role: "member", Active: true}
	require.NoError(t, store.CreateUser(ctx, u), "create user")
	u.AvatarPath = "users/u-1-abcd.png"
	require.NoError(t, store.UpdateUser(ctx, u), "update user")
	backU, _ := store.GetUserByID(ctx, u.ID)
	assert.False(t, backU == nil || backU.AvatarPath != "users/u-1-abcd.png", "users.avatar_path not stored: %+v", backU)
}

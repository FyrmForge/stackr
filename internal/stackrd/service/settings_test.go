package service_test

import (
	"context"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/config/settings"
	"github.com/FyrmForge/stackr/internal/stackrd/service"
	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

func mem(v string) url.Values { return url.Values{"mem_limit_mb": {v}} }

// The point-13 row: `defaults:` is a config-file field at all three levels,
// and each apply replaces the whole blob. Only the panel's org page refused
// an ungated write, so a stack or environment default set by hand survived
// exactly until the next apply and then vanished with no error anywhere.
func TestAManagedStackRefusesDefaultsAtEveryLevel(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, true)
	svc := service.NewSettingsService(store, nil)

	err := svc.SaveStack(ctx, seed.Stack, mem("512"))
	require.Error(t, err)
	cf, ok := svcerr.IsConflict(err)
	assert.True(t, ok, "a config-managed stack is a conflict, not a validation error")
	assert.Contains(t, cf.Msg, "org/cfg")

	err = svc.SaveEnv(ctx, seed.Env, mem("512"))
	require.Error(t, err)
	_, ok = svcerr.IsConflict(err)
	assert.True(t, ok, "an environment is a block in its stack's file, so its stack's gate applies")

	seed.Org.ConfigConnectorID, seed.Org.ConfigRepo = "conn1", "org/orgcfg"
	require.NoError(t, store.UpdateOrg(ctx, seed.Org))
	_, ok = svcerr.IsConflict(svc.SaveOrg(ctx, seed.Org, mem("512")))
	assert.True(t, ok)
}

func TestAnUnmanagedSaveLandsAndABadValueDoesNot(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, false)
	svc := service.NewSettingsService(store, nil)

	require.NoError(t, svc.SaveStack(ctx, seed.Stack, mem("512")))
	got, err := store.GetStack(ctx, seed.Stack.ID)
	require.NoError(t, err)
	require.NotNil(t, settings.Parse(got.Settings).MemLimitMB)
	assert.Equal(t, 512, *settings.Parse(got.Settings).MemLimitMB)

	// Protection with a user and no password locks every URL below this level
	// behind bytes nobody has.
	err = svc.SaveStack(ctx, seed.Stack, url.Values{"protect": {"1"}, "protect_user": {"admin"}})
	_, ok := svcerr.IsInvalid(err)
	assert.True(t, ok, "half a pair is a validation error: %v", err)
}

// SP1: the panel form posts the name on every save and the old handler stored
// whatever arrived, so a save that lost the field renamed the server to "".
func TestAServerNeedsAName(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	testdb.SeedStack(t, store, false)
	svc := service.NewSettingsService(store, nil)

	sv, err := store.GetServer(ctx, "local")
	require.NoError(t, err)
	inv, ok := svcerr.IsInvalid(svc.SaveServer(ctx, sv, "  ", mem("512")))
	require.True(t, ok)
	assert.Equal(t, "a server name is required", inv.Error(), "the whole sentence, no field prefix: a flash renders it verbatim")

	require.NoError(t, svc.SaveServer(ctx, sv, "Box", mem("512")))
	got, err := store.GetServer(ctx, "local")
	require.NoError(t, err)
	assert.Equal(t, "Box", got.Name)
}

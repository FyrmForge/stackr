package main

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/config/settings"
	"github.com/FyrmForge/stackr/internal/stackrd/service"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

func TestSeedInstallRunsOnce(t *testing.T) {
	s := testdb.New(t)
	ctx := context.Background()
	set := service.NewSettingsService(s, nil)
	ok := func(context.Context) error { return nil }

	// A bad CIDR refuses the whole list rather than being skipped with a
	// warning: a silently dropped entry is a trusted proxy that is not
	// trusted, and it surfaces later as every client IP being the proxy's.
	require.Error(t, seedInstall(ctx, s, set, "Example.com", "1", "10.0.0.0/8, nope, 192.168.1.100", ok))
	require.NoError(t, seedInstall(ctx, s, set, "Example.com", "1", "10.0.0.0/8, 192.168.1.100", ok))
	res, err := s.ListDomainResources(ctx)
	require.NoError(t, err)
	require.Len(t, res, 1)
	assert.Equal(t, "example.com", res[0].Host)
	assert.Equal(t, "instance", res[0].Level)
	assert.Equal(t, "local", res[0].OwnerID)
	got, _ := s.GetSetting(ctx, settings.KeyTrustedProxies)
	assert.Equal(t, "10.0.0.0/8\n192.168.1.100/32", got)
	got, _ = s.GetSetting(ctx, settings.KeyTrustCF)
	assert.Equal(t, "1", got)

	// The operator clears them in the panel; a restart must not bring them back.
	require.NoError(t, s.DeleteDomainResource(ctx, res[0].ID))
	require.NoError(t, s.SetSetting(ctx, settings.KeyTrustedProxies, ""))
	require.NoError(t, seedInstall(ctx, s, set, "example.com", "1", "10.0.0.0/8", ok))
	res, _ = s.ListDomainResources(ctx)
	assert.Empty(t, res)
	got, _ = s.GetSetting(ctx, settings.KeyTrustedProxies)
	assert.Empty(t, got)
}

func TestSeedInstallCloudflareFetchFails(t *testing.T) {
	s := testdb.New(t)
	ctx := context.Background()
	set := service.NewSettingsService(s, nil)
	require.NoError(t, seedInstall(ctx, s, set, "", "1", "", func(context.Context) error { return errors.New("offline") }))
	got, _ := s.GetSetting(ctx, settings.KeyTrustCF)
	assert.Empty(t, got, "never on with nothing trusted")
}

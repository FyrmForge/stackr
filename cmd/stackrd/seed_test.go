package main

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

func TestSeedInstallRunsOnce(t *testing.T) {
	s := testdb.New(t)
	ctx := context.Background()
	ok := func(context.Context) error { return nil }

	require.NoError(t, seedInstall(ctx, s, "Example.com", "1", "10.0.0.0/8, nope, 192.168.1.100", ok))
	res, err := s.ListDomainResources(ctx)
	require.NoError(t, err)
	require.Len(t, res, 1)
	assert.Equal(t, "example.com", res[0].Host)
	assert.Equal(t, "instance", res[0].Level)
	assert.Equal(t, "local", res[0].OwnerID)
	got, _ := s.GetSetting(ctx, "trusted_proxies")
	assert.Equal(t, "10.0.0.0/8\n192.168.1.100/32", got)
	got, _ = s.GetSetting(ctx, "trust_cloudflare")
	assert.Equal(t, "1", got)

	// The operator clears them in the panel; a restart must not bring them back.
	require.NoError(t, s.DeleteDomainResource(ctx, res[0].ID))
	require.NoError(t, s.SetSetting(ctx, "trusted_proxies", ""))
	require.NoError(t, seedInstall(ctx, s, "example.com", "1", "10.0.0.0/8", ok))
	res, _ = s.ListDomainResources(ctx)
	assert.Empty(t, res)
	got, _ = s.GetSetting(ctx, "trusted_proxies")
	assert.Empty(t, got)
}

func TestSeedInstallCloudflareFetchFails(t *testing.T) {
	s := testdb.New(t)
	ctx := context.Background()
	require.NoError(t, seedInstall(ctx, s, "", "1", "", func(context.Context) error { return errors.New("offline") }))
	got, _ := s.GetSetting(ctx, "trust_cloudflare")
	assert.Empty(t, got, "never on with nothing trusted")
}

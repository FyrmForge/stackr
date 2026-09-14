package varref_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/config/varref"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// ${{ stackr.* }} is the server describing itself. The values are per-env
// because traefik joins every environment network and lands on a different
// address in each, so they are read off the consumer's own env row, which the
// proxy fills in when it attaches.
func TestPlatformScopeResolves(t *testing.T) {
	ctx := context.Background()
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)

	require.NoError(t, s.SetEnvironmentProxy(ctx, seed.Env.ID, "172.18.0.4", "172.18.0.0/16"),
		"record proxy addr")
	setVar(t, s, repo.OwnerTile, seed.Tile.ID, "TRUSTED_PROXIES", "${{ stackr.PROXY_CIDR }}", false)
	setVar(t, s, repo.OwnerTile, seed.Tile.ID, "PEER", "${{ stackr.PROXY_IP }}", false)
	// It composes with the rest of the syntax like any other reference.
	setVar(t, s, repo.OwnerTile, seed.Tile.ID, "MIXED", "from ${{ stackr.PROXY_IP }} only", false)

	got := resolve(t, s, seed.Tile.ID)
	for name, want := range map[string]string{
		"TRUSTED_PROXIES": "172.18.0.0/16",
		"PEER":            "172.18.0.4",
		"MIXED":           "from 172.18.0.4 only",
	} {
		assert.Equal(t, want, got[name], name)
	}

	// Not a credential, so an external reader sees it too, unlike a secret,
	// which Scoped mode refuses.
	res, err := varref.New(s).Resolve(ctx, seed.Tile.ID, varref.Scoped)
	require.NoError(t, err, "scoped resolve")
	assert.Equal(t, "172.18.0.0/16", res.Vars["TRUSTED_PROXIES"], "scoped mode lost the value")
}

// The two ways this scope can go wrong both have to be loud. A blank
// TRUSTED_PROXIES does not fail the app, it just stops trusting the forwarded
// header and quietly credits every request to the proxy, so neither an unset
// value nor a typo may resolve to "".
func TestPlatformScopeRefusesUnsetAndUnknown(t *testing.T) {
	ctx := context.Background()
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)
	tile := seed.Tile.ID

	t.Run("unset", func(t *testing.T) {
		setVar(t, s, repo.OwnerTile, tile, "A", "${{ stackr.PROXY_IP }}", false)
		mustFail(t, s, tile, varref.System, "not known yet")
		require.NoError(t, s.DeleteVariable(ctx, repo.OwnerTile, tile, "A"))
	})

	t.Run("typo", func(t *testing.T) {
		setVar(t, s, repo.OwnerTile, tile, "B", "${{ stackr.PROXY }}", false)
		mustFail(t, s, tile, varref.System, "is not a stackr value")
		require.NoError(t, s.DeleteVariable(ctx, repo.OwnerTile, tile, "B"))
	})

	// stackr. is one character from stack., and that neighbour takes a
	// three-part form. Accepting it here would resolve a typo against the wrong
	// scope instead of saying so.
	t.Run("three-part", func(t *testing.T) {
		setVar(t, s, repo.OwnerTile, tile, "C", "${{ stackr.web.PORT }}", false)
		mustFail(t, s, tile, varref.System, "stackr has no sources")
		require.NoError(t, s.DeleteVariable(ctx, repo.OwnerTile, tile, "C"))
	})
}

// Parse is the gate that keeps an unknown name from ever reaching resolution,
// so it is checked directly too, the config planner parses without a store.
func TestParsePlatformNames(t *testing.T) {
	for _, ok := range []string{"stackr.PROXY_IP", "stackr.PROXY_CIDR"} {
		_, err := varref.Parse(ok)
		assert.NoError(t, err, "Parse(%q), want it accepted", ok)
	}
	for _, bad := range []string{"stackr.PROXY", "stackr.proxy_ip", "stackr.", "stackr.a.b"} {
		_, err := varref.Parse(bad)
		assert.Error(t, err, "Parse(%q) was accepted, want an error", bad)
	}
}

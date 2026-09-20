package varref_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/config/varref"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/envnet"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// R3: a service publishes its endpoint as STACKR_* outputs, the address is
// stackr's to know, not something each app has to be told twice.
func TestEndpointOutputs(t *testing.T) {
	ctx := context.Background()
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)
	now := time.Now().UTC()

	api := &repo.Tile{ID: "api12345", StackID: seed.Stack.ID, EnvironmentID: seed.Env.ID,
		Name: "api", Slug: "api", Kind: "service", ContainerPort: 3000,
		EndpointProtocol: "http", Status: "idle", CreatedAt: now, UpdatedAt: now}
	require.NoError(t, s.CreateTile(ctx, api), "tile")
	require.NoError(t, s.CreateDomain(ctx, &repo.Domain{ID: "d1", TileID: api.ID, Host: "api.example.com",
		Path: "/", ContainerPort: 3000, HTTPS: true, CreatedAt: now}), "domain")
	// A redirect must not be mistaken for the tile's own public address.
	require.NoError(t, s.CreateDomain(ctx, &repo.Domain{ID: "d2", TileID: api.ID, Host: "www.example.com",
		Path: "/", RedirectTo: "api.example.com", CreatedAt: now}), "redirect domain")

	setVar(t, s, repo.OwnerTile, seed.Tile.ID, "HOST", "${{ tile.api.STACKR_PRIVATE_DOMAIN }}", false)
	setVar(t, s, repo.OwnerTile, seed.Tile.ID, "PORT", "${{ tile.api.STACKR_INTERNAL_PORT }}", false)
	setVar(t, s, repo.OwnerTile, seed.Tile.ID, "URL", "${{ tile.api.STACKR_INTERNAL_URL }}", false)
	setVar(t, s, repo.OwnerTile, seed.Tile.ID, "PUBLIC", "${{ tile.api.STACKR_PUBLIC_URL }}", false)
	setVar(t, s, repo.OwnerTile, seed.Tile.ID, "PUBDOM", "${{ tile.api.STACKR_PUBLIC_DOMAIN }}", false)

	got := resolve(t, s, seed.Tile.ID)
	alias := envnet.TileAlias(api.ID)
	for name, want := range map[string]string{
		"HOST":   alias,
		"PORT":   "3000",
		"URL":    "http://" + alias + ":3000",
		"PUBLIC": "https://api.example.com",
		"PUBDOM": "api.example.com",
	} {
		assert.Equal(t, want, got[name], name)
	}
}

// A tcp endpoint has no URL form, asking for one must fail rather than hand
// back something that looks like a URL and isn't.
func TestTCPEndpointHasNoURL(t *testing.T) {
	ctx := context.Background()
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)
	now := time.Now().UTC()

	tile := &repo.Tile{ID: "tcp12345", StackID: seed.Stack.ID, EnvironmentID: seed.Env.ID,
		Name: "gw", Slug: "gw", Kind: "service", ContainerPort: 9000,
		EndpointProtocol: "tcp", Status: "idle", CreatedAt: now, UpdatedAt: now}
	require.NoError(t, s.CreateTile(ctx, tile), "tile")
	setVar(t, s, repo.OwnerTile, seed.Tile.ID, "U", "${{ tile.gw.STACKR_INTERNAL_URL }}", false)
	mustFail(t, s, seed.Tile.ID, varref.System, "publishes no output named")
}

// The endpoint port is injected under the app's chosen variable name, but an
// explicit variable of that name wins, injection is a convenience, not a lock.
func TestEndpointPortVarInjection(t *testing.T) {
	ctx := context.Background()
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)

	tile := seed.Tile
	tile.ContainerPort = 8080
	tile.EndpointPortVar = "PORT"
	require.NoError(t, s.UpdateTile(ctx, tile.ID, tile.TileConfig), "update")
	assert.Equal(t, "8080", resolve(t, s, tile.ID)["PORT"], "injected PORT")

	setVar(t, s, repo.OwnerTile, tile.ID, "PORT", "9999", false)
	assert.Equal(t, "9999", resolve(t, s, tile.ID)["PORT"], "explicit PORT should win")
}

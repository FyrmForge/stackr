package githubapp

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// tiles.connector is a raw connector id, and a stack file sets it. The
// connector behind it mints a GitHub installation token used to clone, so a
// file naming another org's id would borrow that org's credential and read its
// private repositories (docs/surface-parity.md, bugs found).
func TestConnectorForTileRefusesAnotherOrgsConnector(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, false)
	now := time.Now().UTC()

	other := &repo.Org{ID: "org-other", Name: "Other", Slug: "other", CreatedAt: now}
	require.NoError(t, store.CreateOrg(ctx, other), "create the other org")

	theirs := &repo.Connector{ID: "cn-theirs", OrgID: other.ID, Provider: "github",
		Name: "theirs", CreatedAt: now}
	require.NoError(t, store.CreateConnector(ctx, theirs), "create their connector")

	ours := &repo.Connector{ID: "cn-ours", OrgID: seed.Org.ID, Provider: "github",
		Name: "ours", CreatedAt: now}
	require.NoError(t, store.CreateConnector(ctx, ours), "create our connector")

	tile := &repo.Tile{ID: "1a3c5e70-0000-4000-8000-00000000abcd",
		StackID: seed.Stack.ID, EnvironmentID: seed.Env.ID,
		Name: "api", Slug: "api", Kind: "app", Status: "running",
		ConnectorID: theirs.ID, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, store.CreateTile(ctx, tile), "create tile")

	c := New(store, "https://panel.example")
	assert.Nil(t, c.connectorForTile(ctx, tile),
		"a connector owned by another org must not resolve")

	tile.ConnectorID = ours.ID
	got := c.connectorForTile(ctx, tile)
	require.NotNil(t, got, "the org's own connector still resolves")
	assert.Equal(t, ours.ID, got.ID)
}

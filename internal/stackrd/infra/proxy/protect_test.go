package proxy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/config/settings"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo/sqlite"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// brokenStack is a real store whose stack level cannot be read, which is what
// a cancelled request or a DB error looks like to the cascade.
type brokenStack struct {
	repo.Store
}

func (brokenStack) GetStack(context.Context, string) (*repo.Stack, error) {
	return nil, errors.New("database is closed")
}

// protectedSeed returns a store and tile with protection set at stack level.
func protectedSeed(t *testing.T) (*sqlite.Store, *repo.Tile) {
	t.Helper()
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)
	on, user, pass := true, "admin", "hunter2"
	seed.Stack.Settings = settings.Settings{Protect: &on, ProtectUser: &user, ProtectPassword: &pass}.JSON()
	require.NoError(t, s.UpdateStack(context.Background(), seed.Stack))
	// WriteApp names files after the first 8 characters of the tile id.
	tile := *seed.Tile
	tile.ID = "abcdefgh-1234"
	return s, &tile
}

func TestWriteAppFailsClosedOnStoreError(t *testing.T) {
	s, tile := protectedSeed(t)
	domains := []repo.Domain{{ID: "d1", Host: "a.example.com", ContainerPort: 80, HTTPS: true}}
	p := &Proxy{dir: t.TempDir(), store: s, acmeEmail: "ops@example.com"}
	require.NoError(t, os.MkdirAll(filepath.Join(p.dir, "dynamic"), 0o755))
	path := filepath.Join(p.dir, "dynamic", "app-"+tile.ID+".yml")

	require.NoError(t, p.WriteApp(tile, domains))
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(before), "basicAuth", "the cascade protects this tile")

	p.store = brokenStack{s}
	assert.Error(t, p.WriteApp(tile, domains), "an unreadable cascade is not an unprotected tile")
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after), "the protected route must stay on disk")
}

func TestLevelsReturnsStoreError(t *testing.T) {
	s, tile := protectedSeed(t)
	ctx := context.Background()

	levels, err := settings.Levels(ctx, s, "", tile.StackID, "")
	require.NoError(t, err)
	assert.Equal(t, []string{"server", "org", "stack"}, kinds(levels))

	_, err = settings.Levels(ctx, brokenStack{s}, "", tile.StackID, "")
	assert.Error(t, err)

	_, err = settings.TryTile(ctx, brokenStack{s}, tile)
	assert.Error(t, err)
	assert.False(t, settings.ForTile(ctx, brokenStack{s}, tile).Protect,
		"ForTile still swallows, which is why Protect must not use it")
}

func kinds(levels []settings.Level) []string {
	out := make([]string, 0, len(levels))
	for _, l := range levels {
		out = append(out, l.Kind)
	}
	return out
}

// Package testdb spins up a migrated in-memory SQLite store for tests.
// It is the shared harness behind the drift/security fix acceptance tests,
// one migrated Store plus a minimal org→stack→env→tile seed so tests can
// exercise real store methods and DB constraints (unique indexes, FKs)
// instead of mocking them.
package testdb

import (
	"context"
	"testing"
	"time"

	hamrsqlite "github.com/FyrmForge/hamr/pkg/db/sqlite"
	"github.com/FyrmForge/stackr/internal/stackrd/store/db"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo/sqlite"
)

// New returns a fresh migrated in-memory store. The DB is closed on test cleanup.
func New(t testing.TB) *sqlite.Store {
	t.Helper()
	conn, err := hamrsqlite.Connect(":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := hamrsqlite.Migrate(conn, db.MigrateConfig()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return sqlite.NewStore(conn)
}

// Seed is a minimal object graph: one org, stack, static env, and service tile.
type Seed struct {
	Org   *repo.Org
	Stack *repo.Stack
	Env   *repo.Environment
	Tile  *repo.Tile
}

// SeedStack creates org→stack→env→tile so a test can attach domains/volumes to
// Tile without hand-building the FK chain each time. managed controls whether
// the stack is config-managed (ConfigManaged() true).
func SeedStack(t testing.TB, s *sqlite.Store, managed bool) Seed {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	// Onboarded, because every test that uses this seed is testing what a
	// working org does. An org with setup_done_at still null answers nothing
	// but its own wizard (middleware.RequireOrgSetup).
	org := &repo.Org{ID: "org1", Name: "Org", Slug: "org", CreatedAt: now, SetupDoneAt: &now}
	if err := s.CreateOrg(ctx, org); err != nil {
		t.Fatalf("create org: %v", err)
	}
	st := &repo.Stack{ID: "stack1", OrgID: org.ID, Name: "Stack", Slug: "stack", CreatedAt: now}
	if managed {
		st.ConfigConnectorID, st.ConfigRepo = "conn1", "org/cfg"
	}
	if err := s.CreateStack(ctx, st); err != nil {
		t.Fatalf("create stack: %v", err)
	}
	env := &repo.Environment{ID: "env1", StackID: st.ID, Name: "prod", Slug: "prod", Type: "static", CreatedAt: now}
	if err := s.CreateEnvironment(ctx, env); err != nil {
		t.Fatalf("create env: %v", err)
	}
	tile := &repo.Tile{ID: "tile1", StackID: st.ID, EnvironmentID: env.ID, Name: "app", Slug: "app",
		Kind: "service", SourceType: "image", Status: "idle", CreatedAt: now, UpdatedAt: now}
	if err := s.CreateTile(ctx, tile); err != nil {
		t.Fatalf("create tile: %v", err)
	}
	return Seed{Org: org, Stack: st, Env: env, Tile: tile}
}

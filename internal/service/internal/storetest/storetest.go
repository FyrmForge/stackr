// Package storetest is a migrated store with no orchestrator, for tests of
// the layers below it (store, leaves, flows). servicetest builds on it; flow
// tests use it directly, since servicetest imports the orchestrator and so
// every flow.
package storetest

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/FyrmForge/hamr/pkg/db/sqlite"

	appdb "github.com/FyrmForge/stackr/internal/db"
	"github.com/FyrmForge/stackr/internal/service/internal/secrets"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Key is the master key every harness uses.
var Key = strings.Repeat("ab", 32)

// Store is a fresh migrated store.
func Store(t *testing.T) *store.Store {
	t.Helper()
	st := Open(t, filepath.Join(t.TempDir(), "stackr.db"))
	if err := sqlite.Migrate(st.DB(), appdb.MigrateConfig()); err != nil {
		t.Fatal(err)
	}
	return st
}

// Open is a store on an existing database file, closed with the test.
func Open(t *testing.T, path string) *store.Store {
	t.Helper()
	db, err := sqlite.Connect(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var fk int
	if err := db.Get(&fk, "PRAGMA foreign_keys"); err != nil || fk != 1 {
		t.Fatalf("foreign_keys = %d, %v; want 1", fk, err)
	}
	box, err := secrets.New(Key)
	if err != nil {
		t.Fatal(err)
	}
	return store.New(db, box)
}

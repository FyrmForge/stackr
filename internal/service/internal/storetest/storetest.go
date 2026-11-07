// Package storetest is a migrated store with no orchestrator, for tests of
// the layers below it (store, leaves, flows). servicetest builds on it; flow
// tests use it directly, since servicetest imports the orchestrator and so
// every flow.
package storetest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/FyrmForge/hamr/pkg/db/sqlite"

	appdb "github.com/FyrmForge/stackr/internal/db"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/secrets"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Key is the master key every harness uses.
var Key = strings.Repeat("ab", 32)

// migrated is the schema, migrated once per test binary: the
// migration costs a moment each time (seconds under -race), and
// a hundred tests asking for a store took CI past its timeout.
var migrated = sync.OnceValues(func() ([]byte, error) {
	dir, err := os.MkdirTemp("", "stackr-schema-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "stackr.db")
	db, err := sqlite.Connect(path)
	if err != nil {
		return nil, err
	}
	if err := sqlite.Migrate(db, appdb.MigrateConfig()); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := db.Close(); err != nil { // checkpoints the WAL into the file
		return nil, err
	}
	return os.ReadFile(path)
})

// Schema is an empty migrated database file's bytes, for a test that boots
// an orchestrator on a new file: its migrate step then has nothing to run.
func Schema(t *testing.T) []byte {
	t.Helper()
	schema, err := migrated()
	if err != nil {
		t.Fatal(err)
	}
	return schema
}

// Store is a fresh migrated store.
func Store(t *testing.T) *store.Store {
	t.Helper()
	schema := Schema(t)
	path := filepath.Join(t.TempDir(), "stackr.db")
	if err := os.WriteFile(path, schema, 0o600); err != nil {
		t.Fatal(err)
	}
	return Open(t, path)
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

// Controller is the controller node row of st, made when missing, with the
// default placement on it, as the boot path does. An env fixture that bypasses the env leaf puts its ID in node_id.
func Controller(t *testing.T, st *store.Store) store.Node {
	t.Helper()
	ctx := context.Background()
	n, err := node.New(st.Nodes).Controller(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// default stands on the controller, as the boot path makes it.
	if p, err := st.Placements.GetByName(ctx, "default"); err != nil {
		t.Fatal(err)
	} else if p.NodeID == "" {
		if err := st.Placements.SetNode(ctx, p.ID, n.ID); err != nil {
			t.Fatal(err)
		}
	}
	return n
}

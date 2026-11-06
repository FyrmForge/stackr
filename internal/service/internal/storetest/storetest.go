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

// migrated is the schema, migrated once per test binary: the 90-odd
// migrations cost about half a second each time (seconds under -race), and
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

var shapes = struct {
	sync.Mutex
	m map[uint][]byte
}{m: map[uint][]byte{}}

// SchemaAt is the migrated database file's bytes at an older migration
// version. Walking down from the head costs seconds per step under -race, so
// the walk keeps every version it passes: the first caller pays, the rest
// copy.
func SchemaAt(t *testing.T, version uint) []byte {
	t.Helper()
	shapes.Lock()
	defer shapes.Unlock()
	if b, ok := shapes.m[version]; ok {
		return b
	}
	cfg := appdb.MigrateConfig()
	path := filepath.Join(t.TempDir(), "shape.db")
	// the nearest shape above the wanted one, else the head
	base, from := Schema(t), uint(0)
	for v, b := range shapes.m {
		if v > version && (from == 0 || v < from) {
			base, from = b, v
		}
	}
	if err := os.WriteFile(path, base, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sqlite.Connect(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	v, _, err := sqlite.MigrateVersion(db, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for ; v > version; v-- {
		if err := sqlite.MigrateSteps(db, cfg, -1); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		shapes.m[v-1] = b
	}
	if b, ok := shapes.m[version]; ok {
		return b
	}
	t.Fatalf("storetest: no schema at version %d", version)
	return nil
}

// StoreAt is a store on a database at an older migration version, for a
// migration test that starts from the shape before it.
func StoreAt(t *testing.T, version uint) *store.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stackr.db")
	if err := os.WriteFile(path, SchemaAt(t, version), 0o600); err != nil {
		t.Fatal(err)
	}
	return Open(t, path)
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

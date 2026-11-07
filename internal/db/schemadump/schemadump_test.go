package schemadump_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FyrmForge/hamr/pkg/db/sqlite"
	"github.com/jmoiron/sqlx"

	appdb "github.com/FyrmForge/stackr/internal/db"
	"github.com/FyrmForge/stackr/internal/db/schemadump"
)

func migrated(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlite.ConnectContext(context.Background(), filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(db, appdb.MigrateConfig()); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestDumpDeterministic(t *testing.T) {
	t.Parallel()
	db := migrated(t)
	a, err := schemadump.Dump(db.DB)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := schemadump.Dump(db.DB)
	if a != b || a == "" {
		t.Fatal("dump not deterministic or empty")
	}
	if !strings.Contains(a, "on_delete=CASCADE") || strings.Contains(a, "schema_migrations") {
		t.Fatal("dump missing CASCADE or includes bookkeeping")
	}
	if d := schemadump.Diff(a, b); d != "" {
		t.Fatalf("diff of equal: %q", d)
	}
	if d := schemadump.Diff("a\nb", "a\nc"); d != "- b\n+ c\n" {
		t.Fatalf("diff = %q", d)
	}
}

// The migrated schema matches the committed snapshot, so every schema change
// shows in review. After a deliberate change:
// UPDATE_SNAPSHOT=1 go test ./internal/db/schemadump
func TestSchemaMatchesSnapshot(t *testing.T) {
	t.Parallel()
	got, err := schemadump.Dump(migrated(t).DB)
	if err != nil {
		t.Fatal(err)
	}
	const path = "testdata/schema.snapshot"
	if os.Getenv("UPDATE_SNAPSHOT") != "" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if d := schemadump.Diff(string(want), got); d != "" {
		t.Fatalf("schema differs from %s (UPDATE_SNAPSHOT=1 after a deliberate change):\n%s", path, d)
	}
}

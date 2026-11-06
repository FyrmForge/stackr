package db_test

import (
	"context"
	"io/fs"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/FyrmForge/hamr/pkg/db/sqlite"
	"github.com/jmoiron/sqlx"

	appdb "github.com/FyrmForge/stackr/internal/db"
)

// A migration runs inside a transaction, where SQLite ignores PRAGMA
// foreign_keys. Rebuilding (DROP TABLE and rename) a table that other tables
// reference therefore cascade-deletes their rows. Migrations up to
// legacyMigrations did it before this guard; a newer one that has to marks the
// file with "-- cascade-safe: <why>".
const legacyMigrations = 40

var dropTable = regexp.MustCompile(`(?im)^\s*DROP\s+TABLE\s+(?:IF\s+EXISTS\s+)?"?(\w+)"?`)

// referenced returns the tables sql drops that another table's foreign key
// points at, in the schema db has now.
func referenced(t *testing.T, db *sqlx.DB, sql string) []string {
	t.Helper()
	var tables []string
	if err := db.Select(&tables, `SELECT name FROM sqlite_master WHERE type = 'table'`); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range dropTable.FindAllStringSubmatch(sql, -1) {
		for _, from := range tables {
			if from == m[1] {
				continue
			}
			var refs []string
			if err := db.Select(&refs, `SELECT "table" FROM pragma_foreign_key_list(?)`, from); err != nil {
				t.Fatal(err)
			}
			if slices.Contains(refs, m[1]) {
				out = append(out, m[1]+" (from "+from+")")
			}
		}
	}
	return out
}

func TestGuardSeesReferencedDrop(t *testing.T) {
	t.Parallel()
	db, err := sqlite.ConnectContext(context.Background(), filepath.Join(t.TempDir(), "g.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.MustExec(`CREATE TABLE p (id TEXT PRIMARY KEY); CREATE TABLE c (id TEXT, p TEXT REFERENCES p(id) ON DELETE CASCADE)`)
	if got := referenced(t, db, "DROP TABLE p;"); len(got) != 1 {
		t.Errorf("referenced = %v", got)
	}
	if got := referenced(t, db, "DROP TABLE c;"); len(got) != 0 {
		t.Errorf("referenced = %v", got)
	}
}

func TestNoNewMigrationRebuildsAReferencedTable(t *testing.T) {
	t.Parallel()
	cfg := appdb.MigrateConfig()
	files, err := fs.Glob(cfg.FS, cfg.Directory+"/*.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(files)
	db, err := sqlite.ConnectContext(context.Background(), filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, f := range files {
		b, err := fs.ReadFile(cfg.FS, f)
		if err != nil {
			t.Fatal(err)
		}
		n, _ := strconv.Atoi(strings.SplitN(filepath.Base(f), "_", 2)[0])
		if n > legacyMigrations && !strings.Contains(string(b), "-- cascade-safe:") {
			if got := referenced(t, db, string(b)); len(got) > 0 {
				t.Errorf("%s drops tables other tables reference, which cascade-deletes their rows: %v", f, got)
			}
		}
		if _, err := db.Exec(string(b)); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
}

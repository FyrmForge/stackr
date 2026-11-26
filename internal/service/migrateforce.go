package service

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/FyrmForge/hamr/pkg/db/sqlite"

	appdb "github.com/FyrmForge/stackr/internal/db"
	"github.com/FyrmForge/stackr/internal/paneldb"
)

// ForceMigration is the operator's way out of a failed migration: the
// database is marked dirty at the failed version and every start refuses it.
// A negative version only reports where the database stands. Otherwise the
// version is set without running anything, so the next start runs the
// migrations after it again (force the version before the one that failed).
// It opens the file alone, so it runs while the panel is down.
func ForceMigration(ctx context.Context, cfg Config, version int, out io.Writer) error {
	cfg.DBPath = DBPathOr(cfg.DataDir, cfg.DBPath)
	if err := paneldb.Check(cfg.DataDir, cfg.DBPath); err != nil {
		return err
	}
	if _, err := os.Stat(cfg.DBPath); err != nil {
		return fmt.Errorf("no database to force: %w", err) // connecting would create an empty one
	}
	db, err := sqlite.ConnectContext(ctx, cfg.DBPath)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer func() { _ = db.Close() }()
	v, dirty, err := sqlite.MigrateVersion(db, appdb.MigrateConfig())
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "schema version %d, dirty %t\n", v, dirty)
	if version < 0 {
		return nil
	}
	if err := sqlite.MigrateForce(db, appdb.MigrateConfig(), version); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "schema version forced to %d; the next start runs the migrations after it\n", version)
	return nil
}

// DBPathOr is the effective database path: the configured one, else the
// layout's <DataDir>/db/stackr.db. Every entry point that opens the database
// takes its fallback from here.
func DBPathOr(dataDir, dbPath string) string {
	if dbPath == "" {
		return paneldb.Path(dataDir)
	}
	return dbPath
}

// CheckPanelDB is checkPanelDB: the refusal each stackrd entry point that
// touches the database runs first, before any key or database file is made.
func CheckPanelDB(dataDir, dbPath string) error {
	return paneldb.Check(dataDir, DBPathOr(dataDir, dbPath))
}

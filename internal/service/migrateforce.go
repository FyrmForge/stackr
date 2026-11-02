package service

import (
	"context"
	"fmt"
	"io"
	"path/filepath"

	"github.com/FyrmForge/hamr/pkg/db/sqlite"

	appdb "github.com/FyrmForge/stackr/internal/db"
)

// ForceMigration is the operator's way out of a failed migration: the
// database is marked dirty at the failed version and every start refuses it.
// A negative version only reports where the database stands. Otherwise the
// version is set without running anything, so the next start runs the
// migrations after it again (force the version before the one that failed).
// It opens the file alone, so it runs while the panel is down.
func ForceMigration(ctx context.Context, cfg Config, version int, out io.Writer) error {
	if cfg.DBPath == "" {
		cfg.DBPath = filepath.Join(cfg.DataDir, "stackr.db")
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

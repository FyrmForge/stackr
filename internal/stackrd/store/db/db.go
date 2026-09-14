// Package db opens the SQLite connection and owns the embedded migrations,
// which run at startup. It is the only place that knows where the database
// file lives or what pragmas it needs.
package db

import (
	"embed"

	"github.com/FyrmForge/hamr/pkg/db/sqlite"
	"github.com/jmoiron/sqlx"
)

//go:embed migrations/*.sql
var migrations embed.FS

// MigrateConfig returns the migration configuration for this project.
func MigrateConfig() sqlite.MigrateConfig {
	return sqlite.MigrateConfig{
		FS:        migrations,
		Directory: "migrations",
	}
}

// Migrate brings the database to head, which since 2026-09-14 is one file:
// 001_initial, squashed through the old 018. No adoption shim, so any database
// at an older version starts again from empty. Safe because stackr has no
// install anywhere but the test rig; the chain starts growing again at 002 the
// day there is data that has to survive an upgrade.
func Migrate(conn *sqlx.DB) error {
	return sqlite.Migrate(conn, MigrateConfig())
}

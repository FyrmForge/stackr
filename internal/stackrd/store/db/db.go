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

// Migrate brings the database to head. 001_initial is one file, squashed
// through the old 018 on 2026-09-14, and frozen on 2026-09-18: the chain grows
// at 002 from here on, forward-only and additive, and migrate_guard_test.go
// enforces that. There is no adoption shim, so a database written before the
// squash starts again from empty; nothing but the test rig ever held one.
func Migrate(conn *sqlx.DB) error {
	return sqlite.Migrate(conn, MigrateConfig())
}

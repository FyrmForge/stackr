package db

import (
	"embed"
	"github.com/FyrmForge/hamr/pkg/db/sqlite"
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

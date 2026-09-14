package managedtiles_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/managedtiles"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// A restore has to replace what is there, not add to it. Postgres dumps carry
// no DROP statements, so a bare `psql < dump` duplicated every row and still
// reported success. Every engine that restores must wipe
// first, and a failure halfway has to be reported rather than swallowed. (When
// mariadb comes back, its dump carries DROP TABLE IF EXISTS, so the check for
// it is that the dump keeps --add-drop-table; mongo restores with --drop.)
func TestRestoreReplacesRatherThanMerges(t *testing.T) {
	d := &repo.Tile{DBName: "app", DBUser: "app", DBPassword: "s3cr3t"}
	for kind, eng := range managedtiles.Engines {
		if eng.RestoreCmd == nil {
			continue // s3 restores by volume, not by dump
		}
		cmd := strings.Join(eng.RestoreCmd(d), " ")
		switch kind {
		case "postgres":
			assert.Contains(t, cmd, "DROP SCHEMA public CASCADE", "%s: restore must wipe first", kind)
			assert.Contains(t, cmd, "ON_ERROR_STOP=1", "%s: a half-restore must fail loudly", kind)
			// The password travels as a positional arg, never inside the
			// script text, so it stays out of the command line we log.
			assert.NotContains(t, strings.Join(eng.RestoreCmd(d)[:3], " "), "s3cr3t", "%s: password in the script", kind)
		default:
			t.Errorf("%s restores but this test has no opinion on how it avoids merging", kind)
		}
	}
}

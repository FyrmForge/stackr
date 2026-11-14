package db_test

import (
	"os"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// Migration 014 flips metrics history to off by default; an install that
// already ran (it has a node) and never stored the switch keeps it on, one
// that stored a choice keeps that, and a fresh database stays on the default.
func TestObserveDefaultOffKeepsRunningInstalls(t *testing.T) {
	b, err := os.ReadFile("migrations/014_observe_default_off.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	stored := func(db *sqlx.DB) []string {
		var v []string
		if err := db.Select(&v, `SELECT value FROM settings WHERE key = 'observe_enabled'`); err != nil {
			t.Fatal(err)
		}
		return v
	}
	run := func(db *sqlx.DB) {
		t.Helper()
		execAll(t, db, []stmt{{string(b), nil}})
	}
	node := stmt{`INSERT INTO nodes (id, name, slug, role, tags, range_cidr, wg_public_key, wg_address, status, created_at) VALUES ('n1','controller','controller','controller','','10.0.0.0/24','k','10.0.0.1','online',?)`, []any{time.Now()}}

	fresh := servicetest.Store(t).DB()
	run(fresh)
	if v := stored(fresh); len(v) != 0 {
		t.Errorf("a fresh database stored observe_enabled = %v", v)
	}

	running := servicetest.Store(t).DB()
	execAll(t, running, []stmt{node})
	run(running)
	if v := stored(running); len(v) != 1 || v[0] != "true" {
		t.Errorf("a running install stored %v, want true", v)
	}

	chose := servicetest.Store(t).DB()
	execAll(t, chose, []stmt{node, {`INSERT INTO settings (key, value) VALUES ('observe_enabled', 'false')`, nil}})
	run(chose)
	if v := stored(chose); len(v) != 1 || v[0] != "false" {
		t.Errorf("a stored choice became %v, want false", v)
	}
}

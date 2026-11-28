package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/FyrmForge/stackr/internal/paneldb"
)

// dbAt is the database path a boot on dir uses by default (db/ made).
func dbAt(dir string) string {
	if err := os.MkdirAll(filepath.Join(dir, paneldb.DirName), 0o700); err != nil {
		panic(err)
	}
	return paneldb.Path(dir)
}

// TestMain migrates one empty database up front. Most tests boot an
// orchestrator on a fresh data dir, and running every migration each time
// was a large share of the suite's time.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "stackr-seed")
	if err != nil {
		panic(err)
	}
	path := filepath.Join(dir, "stackr.db")
	db, err := openDB(context.Background(), path)
	if err != nil {
		panic(err)
	}
	_ = db.Close()
	if seedDB, err = os.ReadFile(path); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

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

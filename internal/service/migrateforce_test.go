package service

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FyrmForge/hamr/pkg/db/sqlite"

	appdb "github.com/FyrmForge/stackr/internal/db"
)

func TestForceMigration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "stackr.db")
	db, err := openDB(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	top, _, err := sqlite.MigrateVersion(db, appdb.MigrateConfig())
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	cfg := Config{DBPath: path}
	var out bytes.Buffer
	if err := ForceMigration(ctx, cfg, -1, &out); err != nil || strings.Contains(out.String(), "forced") {
		t.Fatalf("report = %q %v", out.String(), err)
	}
	if err := ForceMigration(ctx, cfg, int(top)-1, &out); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := ForceMigration(ctx, cfg, -1, &out); err != nil || !strings.Contains(out.String(), fmt.Sprintf("version %d, dirty false", top-1)) {
		t.Fatalf("after force = %q %v", out.String(), err)
	}
}

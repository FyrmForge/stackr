package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/FyrmForge/stackr/internal/installspec"
)

// With no install.json the panel moves an old-path database into db/ itself;
// with one (an installed panel) it leaves that to the swap helper and refuses.
func TestCheckPanelDBAdoptsOnlyWithoutInstall(t *testing.T) {
	old, oldDB := envDataDir, envDatabasePath
	t.Cleanup(func() { envDataDir, envDatabasePath = old, oldDB })
	envDatabasePath = ""

	dev := t.TempDir()
	envDataDir = dev
	if err := os.WriteFile(filepath.Join(dev, "stackr.db"), []byte("db"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkPanelDB(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dev, "db", "stackr.db")); string(b) != "db" {
		t.Errorf("dev database not moved: %q", b)
	}

	inst := t.TempDir()
	envDataDir = inst
	if err := os.WriteFile(filepath.Join(inst, "stackr.db"), []byte("db"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inst, installspec.File), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkPanelDB(); err == nil {
		t.Error("an installed panel accepted an old-path database")
	}
	if b, _ := os.ReadFile(filepath.Join(inst, "stackr.db")); string(b) != "db" {
		t.Error("the installed panel's database moved")
	}
}

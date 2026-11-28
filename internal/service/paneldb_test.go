package service

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/paneldb"
)

// listing is every path under dir, to prove a refusal made nothing.
func listing(t *testing.T, dir string) string {
	t.Helper()
	var out []string
	must(t, filepath.WalkDir(dir, func(p string, _ os.DirEntry, err error) error {
		out = append(out, p)
		return err
	}))
	return strings.Join(out, "\n")
}

// Every entry point that touches the database refuses first, through the same
// check and the fallback path, and leaves no database, no recovery key and no
// directory behind.
func TestPanelDBCheckAtEveryEntryPoint(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, dir string)
		want  string
	}{
		{"layout change in progress", func(t *testing.T, dir string) {
			must(t, paneldb.WriteJournal(dir, paneldb.Journal{Phase: paneldb.PhaseLayout, Direction: paneldb.DirLayout}))
		}, `phase "layout"`},
		{"database on a disk that is not mounted", func(t *testing.T, dir string) {
			must(t, paneldb.EnsureLayout(dir))
			must(t, paneldb.WriteJournal(dir, paneldb.Journal{Phase: paneldb.PhaseMoved, DiskID: "d1", InstallID: "i1"}))
		}, "disk missing"},
		{"database still at the old path", func(t *testing.T, dir string) {
			must(t, os.WriteFile(paneldb.Legacy(dir), []byte("db"), 0o600))
		}, "still at"},
	} {
		entries := map[string]func(cfg Config) error{
			"New": func(cfg Config) error { _, err := New(cfg); return err },
			"create-admin --check": func(cfg Config) error {
				_, err := NeedsAdmin(ctx, cfg)
				return err
			},
			"create-admin": func(cfg Config) error {
				_, _, err := CreateAdmin(ctx, cfg, "a@example.com", "a-long-enough-password", "Admin")
				return err
			},
			"admin-key": func(cfg Config) error {
				_, _, _, err := MintAdminKey(ctx, cfg)
				return err
			},
			"migrate-force": func(cfg Config) error { return ForceMigration(ctx, cfg, -1, io.Discard) },
			"master key rotation": func(cfg Config) error {
				_, err := LoadMasterKey(ctx, cfg.DataDir, cfg.DBPath, "")
				return err
			},
		}
		for name, entry := range entries {
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				dir := t.TempDir()
				tc.setup(t, dir)
				if name == "master key rotation" {
					must(t, installspec.WriteKeyFile(filepath.Join(dir, installspec.KeyFile), []byte(testKey+"\n")))
					must(t, installspec.WriteKeyFile(filepath.Join(dir, installspec.KeyNextFile), []byte(nextKey+"\n")))
				}
				before := listing(t, dir)
				// no DBPath: the fallback is what is judged
				err := entry(Config{DataDir: dir, SecretsKey: testKey})
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.want)
				}
				if after := listing(t, dir); after != before {
					t.Errorf("a refusal changed the data dir:\nbefore:\n%s\nafter:\n%s", before, after)
				}
			})
		}
	}
}

// An older binary resolves the old path, which is now a directory: opening it
// fails, and nothing is created. It is EISDIR from the driver, not a refusal
// the old code wrote.
func TestOldPathIsADirectoryNotADatabase(t *testing.T) {
	dir := t.TempDir()
	must(t, paneldb.EnsureLayout(dir))
	_, err := openDB(context.Background(), paneldb.Legacy(dir))
	if err == nil {
		t.Fatal("opened a directory as a database")
	}
	if !strings.Contains(err.Error(), "directory") && !strings.Contains(err.Error(), "unable to open") {
		t.Errorf("err = %v", err)
	}
	if fi, err := os.Stat(paneldb.Legacy(dir)); err != nil || !fi.IsDir() {
		t.Error("the placeholder is not a directory any more")
	}
	// and migrate-force, which stats first, fails cleanly on the old path too
	if err := ForceMigration(context.Background(), Config{DataDir: dir, DBPath: paneldb.Legacy(dir)}, -1, io.Discard); err == nil {
		t.Error("migrate-force worked on the placeholder")
	}
}

// The fallback is the layout's path, for every entry point alike.
func TestDBPathFallback(t *testing.T) {
	if got, want := DBPathOr("/d", ""), "/d/db/stackr.db"; got != want {
		t.Errorf("fallback = %q, want %q", got, want)
	}
	if got := DBPathOr("/d", "/x/y.db"); got != "/x/y.db" {
		t.Errorf("configured path = %q", got)
	}
}

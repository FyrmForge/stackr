package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/paneldb"
)

// liveInstall is a laid-out install with a live database and its wal and shm.
func liveInstall(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := paneldb.EnsureLayout(dir); err != nil {
		t.Fatal(err)
	}
	put(t, paneldb.Path(dir), "live")
	put(t, paneldb.Path(dir)+"-wal", "live wal")
	put(t, paneldb.Path(dir)+"-shm", "live shm")
	return dir
}

func stageWith(t *testing.T, body string) string {
	t.Helper()
	stage := filepath.Join(t.TempDir(), "restore-20261126T101500")
	put(t, filepath.Join(stage, "stackr.db"), body)
	return stage
}

// The restore is killed at every step. Until it finishes, the boot check
// refuses (no empty database is made, whatever is or is not at db/stackr.db);
// running the restore again ends with exactly the restored database, no wal or
// shm of the old one beside it, and the old set kept in swap/.
func TestRestoreCrashAtEveryStep(t *testing.T) {
	steps := []string{"journaled", "aside-wal", "aside", "installed", "layout"}
	for _, step := range steps {
		t.Run(step, func(t *testing.T) {
			dir := liveInstall(t)
			restoreCrash = func(s string) error {
				if s == step {
					return errors.New("crash at " + s)
				}
				return nil
			}
			t.Cleanup(func() { restoreCrash = nil })
			err := beginRestore(dir, "20261126T101500")
			if err == nil {
				if step == "journaled" {
					err = errors.New("crash at journaled")
				} else {
					err = installRestoredDB(dir, stageWith(t, "restored"))
				}
			}
			if err == nil || !strings.Contains(err.Error(), "crash at "+step) {
				t.Fatalf("no crash at %s: %v", step, err)
			}
			restoreCrash = nil

			if err := paneldb.Check(dir, paneldb.Path(dir)); err == nil {
				t.Error("the boot check lets a half-done restore start")
			}
			if err := paneldb.CheckInstaller(dir); err == nil {
				t.Error("the installer lets a half-done restore through")
			}
			if err := restorePanelDB(dir); err != nil {
				t.Fatalf("restore again is refused: %v", err)
			}

			// again, as `stackr-install restore` would
			if err := beginRestore(dir, "20261126T101999"); err != nil {
				t.Fatal(err)
			}
			if err := installRestoredDB(dir, stageWith(t, "restored")); err != nil {
				t.Fatal(err)
			}
			if get(paneldb.Path(dir)) != "restored" {
				t.Errorf("db = %q", get(paneldb.Path(dir)))
			}
			for _, suf := range []string{"-wal", "-shm"} {
				if _, err := os.Lstat(paneldb.Path(dir) + suf); err == nil {
					t.Errorf("old %s sits beside the restored database", suf)
				}
			}
			if err := paneldb.Check(dir, paneldb.Path(dir)); err != nil {
				t.Errorf("boot check after the restore: %v", err)
			}
			if j, _ := paneldb.ReadJournal(dir); j.Phase != paneldb.PhaseNone {
				t.Errorf("journal = %q", j.Phase)
			}
			if fi, err := os.Stat(paneldb.Legacy(dir)); err != nil || !fi.IsDir() {
				t.Errorf("placeholder: %v", err)
			}
			// the live set is kept together under the first run's name, unless a crash
			// after the restored file went in made the second run keep that one too
			kept := filepath.Join(dir, "swap", "stackr.db.before-restore-20261126T101500")
			if get(kept) != "live" || get(kept+"-wal") != "live wal" {
				t.Errorf("kept set = %q %q", get(kept), get(kept+"-wal"))
			}
		})
	}
}

// A restore onto a box with an old-layout file at the old path keeps that too,
// and a first-time box (nothing there) restores cleanly.
func TestRestoreOldLayoutAndBareBox(t *testing.T) {
	old := t.TempDir()
	put(t, paneldb.Legacy(old), "old layout")
	put(t, paneldb.Legacy(old)+"-wal", "old layout wal")
	if err := beginRestore(old, "S"); err != nil {
		t.Fatal(err)
	}
	if err := installRestoredDB(old, stageWith(t, "restored")); err != nil {
		t.Fatal(err)
	}
	kept := filepath.Join(old, "swap", "stackr.db.before-restore-S.old-layout")
	if get(kept) != "old layout" || get(kept+"-wal") != "old layout wal" || get(paneldb.Path(old)) != "restored" {
		t.Errorf("kept %q %q, db %q", get(kept), get(kept+"-wal"), get(paneldb.Path(old)))
	}
	if err := paneldb.Check(old, paneldb.Path(old)); err != nil {
		t.Error(err)
	}

	bare := t.TempDir()
	if err := paneldb.EnsureFresh(bare); err != nil {
		t.Fatal(err)
	}
	if err := beginRestore(bare, "S"); err != nil {
		t.Fatal(err)
	}
	if err := installRestoredDB(bare, stageWith(t, "restored")); err != nil {
		t.Fatal(err)
	}
	if err := paneldb.Check(bare, paneldb.Path(bare)); err != nil || get(paneldb.Path(bare)) != "restored" {
		t.Errorf("bare box: %v %q", err, get(paneldb.Path(bare)))
	}
}

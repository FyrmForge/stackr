package paneldb

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An install (install.json) never starts a database from nothing, whatever
// else is or is not there. Only a box with no install may (a dev checkout).
func TestInstalledBoxNeverStartsFresh(t *testing.T) {
	installed := t.TempDir()
	write(t, filepath.Join(installed, InstallFile), "{}")
	if err := Check(installed, Path(installed)); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Errorf("boot, old-layout install whose database vanished: %v", err)
	}
	if err := CheckInstaller(installed); err == nil {
		t.Error("the installer let an install with no database through")
	}
	if err := EnsureFresh(installed); err != nil {
		t.Fatal(err)
	}
	if j, _ := ReadJournal(installed); j.Phase != PhaseNone {
		t.Errorf("EnsureFresh wrote %q on an install", j.Phase)
	}
	if err := Check(installed, Path(installed)); err == nil {
		t.Error("an install with the layout made and no database was let through")
	}

	// the database still at the old path is the swap's to move
	old := t.TempDir()
	write(t, filepath.Join(old, InstallFile), "{}")
	write(t, Legacy(old), "db")
	if err := CheckInstaller(old); err != nil {
		t.Errorf("installer on an old-layout install: %v", err)
	}
	if err := Check(old, Path(old)); err == nil || !strings.Contains(err.Error(), "still at") {
		t.Errorf("boot on an old-layout install: %v", err)
	}

	// no install, no layout: a plain binary may create its first database
	dev := t.TempDir()
	if err := Check(dev, Path(dev)); err != nil {
		t.Errorf("dev: %v", err)
	}
}

// The fresh phase is written before anything is made: a crash right after it
// leaves a state the rerun accepts.
func TestEnsureFreshCrashAfterJournal(t *testing.T) {
	dir := t.TempDir()
	must(t, WriteJournal(dir, Journal{Phase: PhaseFresh})) // died here: no db/, no placeholder
	if err := Check(dir, Path(dir)); err != nil {
		t.Fatalf("boot after the crash: %v", err)
	}
	if err := CheckInstaller(dir); err != nil {
		t.Fatalf("installer rerun: %v", err)
	}
	must(t, EnsureFresh(dir))
	if j, _ := ReadJournal(dir); j.Phase != PhaseFresh {
		t.Errorf("journal = %q", j.Phase)
	}
	if fi, err := os.Stat(Legacy(dir)); err != nil || !fi.IsDir() {
		t.Error("layout not completed")
	}
	// died after db/ but before the placeholder
	dir2 := t.TempDir()
	must(t, WriteJournal(dir2, Journal{Phase: PhaseFresh}))
	must(t, os.Mkdir(filepath.Join(dir2, DirName), 0o700))
	must(t, CheckInstaller(dir2))
	must(t, EnsureFresh(dir2))
}

// Once the new panel has run, a rollback needs its copy: without it nothing
// is rolled back and the journal stays, so v0.8.36 is never started on a
// database the new panel migrated.
func TestRecoverRefusesWithoutTheCopyOnceRan(t *testing.T) {
	for _, dir := range []string{DirLayout, DirUpdate} {
		t.Run(dir, func(t *testing.T) {
			d := t.TempDir()
			must(t, EnsureLayout(d))
			write(t, Path(d), "migrated by the new panel")
			j := Journal{Phase: PhaseStartingNew, Direction: dir, Dst: Path(d), Src: Legacy(d),
				Copy: filepath.Join(Swap(d), "stackr.db.pre-1"), Ran: true}
			must(t, WriteJournal(d, j))
			if err := Recover(d); err == nil || !strings.Contains(err.Error(), "copy is missing") {
				t.Fatalf("Recover = %v", err)
			}
			if got, _ := ReadJournal(d); got.Phase != PhaseStartingNew {
				t.Errorf("journal = %q, must stay", got.Phase)
			}
			if readf(Path(d)) != "migrated by the new panel" {
				t.Error("the database was touched")
			}
			// the copy present: the update direction restores it where it was
			write(t, j.Copy, "pre-upgrade")
			must(t, Recover(d))
			want := Legacy(d)
			if dir == DirUpdate {
				want = Path(d)
			}
			if readf(want) != "pre-upgrade" {
				t.Errorf("restored %s = %q", want, readf(want))
			}
			if got, _ := ReadJournal(d); got.Phase != PhaseNone {
				t.Errorf("journal = %q", got.Phase)
			}
			if _, err := os.Stat(j.Copy); err == nil {
				t.Error("copy left after the journal cleared")
			}
		})
	}
}

// AsideCopy puts an orphan copy out of the way of every later restore.
func TestAsideCopy(t *testing.T) {
	d := t.TempDir()
	cp := filepath.Join(Swap(d), "stackr.db.pre-1")
	write(t, cp, "old")
	write(t, cp+"-wal", "old wal")
	must(t, AsideCopy(d, cp))
	if _, err := os.Stat(cp); err == nil {
		t.Error("still there")
	}
	m, _ := filepath.Glob(filepath.Join(Swap(d), "orphan-*"))
	if len(m) != 2 {
		t.Errorf("aside = %v", m)
	}
}

// AdoptDev killed at each step of its own layout change recovers and retries.
func TestAdoptDevRecoversItsOwnCrash(t *testing.T) {
	for _, step := range []string{"aside-done", "moved", "moved-wal", "moved-shm"} {
		t.Run(step, func(t *testing.T) {
			dir := t.TempDir()
			write(t, Legacy(dir), "db")
			write(t, Legacy(dir)+"-wal", "wal")
			crashHook = func(s string) error {
				if s == step {
					panic("process died at " + s)
				}
				return nil
			}
			func() {
				defer func() { _ = recover() }()
				_, _ = AdoptDev(dir)
				t.Error("no crash")
			}()
			crashHook = nil
			t.Cleanup(func() { crashHook = nil })
			if err := Check(dir, Path(dir)); err == nil {
				t.Error("the boot check lets the dead layout change start")
			}
			moved, err := AdoptDev(dir)
			if err != nil || !moved {
				t.Fatalf("retry: moved=%v err=%v", moved, err)
			}
			if readf(Path(dir)) != "db" || readf(Path(dir)+"-wal") != "wal" {
				t.Errorf("after the retry: %q %q", readf(Path(dir)), readf(Path(dir)+"-wal"))
			}
			if err := Check(dir, Path(dir)); err != nil {
				t.Error(err)
			}
		})
	}
}

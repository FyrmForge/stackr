package paneldb

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readf(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "<gone>"
	}
	return string(b)
}

// crashAt stops the code at the named step, as a dying process would, once.
func crashAt(t *testing.T, step string) {
	t.Helper()
	crashHook = func(s string) error {
		if s == step {
			crashHook = nil
			return errors.New("crash at " + s)
		}
		return nil
	}
	t.Cleanup(func() { crashHook = nil })
}

// Once the layout is established a missing database is a loss: the boot check
// refuses it. Only the explicit fresh phase may create the first one.
func TestMissingDatabaseIsNotAFreshStart(t *testing.T) {
	dir := t.TempDir()
	must(t, EnsureLayout(dir))
	if err := Check(dir, Path(dir)); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("boot: %v", err)
	}
	if err := CheckInstaller(dir); err == nil {
		t.Error("the installer let a missing database through")
	}
	if err := CheckRestore(dir); err != nil {
		t.Errorf("restore, the way out: %v", err)
	}

	fresh := t.TempDir()
	must(t, EnsureFresh(fresh))
	if err := Check(fresh, Path(fresh)); err != nil {
		t.Errorf("a fresh install: %v", err)
	}
	write(t, Path(fresh), "db")
	must(t, Seeded(fresh))
	if j, _ := ReadJournal(fresh); j.Phase != PhaseNone {
		t.Errorf("journal after seeding = %q", j.Phase)
	}
	must(t, os.Remove(Path(fresh)))
	if err := Check(fresh, Path(fresh)); err == nil {
		t.Error("a database that vanished after seeding was treated as fresh")
	}
	// EnsureFresh never turns an established install back into a fresh one.
	must(t, EnsureFresh(fresh))
	if j, _ := ReadJournal(fresh); j.Phase != PhaseNone {
		t.Errorf("EnsureFresh on an established install wrote %q", j.Phase)
	}
}

func TestRestoringJournalRefusesBootAndInstaller(t *testing.T) {
	dir := t.TempDir()
	must(t, EnsureLayout(dir))
	must(t, WriteJournal(dir, Journal{Phase: PhaseRestoring, Direction: DirRestore, Aside: "x"}))
	if err := Check(dir, Path(dir)); err == nil || !strings.Contains(err.Error(), "restore") {
		t.Errorf("boot: %v", err)
	}
	if err := CheckInstaller(dir); err == nil {
		t.Error("installer")
	}
	if err := CheckRestore(dir); err != nil {
		t.Errorf("restore again: %v", err)
	}
}

// both-exist: the old restore's set in db/ is moved to swap/ under a journaled
// name. Killed at every step, recovery never lets a db/ sidecar land beside the
// legacy main, and the old restore's set stays whole in swap/.
func TestSupersedeCrashAtEveryStep(t *testing.T) {
	steps := []string{"aside", "aside-wal", "aside-shm", "aside-done", "moved", "moved-wal", "moved-shm"}
	for _, step := range steps {
		t.Run(step, func(t *testing.T) {
			dir := t.TempDir()
			write(t, Legacy(dir), "legacy")
			write(t, Legacy(dir)+"-wal", "legacy wal")
			write(t, Path(dir), "restored")
			write(t, Path(dir)+"-wal", "restored wal")
			write(t, Path(dir)+"-shm", "restored shm")
			crashAt(t, step)
			if _, err := ApplyLayout(dir, "", "c"); err == nil {
				t.Fatal("no crash")
			}
			crashHook = nil
			must(t, Recover(dir))
			if got := readf(Legacy(dir)); got != "legacy" {
				t.Errorf("legacy main = %q", got)
			}
			if got := readf(Legacy(dir) + "-wal"); got != "legacy wal" {
				t.Errorf("legacy wal = %q (a wal of another generation?)", got)
			}
			if _, err := os.Stat(filepath.Join(dir, DirName)); err == nil {
				t.Error("db/ left behind")
			}
			m, _ := filepath.Glob(filepath.Join(Swap(dir), "db.superseded-*"))
			set := map[string]string{}
			for _, f := range m {
				set[f[strings.Index(f, "db.superseded-")+len("db.superseded-20060102T150405Z"):]] = readf(f)
			}
			if set[""] != "restored" || set["-wal"] != "restored wal" {
				t.Errorf("the superseded set is not whole: %v", set)
			}
			if j, _ := ReadJournal(dir); j.Phase != PhaseNone {
				t.Errorf("journal = %q", j.Phase)
			}
			// and the layout change can run again from here
			if applied, err := ApplyLayout(dir, "", "c"); err != nil || !applied {
				t.Fatalf("again: %v %v", applied, err)
			}
			if readf(Path(dir)) != "legacy" || readf(Path(dir)+"-wal") != "legacy wal" {
				t.Error("second run did not adopt the legacy files")
			}
		})
	}
}

// Rolling back restores the pre-upgrade copy without consuming it: killed
// between the main and the wal, the copy is whole and the next run finishes.
func TestRecoverKeepsTheCopyUntilTheJournalIsClear(t *testing.T) {
	for _, step := range []string{"restored", "restored-wal"} {
		t.Run(step, func(t *testing.T) {
			dir := t.TempDir()
			write(t, Path(dir), "migrated by the new panel")
			write(t, Path(dir)+"-wal", "new wal")
			must(t, EnsureLayout(dir))
			cp := filepath.Join(Swap(dir), "stackr.db.pre-1")
			write(t, cp, "old main")
			write(t, cp+"-wal", "old wal-only commits")
			must(t, WriteJournal(dir, Journal{Phase: PhaseStartingNew, Direction: DirLayout, Copy: cp}))
			crashAt(t, step)
			if err := Recover(dir); err == nil {
				t.Fatal("no crash")
			}
			crashHook = nil
			if readf(cp) != "old main" || readf(cp+"-wal") != "old wal-only commits" {
				t.Fatal("the copy was consumed by a restore that did not finish")
			}
			must(t, Recover(dir))
			if readf(Legacy(dir)) != "old main" || readf(Legacy(dir)+"-wal") != "old wal-only commits" {
				t.Errorf("restored = %q %q", readf(Legacy(dir)), readf(Legacy(dir)+"-wal"))
			}
			if _, err := os.Stat(cp); err == nil {
				t.Error("copy left after the journal cleared")
			}
			if j, _ := ReadJournal(dir); j.Phase != PhaseNone {
				t.Errorf("journal = %q", j.Phase)
			}
		})
	}
}

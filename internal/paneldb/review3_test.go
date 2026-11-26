package paneldb

import (
	"errors"
	"path/filepath"
	"testing"
)

// A custom DATABASE_PATH on an installed box must exist too: the check is not
// only for the canonical path.
func TestCustomDatabasePathMustExistOnAnInstall(t *testing.T) {
	dir := t.TempDir()
	custom := filepath.Join(dir, "elsewhere", "my.db")
	if err := Check(dir, custom); err != nil {
		t.Fatalf("no install, dev: %v", err)
	}
	write(t, filepath.Join(dir, InstallFile), "{}")
	if err := Check(dir, custom); err == nil {
		t.Fatal("a missing custom database on an install passed")
	}
	write(t, custom, "db")
	if err := Check(dir, custom); err != nil {
		t.Errorf("an existing custom database: %v", err)
	}
	// the fresh phase is the one way to create one
	fresh := t.TempDir()
	write(t, filepath.Join(fresh, InstallFile), "{}")
	must(t, WriteJournal(fresh, Journal{Phase: PhaseFresh}))
	if err := Check(fresh, filepath.Join(fresh, "x.db")); err != nil {
		t.Errorf("fresh: %v", err)
	}
}

func TestRollbackBlockedIsATypedError(t *testing.T) {
	dir := t.TempDir()
	must(t, EnsureLayout(dir))
	write(t, Path(dir), "migrated")
	must(t, WriteJournal(dir, Journal{Phase: PhaseStartingNew, Direction: DirUpdate, Dst: Path(dir), Copy: filepath.Join(dir, "gone"), Ran: true}))
	if err := Recover(dir); !errors.Is(err, ErrCopyMissing) {
		t.Fatalf("Recover = %v", err)
	}
	if j, _ := ReadJournal(dir); !j.RollbackBlocked() || j.Phase != PhaseStartingNew {
		t.Errorf("journal changed: %+v", j)
	}
}

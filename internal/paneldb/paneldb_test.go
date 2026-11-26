package paneldb

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	must(t, os.MkdirAll(filepath.Dir(path), 0o700))
	must(t, os.WriteFile(path, []byte(body), 0o600))
}

// The boot check judges the journal, the mount test and the shape of the
// legacy path, and it never makes a file.
func TestCheckTable(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, dir string)
		db    func(dir string) string
		want  string // "" = allowed
	}{
		{"fresh data dir", func(*testing.T, string) {}, Path, ""},
		{"laid out", func(t *testing.T, dir string) { must(t, EnsureLayout(dir)); write(t, Path(dir), "db") }, Path, ""},
		{"layout in progress", func(t *testing.T, dir string) {
			must(t, WriteJournal(dir, Journal{Phase: PhaseLayout, Direction: DirLayout}))
		}, Path, `phase "layout"`},
		{"rolling back", func(t *testing.T, dir string) {
			must(t, WriteJournal(dir, Journal{Phase: PhaseRollingBack, Direction: DirLayout}))
		}, Path, `phase "rolling-back"`},
		{"a move in progress", func(t *testing.T, dir string) {
			must(t, WriteJournal(dir, Journal{Phase: "copied", Direction: "in"}))
		}, Path, `phase "copied"`},
		{"the new panel booting in the layout change", func(t *testing.T, dir string) {
			must(t, EnsureLayout(dir))
			write(t, Path(dir), "db")
			must(t, WriteJournal(dir, Journal{Phase: PhaseStartingNew, Direction: DirLayout}))
		}, Path, ""},
		{"moved, disk missing", func(t *testing.T, dir string) {
			must(t, EnsureLayout(dir))
			must(t, WriteJournal(dir, Journal{Phase: PhaseMoved, DiskID: "d1", InstallID: "i1"}))
		}, Path, "disk missing"},
		{"moved, starting-new on a move", func(t *testing.T, dir string) {
			must(t, EnsureLayout(dir))
			must(t, WriteJournal(dir, Journal{Phase: PhaseStartingNew, Direction: "in", DiskID: "d1"}))
		}, Path, "disk missing"},
		{"journal unreadable", func(t *testing.T, dir string) { write(t, filepath.Join(dir, JournalName), "{") }, Path, "journal cannot be read"},
		{"old file at the old path, new path asked", func(t *testing.T, dir string) { write(t, Legacy(dir), "db") }, Path, "still at"},
		{"old path asked, placeholder there", func(t *testing.T, dir string) { must(t, EnsureLayout(dir)) }, Legacy, "is a directory"},
		{"old path asked, file there (unchanged install, old binary)", func(t *testing.T, dir string) { write(t, Legacy(dir), "db") }, Legacy, ""},
		{"a dev path elsewhere", func(t *testing.T, dir string) { write(t, filepath.Join(dir, "x", "my.db"), "db") },
			func(dir string) string { return filepath.Join(dir, "x", "my.db") }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tc.setup(t, dir)
			before := tree(t, dir)
			err := Check(dir, tc.db(dir))
			if tc.want == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
			if after := tree(t, dir); after != before {
				t.Errorf("the check changed the data dir:\nbefore %s\nafter  %s", before, after)
			}
		})
	}
}

func tree(t *testing.T, dir string) string {
	t.Helper()
	var names []string
	must(t, filepath.WalkDir(dir, func(p string, _ os.DirEntry, err error) error {
		names = append(names, p)
		return err
	}))
	return strings.Join(names, "\n")
}

// The installer's check lets an unfinished layout change and an old install
// through (its recovery and the panel swap handle them) but not a move.
func TestCheckInstaller(t *testing.T) {
	dir := t.TempDir()
	write(t, Legacy(dir), "db")
	if err := CheckInstaller(dir); err != nil {
		t.Errorf("an old install: %v", err)
	}
	must(t, WriteJournal(dir, Journal{Phase: PhaseLayout, Direction: DirLayout}))
	if err := CheckInstaller(dir); err != nil {
		t.Errorf("a layout change in progress: %v", err)
	}
	must(t, WriteJournal(dir, Journal{Phase: "copied", Direction: "in"}))
	if err := CheckInstaller(dir); err == nil {
		t.Error("a move in progress was let through")
	}
}

// The old binary opens the old path, which is now a directory: SQLite cannot
// open it, so no empty database is made. (The panel's own test of this runs
// the real driver; here the file system fact.)
func TestPlaceholderIsADirectoryWithOneNote(t *testing.T) {
	dir := t.TempDir()
	must(t, EnsureLayout(dir))
	fi, err := os.Stat(Legacy(dir))
	if err != nil || !fi.IsDir() {
		t.Fatalf("placeholder: %v %v", fi, err)
	}
	if _, err := os.ReadFile(Legacy(dir)); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Errorf("reading the old path as a file: %v", err)
	}
	ents, _ := os.ReadDir(Legacy(dir))
	if len(ents) != 1 || ents[0].Name() != NoteName {
		t.Errorf("placeholder holds %v", ents)
	}
	if b, _ := os.ReadFile(filepath.Join(Legacy(dir), NoteName)); string(b) != Note {
		t.Errorf("note = %q", b)
	}
	if fi, _ := os.Stat(filepath.Join(dir, DirName)); fi.Mode().Perm() != 0o700 {
		t.Errorf("db/ mode = %v", fi.Mode().Perm())
	}
}

// Recover from the state a helper killed at each step of the layout change
// leaves: the files end up at the old path, whole, with the journal cleared.
func TestRecoverFromEveryStep(t *testing.T) {
	steps := map[string]func(t *testing.T, dir string){
		"journal written": func(t *testing.T, dir string) {},
		"db/ made": func(t *testing.T, dir string) {
			must(t, os.Mkdir(filepath.Join(dir, DirName), 0o700))
		},
		"wal moved": func(t *testing.T, dir string) {
			must(t, os.Mkdir(filepath.Join(dir, DirName), 0o700))
			must(t, os.Rename(Legacy(dir)+"-wal", Path(dir)+"-wal"))
		},
		"all moved": func(t *testing.T, dir string) {
			must(t, os.Mkdir(filepath.Join(dir, DirName), 0o700))
			must(t, os.Rename(Legacy(dir)+"-wal", Path(dir)+"-wal"))
			must(t, os.Rename(Legacy(dir), Path(dir)))
		},
		"placeholder made": func(t *testing.T, dir string) {
			must(t, os.Mkdir(filepath.Join(dir, DirName), 0o700))
			must(t, os.Rename(Legacy(dir)+"-wal", Path(dir)+"-wal"))
			must(t, os.Rename(Legacy(dir), Path(dir)))
			must(t, EnsureLayout(dir))
		},
		"rolling back, placeholder already gone": func(t *testing.T, dir string) {
			must(t, os.Mkdir(filepath.Join(dir, DirName), 0o700))
			must(t, os.Rename(Legacy(dir), Path(dir)))
			must(t, WriteJournal(dir, Journal{Phase: PhaseRollingBack, Direction: DirLayout}))
		},
	}
	for name, step := range steps {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, Legacy(dir), "db")
			write(t, Legacy(dir)+"-wal", "wal")
			must(t, WriteJournal(dir, Journal{Phase: PhaseLayout, Direction: DirLayout, Src: Legacy(dir), Dst: Path(dir)}))
			step(t, dir)
			must(t, Recover(dir))
			if b, _ := os.ReadFile(Legacy(dir)); string(b) != "db" {
				t.Errorf("main = %q", b)
			}
			if b, _ := os.ReadFile(Legacy(dir) + "-wal"); string(b) != "wal" {
				t.Errorf("wal = %q", b)
			}
			if _, err := os.Stat(filepath.Join(dir, DirName)); err == nil {
				t.Error("db/ left behind")
			}
			if j, _ := ReadJournal(dir); j.Phase != PhaseNone {
				t.Errorf("journal = %q", j.Phase)
			}
			// a second run changes nothing
			must(t, Recover(dir))
		})
	}
}

// Recovery of a layout that was applied and then rolled back twice, and the
// rollback refuses to guess when both copies of a file exist.
func TestRollbackRefusesWhenBothExist(t *testing.T) {
	dir := t.TempDir()
	write(t, Path(dir), "new place")
	write(t, Legacy(dir), "old place") // a regular file at the old path as well
	must(t, WriteJournal(dir, Journal{Phase: PhaseLayout, Direction: DirLayout}))
	if err := RollbackLayout(dir); err == nil {
		t.Fatal("rollback guessed between two database files")
	}
	if b, _ := os.ReadFile(Path(dir)); string(b) != "new place" {
		t.Error("a file was overwritten")
	}
}

// Fresh install: ApplyLayout has nothing to move, and makes the layout.
func TestApplyLayoutFresh(t *testing.T) {
	dir := t.TempDir()
	applied, err := ApplyLayout(dir, "", "")
	if err != nil || applied {
		t.Fatalf("applied=%v err=%v", applied, err)
	}
	if err := Check(dir, Path(dir)); err != nil {
		t.Error(err)
	}
}

// The copy: the main file lands last, so a main file at the copy's name is whole.
func TestSaveCopyAndRestore(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "stackr.db")
	write(t, db, "main")
	write(t, db+"-wal", "wal")
	cp := filepath.Join(dir, "swap", "stackr.db.pre-1")
	must(t, SaveCopy(db, cp))
	write(t, db, "migrated")
	write(t, db+"-wal", "new wal")
	write(t, db+"-shm", "shm")
	must(t, RestoreCopy(db, cp))
	for suf, want := range map[string]string{"": "main", "-wal": "wal", "-shm": "<gone>"} {
		got := "<gone>"
		if b, err := os.ReadFile(db + suf); err == nil {
			got = string(b)
		}
		if got != want {
			t.Errorf("%s = %q, want %q", suf, got, want)
		}
	}
	if m, _ := filepath.Glob(cp + "*"); len(m) != 0 {
		t.Errorf("copy left: %v", m)
	}
}

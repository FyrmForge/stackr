package paneldb

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// Files that make one SQLite database.
var suffixes = []string{"", "-wal", "-shm"}

// crashHook lets a test stop the code between two steps, as a dying process
// would. Nil outside tests.
var crashHook func(step string) error

func crash(step string) error {
	if crashHook != nil {
		return crashHook(step)
	}
	return nil
}

// EnsureLayout makes db/ (0700) and the legacy placeholder directory with its
// note. It never touches a database file: a legacy path that is a regular file
// is an unmoved install and is an error here (ApplyLayout moves it).
func EnsureLayout(dataDir string) error {
	if err := os.MkdirAll(filepath.Join(dataDir, DirName), 0o700); err != nil {
		return err
	}
	legacy := Legacy(dataDir)
	fi, err := os.Lstat(legacy)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.Mkdir(legacy, 0o755); err != nil {
			return err
		}
	case err != nil:
		return err
	case !fi.IsDir():
		return fmt.Errorf("%s is a database file, not the placeholder directory", legacy)
	}
	note := filepath.Join(legacy, NoteName)
	if _, err := os.Stat(note); errors.Is(err, os.ErrNotExist) {
		return os.WriteFile(note, []byte(Note), 0o644)
	}
	return nil
}

// EnsureFresh makes the layout of a new install. The fresh phase, the one
// state in which the panel may create its first database, is written first
// (before anything is made, so a crash leaves a state a rerun accepts) and only
// for a box that has no install.json, no database, no placeholder and no
// journal. Anything else gets its layout ensured and no fresh phase: a missing
// database there is a loss, not a start.
func EnsureFresh(dataDir string) error {
	_, ierr := os.Stat(filepath.Join(dataDir, InstallFile))
	_, lerr := os.Lstat(Legacy(dataDir))
	_, derr := os.Stat(Path(dataDir))
	j, err := ReadJournal(dataDir)
	if err != nil {
		return err
	}
	if ierr != nil && lerr != nil && derr != nil && j.Phase == PhaseNone {
		if err := os.MkdirAll(dataDir, 0o700); err != nil {
			return err
		}
		if err := WriteJournal(dataDir, Journal{Phase: PhaseFresh}); err != nil {
			return err
		}
	}
	return EnsureLayout(dataDir)
}

// ApplyLayout is the layout change: journal first, then the files of an
// unmoved install go into db/ by rename and the placeholder takes their old
// place. cp is the pre-upgrade copy the caller makes next (recorded so a
// recovery can find it), container the new panel's name. False means there was
// nothing to move (a fresh or already laid-out install; the placeholder is
// ensured either way). A db/stackr.db that exists next to a legacy regular
// file is the newer file of an old restore: its set (main, -wal, -shm) moves
// to swap/ under a name the journal records first, the legacy file is adopted,
// nothing is merged.
func ApplyLayout(dataDir, cp, container string) (bool, error) {
	j, err := ReadJournal(dataDir)
	if err != nil {
		return false, err
	}
	switch j.Phase {
	case PhaseNone, PhaseFresh:
	case PhaseMoved:
		return false, errors.New("the database is on a node disk (move.journal says moved): refusing to change the layout")
	default:
		return false, fmt.Errorf("move.journal is in phase %q: resolve it first", j.Phase)
	}
	legacy, db := Legacy(dataDir), Path(dataDir)
	fi, err := os.Lstat(legacy)
	if errors.Is(err, os.ErrNotExist) || (err == nil && fi.IsDir()) {
		return false, EnsureLayout(dataDir)
	}
	if err != nil {
		return false, err
	}
	if !fi.Mode().IsRegular() {
		return false, fmt.Errorf("%s is neither a database file nor a directory", legacy)
	}
	var aside string
	for _, suf := range suffixes {
		if _, err := os.Lstat(db + suf); err == nil {
			aside = "db.superseded-" + time.Now().UTC().Format(Stamp)
		}
	}
	j = Journal{Phase: PhaseLayout, Direction: DirLayout, Src: legacy, Dst: db, Copy: cp, Container: container, Aside: aside}
	if err := WriteJournal(dataDir, j); err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(db), 0o700); err != nil {
		return false, err
	}
	if aside != "" {
		slog.Warn("database: a file in db/ and one at the old path (an old restore ran after the layout change): the old path's file is adopted, the one in db/ is kept", "kept", filepath.Join(Swap(dataDir), aside))
		if err := finishAside(dataDir, j); err != nil {
			return false, err
		}
		j.AsideDone = true
		if err := WriteJournal(dataDir, j); err != nil {
			return false, err
		}
	}
	if err := crash("aside-done"); err != nil {
		return false, err
	}
	for _, suf := range suffixes {
		if err := os.Rename(legacy+suf, db+suf); err != nil && !(suf != "" && errors.Is(err, os.ErrNotExist)) {
			return false, err
		}
		if err := crash("moved" + suf); err != nil {
			return false, err
		}
	}
	if err := SyncDir(filepath.Dir(db)); err != nil {
		return false, err
	}
	if err := os.Mkdir(legacy, 0o755); err != nil {
		return false, err
	}
	if err := EnsureLayout(dataDir); err != nil {
		return false, err
	}
	return true, SyncDir(dataDir)
}

// finishAside moves the set the journal names (whatever of main, -wal and -shm
// is still in db/) to swap/<aside>, file by file, so it can be repeated after a
// crash. It touches only db/, only before the old path's files have moved in.
func finishAside(dataDir string, j Journal) error {
	db := Path(dataDir)
	if err := os.MkdirAll(Swap(dataDir), 0o700); err != nil {
		return err
	}
	dst := filepath.Join(Swap(dataDir), j.Aside)
	for _, suf := range suffixes {
		if _, err := os.Lstat(db + suf); err != nil {
			continue
		}
		if _, err := os.Lstat(dst + suf); err == nil {
			return fmt.Errorf("both %s and %s exist: not guessing which is newer", db+suf, dst+suf)
		}
		if err := os.Rename(db+suf, dst+suf); err != nil {
			return err
		}
		if err := crash("aside" + suf); err != nil {
			return err
		}
	}
	return errors.Join(SyncDir(filepath.Dir(db)), SyncDir(Swap(dataDir)))
}

// RollbackLayout undoes ApplyLayout, every step safe to repeat. A superseded
// set that was only partly moved is finished first (those files are the old
// restore's, not the legacy file's, and never go onto the legacy main). Then
// the placeholder goes, the files go back to the legacy path, an empty db/
// goes. The journal is left at rolling-back; the caller restores the
// pre-upgrade copy and then clears it, so a crash in between resumes the same
// rollback. On error nothing may start an old panel: its path may hold no
// database.
func RollbackLayout(dataDir string) error {
	j, err := ReadJournal(dataDir)
	if err != nil {
		return err
	}
	j.Phase, j.Direction = PhaseRollingBack, DirLayout
	if err := WriteJournal(dataDir, j); err != nil {
		return err
	}
	if j.Aside != "" && !j.AsideDone {
		if err := finishAside(dataDir, j); err != nil {
			return err
		}
		j.AsideDone = true
		if err := WriteJournal(dataDir, j); err != nil {
			return err
		}
	}
	legacy, db := Legacy(dataDir), Path(dataDir)
	if fi, err := os.Lstat(legacy); err == nil && fi.IsDir() {
		if err := os.Remove(filepath.Join(legacy, NoteName)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.Remove(legacy); err != nil {
			return fmt.Errorf("the placeholder %s holds more than its note: %w", legacy, err)
		}
	}
	for _, suf := range suffixes {
		if _, err := os.Lstat(db + suf); err != nil {
			continue
		}
		if _, err := os.Lstat(legacy + suf); err == nil {
			return fmt.Errorf("both %s and %s exist: not guessing which is newer", legacy+suf, db+suf)
		}
		if err := os.Rename(db+suf, legacy+suf); err != nil {
			return err
		}
	}
	_ = os.Remove(filepath.Dir(db)) // only when empty
	return SyncDir(dataDir)
}

// Recover finishes a swap that a helper left unfinished: a layout change goes
// back, and the pre-upgrade copy the journal names, when there is one, is put
// over the database. Once the new panel has run (Ran, or starting-new and
// gating) the copy is required: without it nothing is rolled back and the
// journal is not cleared. The copy is deleted only after the journal is
// cleared. The caller has stopped the new panel. Nothing to do for any other
// journal state (a move is another phase's business).
func Recover(dataDir string) error {
	j, err := ReadJournal(dataDir)
	if err != nil {
		return err
	}
	if (j.Direction != DirLayout && j.Direction != DirUpdate) || j.Phase == PhaseNone {
		return nil
	}
	if j.Ran || j.Phase == PhaseStartingNew || j.Phase == PhaseGating {
		if j.Copy == "" {
			return errors.New("the new panel ran and the journal names no pre-upgrade copy: not rolling back")
		}
		if _, err := os.Stat(j.Copy); err != nil {
			return fmt.Errorf("the new panel ran and its pre-upgrade copy is missing (%v): not rolling back, the journal stays", err)
		}
	}
	target := j.Dst
	if j.Direction == DirLayout {
		if err := RollbackLayout(dataDir); err != nil {
			return err
		}
		target = Legacy(dataDir)
	} else {
		j.Phase = PhaseRollingBack
		if err := WriteJournal(dataDir, j); err != nil {
			return err
		}
	}
	if j.Copy != "" && target != "" {
		if _, err := os.Stat(j.Copy); err == nil {
			if err := RestoreCopy(target, j.Copy); err != nil {
				return err
			}
		}
	}
	if err := ClearJournal(dataDir); err != nil {
		return err
	}
	if j.Copy != "" {
		DropCopy(j.Copy)
	}
	return nil
}

// AsideCopy moves a copy no journal names (an earlier attempt's) to
// swap/orphan-<timestamp>-<name>, so it can never be restored over newer data.
func AsideCopy(dataDir, cp string) error {
	dst := filepath.Join(Swap(dataDir), "orphan-"+time.Now().UTC().Format(Stamp)+"-"+filepath.Base(cp))
	for _, suf := range suffixes {
		for _, tmp := range []string{"", ".tmp"} {
			if err := os.Rename(cp+suf+tmp, dst+suf+tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return SyncDir(Swap(dataDir))
}

// SaveCopy copies the database files to cp (0600, synced). Each file lands
// under a temp name first and the main file last, so a main file at cp means
// the copy is whole.
func SaveCopy(db, cp string) error {
	if err := os.MkdirAll(filepath.Dir(cp), 0o700); err != nil {
		return err
	}
	for i := len(suffixes) - 1; i >= 0; i-- {
		suf := suffixes[i]
		err := copyFile(db+suf, cp+suf+".tmp")
		if err == nil {
			err = os.Rename(cp+suf+".tmp", cp+suf)
		}
		if err != nil && (suf == "" || !errors.Is(err, os.ErrNotExist)) {
			DropCopy(cp)
			return err
		}
	}
	return SyncDir(filepath.Dir(cp))
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err == nil {
		err = out.Sync()
	}
	return errors.Join(err, out.Close())
}

// DropCopy removes the copy and any temp file of it.
func DropCopy(cp string) {
	for _, suf := range suffixes {
		_ = os.Remove(cp + suf)
		_ = os.Remove(cp + suf + ".tmp")
	}
}

// RestoreCopy puts the copy over db and leaves the copy where it is: every
// file is copied to a temp name and renamed, so a crash at any point leaves
// the copy whole and the same call can run again. The live -wal/-shm go first,
// so none is replayed onto the old file. The caller deletes the copy (DropCopy)
// only after its own record of the rollback is durable.
func RestoreCopy(db, cp string) error {
	if _, err := os.Stat(cp); err != nil {
		return err
	}
	for _, suf := range suffixes[1:] {
		_ = os.Remove(db + suf)
	}
	for _, suf := range suffixes {
		err := copyFile(cp+suf, db+suf+".tmp")
		if errors.Is(err, os.ErrNotExist) && suf != "" {
			continue
		}
		if err == nil {
			err = os.Rename(db+suf+".tmp", db+suf)
		}
		if err != nil {
			return err
		}
		if err := crash("restored" + suf); err != nil {
			return err
		}
	}
	return SyncDir(filepath.Dir(db))
}

// AdoptDev moves a dev checkout's or plain binary's database into db/ at
// boot, the layout change an installed panel gets from its swap helper. Only
// for a data dir that has no journal and an empty or absent db/; the caller
// has judged there is no install. False means there was nothing to do.
func AdoptDev(dataDir string) (bool, error) {
	// A layout change this function itself began (no container, no copy) and
	// did not finish is undone first, then tried again.
	if j, err := ReadJournal(dataDir); err == nil && j.Direction == DirLayout && j.Container == "" && j.Copy == "" &&
		(j.Phase == PhaseLayout || j.Phase == PhaseRollingBack) {
		if err := Recover(dataDir); err != nil {
			return false, err
		}
	} else if _, err := os.Stat(filepath.Join(dataDir, JournalName)); err == nil {
		return false, nil
	}
	if fi, err := os.Lstat(Legacy(dataDir)); err != nil || !fi.Mode().IsRegular() {
		return false, nil
	}
	if ents, err := os.ReadDir(filepath.Join(dataDir, DirName)); err == nil && len(ents) > 0 {
		return false, nil
	}
	slog.Info("database: moving the old-path database into db/ (no install here)", "from", Legacy(dataDir), "to", Path(dataDir))
	if _, err := ApplyLayout(dataDir, "", ""); err != nil {
		return false, errors.Join(err, Recover(dataDir))
	}
	return true, ClearJournal(dataDir)
}

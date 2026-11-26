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

// ApplyLayout is the layout change: journal first, then the files of an
// unmoved install go into db/ by rename and the placeholder takes their old
// place. cp is the pre-upgrade copy the caller makes next (recorded so a
// recovery can find it), container the new panel's name. False means there was
// nothing to move (a fresh or already laid-out install; the placeholder is
// ensured either way). A db/stackr.db that exists next to a legacy regular
// file is the newer file of an old restore: it moves to swap/ untouched, the
// legacy file is adopted, nothing is merged.
func ApplyLayout(dataDir, cp, container string) (bool, error) {
	j, err := ReadJournal(dataDir)
	if err != nil {
		return false, err
	}
	switch j.Phase {
	case PhaseNone:
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
	if err := WriteJournal(dataDir, Journal{Phase: PhaseLayout, Direction: DirLayout, Src: legacy, Dst: db, Copy: cp, Container: container}); err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(db), 0o700); err != nil {
		return false, err
	}
	if err := supersede(dataDir, db); err != nil {
		return false, err
	}
	for _, suf := range suffixes {
		if err := os.Rename(legacy+suf, db+suf); err != nil && !(suf != "" && errors.Is(err, os.ErrNotExist)) {
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

// supersede moves whatever sits at db (a file left by an old restore after the
// layout change) to swap/db.superseded-<timestamp>, with its -wal and -shm.
func supersede(dataDir, db string) error {
	var found bool
	for _, suf := range suffixes {
		if _, err := os.Lstat(db + suf); err == nil {
			found = true
		}
	}
	if !found {
		return nil
	}
	if err := os.MkdirAll(Swap(dataDir), 0o700); err != nil {
		return err
	}
	dst := filepath.Join(Swap(dataDir), "db.superseded-"+time.Now().UTC().Format(Stamp))
	slog.Warn("database: a file in db/ and one at the old path (an old restore ran after the layout change): the old path's file is adopted, the one in db/ is kept", "kept", dst)
	for _, suf := range suffixes {
		if err := os.Rename(db+suf, dst+suf); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// RollbackLayout undoes ApplyLayout, every step safe to repeat: the placeholder
// goes, the files go back to the legacy path, an empty db/ goes. The journal
// is left at rolling-back; the caller restores the pre-upgrade copy and then
// clears it, so a crash in between resumes the same rollback. On error nothing
// may start an old panel: its path may hold no database.
func RollbackLayout(dataDir string) error {
	j, err := ReadJournal(dataDir)
	if err != nil {
		return err
	}
	j.Phase, j.Direction = PhaseRollingBack, DirLayout
	if err := WriteJournal(dataDir, j); err != nil {
		return err
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

// Recover finishes a layout change that a helper left unfinished: the layout
// goes back and the pre-upgrade copy, when one was made, is put over the
// database. The caller has stopped the new panel. Nothing to do for any other
// journal state (a move is another phase's business).
func Recover(dataDir string) error {
	j, err := ReadJournal(dataDir)
	if err != nil {
		return err
	}
	if j.Direction != DirLayout || j.Phase == PhaseNone {
		return nil
	}
	if err := RollbackLayout(dataDir); err != nil {
		return err
	}
	if j.Copy != "" {
		if _, err := os.Stat(j.Copy); err == nil {
			if err := RestoreCopy(Legacy(dataDir), j.Copy); err != nil {
				return err
			}
		}
	}
	return ClearJournal(dataDir)
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

// RestoreCopy puts the copy back over db: the live -wal/-shm go first, so
// none is replayed onto the old file.
func RestoreCopy(db, cp string) error {
	for _, suf := range suffixes[1:] {
		_ = os.Remove(db + suf)
	}
	for _, suf := range suffixes {
		if err := os.Rename(cp+suf, db+suf); err != nil && (suf == "" || !errors.Is(err, os.ErrNotExist)) {
			return err
		}
	}
	return nil
}

// AdoptDev moves a dev checkout's or plain binary's database into db/ at
// boot, the layout change an installed panel gets from its swap helper. Only
// for a data dir that has no journal and an empty or absent db/; the caller
// has judged there is no install. False means there was nothing to do.
func AdoptDev(dataDir string) (bool, error) {
	if _, err := os.Stat(filepath.Join(dataDir, JournalName)); err == nil {
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

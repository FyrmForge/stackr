// Package paneldb is where the panel's database lives and the one durable
// state file about it. The database is always <DataDir>/db/stackr.db. The old
// path <DataDir>/stackr.db is a directory holding one note, so an older binary
// that still opens the old path fails (SQLite cannot open a directory) and
// never seeds an empty database. Swap leftovers live in <DataDir>/swap.
//
// Stdlib only: the panel, its helper and stackr-install all import it.
package paneldb

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const (
	DirName     = "db"
	FileName    = "stackr.db"
	SwapName    = "swap"
	JournalName = "move.journal"
	MarkerName  = ".stackr-panel-data"
	NoteName    = "MOVED"
	// Note is what the placeholder directory's one file says.
	Note = "The panel database now lives in db/stackr.db. Do not create a file here.\n"
)

// Path is the database file under dataDir.
func Path(dataDir string) string { return filepath.Join(dataDir, DirName, FileName) }

// Legacy is the old database path, a placeholder directory since the layout change.
func Legacy(dataDir string) string { return filepath.Join(dataDir, FileName) }

// Swap is the folder for swap copies, the failure file and move leftovers.
func Swap(dataDir string) string { return filepath.Join(dataDir, SwapName) }

// DataDirOf is the data dir a database path belongs to.
func DataDirOf(db string) string {
	d := filepath.Dir(db)
	if filepath.Base(d) == DirName {
		return filepath.Dir(d)
	}
	return d
}

// SwapDirOf is the swap folder for a database path.
func SwapDirOf(db string) string { return Swap(DataDirOf(db)) }

// Phases of move.journal. The layout change uses layout, starting-new and
// rolling-back; the move onto a node disk adds the rest.
const (
	PhaseNone        = "none"
	PhaseLayout      = "layout"
	PhaseRollingBack = "rolling-back"
	PhaseStartingNew = "starting-new"
	PhaseGating      = "gating"
	PhaseMoved       = "moved"
)

// DirLayout is the journal direction of the layout change.
const DirLayout = "layout"

// Journal is <DataDir>/move.journal: the one durable state file for "where is
// the database". Ids and hashes only, no secrets.
type Journal struct {
	Phase     string `json:"phase"`
	Direction string `json:"direction,omitempty"`
	Src       string `json:"src,omitempty"`
	Dst       string `json:"dst,omitempty"`
	Copy      string `json:"copy,omitempty"` // the pre-upgrade copy of the main file
	SHA256    string `json:"sha256,omitempty"`
	Container string `json:"container,omitempty"`
	DiskID    string `json:"disk_id,omitempty"`
	InstallID string `json:"install_id,omitempty"`
	MoveID    string `json:"move_id,omitempty"`
	Time      string `json:"time,omitempty"`
}

// ReadJournal reads the journal; none on disk means phase none.
func ReadJournal(dataDir string) (Journal, error) {
	b, err := os.ReadFile(filepath.Join(dataDir, JournalName))
	if errors.Is(err, os.ErrNotExist) {
		return Journal{Phase: PhaseNone}, nil
	}
	if err != nil {
		return Journal{}, err
	}
	var j Journal
	if err := json.Unmarshal(b, &j); err != nil {
		return Journal{}, fmt.Errorf("%s is unreadable: %w", JournalName, err)
	}
	if j.Phase == "" {
		j.Phase = PhaseNone
	}
	return j, nil
}

// Stamp is the full UTC timestamp used in leftover names (DD-MM is for the UI).
const Stamp = "20060102T150405Z"

// WriteJournal writes a temp file, fsyncs, renames and fsyncs the directory.
func WriteJournal(dataDir string, j Journal) error {
	j.Time = time.Now().UTC().Format(Stamp)
	b, err := json.Marshal(j)
	if err != nil {
		return err
	}
	path := filepath.Join(dataDir, JournalName)
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if err = errors.Join(err, f.Close()); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return SyncDir(dataDir)
}

// ClearJournal records phase none. The file is never deleted.
func ClearJournal(dataDir string) error { return WriteJournal(dataDir, Journal{Phase: PhaseNone}) }

// SyncDir fsyncs a directory.
func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}

// Check is checkPanelDB: the one refusal every entry point that touches the
// database runs first, before any key or database file is made. dbPath is the
// effective path after the fallbacks. It reads the journal, the mount test
// and the shape of the legacy path, nothing else.
func Check(dataDir, dbPath string) error { return check(dataDir, dbPath, false) }

// CheckInstaller is Check for stackr-install, which is also the way out of an
// unfinished layout change (the recovery it runs resolves it) and of an old
// install the layout change has not reached yet.
func CheckInstaller(dataDir string) error { return check(dataDir, Path(dataDir), true) }

func check(dataDir, dbPath string, installer bool) error {
	if dataDir == "" {
		return nil
	}
	dbPath = filepath.Clean(dbPath)
	j, err := ReadJournal(dataDir)
	if err != nil {
		return refuse("the move journal cannot be read (%v). Not starting, so no empty database is made", err)
	}
	switch j.Phase {
	case PhaseNone:
	case PhaseMoved:
		if err := checkMounted(dataDir, j); err != nil {
			return err
		}
	case PhaseStartingNew, PhaseGating:
		// The new panel is the one booting here: it must not refuse itself.
		if j.Direction != DirLayout {
			if err := checkMounted(dataDir, j); err != nil {
				return err
			}
		}
	default:
		if installer && j.Direction == DirLayout {
			return nil // the recovery the installer runs resolves it
		}
		return refuse("the database is in phase %q (%s). Not starting, so no empty database is made. Run stackr-install on this node. It reads the journal and finishes or rolls back", j.Phase, j.Direction)
	}
	if installer {
		return nil
	}
	if fi, err := os.Stat(dbPath); err == nil && fi.IsDir() {
		return refuse("%s is a directory: the database lives in %s. Not starting, so no empty database is made. Start the panel from its current spec (stackr-install, or Update)", dbPath, Path(dataDir))
	}
	if dbPath == Path(dataDir) {
		if fi, err := os.Lstat(Legacy(dataDir)); err == nil && fi.Mode().IsRegular() {
			return refuse("the database is still at %s and this build looks in %s. Not starting, so no empty database is made. Run stackr-install, or Update from the panel, to move it. On a dev checkout: mkdir -p %s && mv %s* %s/", Legacy(dataDir), dbPath, filepath.Join(dataDir, DirName), Legacy(dataDir), filepath.Join(dataDir, DirName))
		}
	}
	return nil
}

func refuse(format string, a ...any) error {
	return fmt.Errorf("panel database: "+format, a...)
}

// checkMounted is the "moved" judgement: db/ must be a mount point (another
// device than its parent) holding the marker of this install, 0700 root.
// The immutable-flag half of the RFC's test arrives with the move itself.
func checkMounted(dataDir string, j Journal) error {
	dir := filepath.Join(dataDir, DirName)
	missing := refuse("disk missing: the database is on node disk %s and it is not mounted. Not starting, so no empty database is made. Attach the disk, then run stackr-install disk-check", j.DiskID)
	a, err := os.Stat(dir)
	if err != nil {
		return missing
	}
	p, err := os.Stat(dataDir)
	if err != nil {
		return missing
	}
	as, ok1 := a.Sys().(*syscall.Stat_t)
	ps, ok2 := p.Sys().(*syscall.Stat_t)
	if !ok1 || !ok2 || as.Dev == ps.Dev {
		return missing
	}
	if a.Mode().Perm() != 0o700 || as.Uid != 0 {
		return refuse("%s is not 0700 root. Not starting", dir)
	}
	b, err := os.ReadFile(filepath.Join(dir, MarkerName))
	if err != nil {
		return refuse("the disk at %s has no panel database marker. Not starting", dir)
	}
	var m struct {
		InstallID string `json:"install_id"`
	}
	if json.Unmarshal(b, &m) != nil || m.InstallID != j.InstallID {
		return refuse("the disk at %s belongs to another install. Not starting", dir)
	}
	return nil
}

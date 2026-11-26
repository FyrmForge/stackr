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
	// PhasePassed: the new panel passed its gate. From here a move only goes
	// forward (the new panel has taken writes and the old files are going away):
	// recovery finishes it, it never rolls back.
	PhasePassed = "passed"
	PhaseMoved  = "moved"
	// PhaseFresh is an explicit empty start: the installer made the layout of a
	// new install and the panel may create its first database. The panel clears
	// it once it has. Absence of a database is never read as "fresh" otherwise.
	PhaseFresh = "fresh"
	// The move onto a node disk (direction move-in) and back (move-back). Each
	// external change is bracketed: an intent phase before it, a completion
	// phase after (mounting before the files leave db/, unmounting before the
	// unit goes).
	PhasePrepared   = "prepared"   // folder placed, panel running or just stopped, nothing changed
	PhaseCopied     = "copied"     // the copy is verified (move-in: and marked)
	PhaseMounting   = "mounting"   // intent: db/ is being emptied, locked, mounted
	PhaseMounted    = "mounted"    // the mount is live and its marker read
	PhaseUnmounting = "unmounting" // move-back intent: the unit is going
	PhaseUnmounted  = "unmounted"  // move-back: db/ is the bare underlay
	PhasePlaced     = "placed"     // move-back: the copy is in db/stackr.db, no panel yet
	// PhaseAbandoning: stackr-install restore --abandon-disk is taking a dead
	// disk's bind unit out so the restore can write db/ on the system disk.
	PhaseAbandoning = "abandoning"
	// PhaseRestoring is a restore that has begun: the live database is being set
	// aside and the archive's put in its place.
	PhaseRestoring = "restoring"
)

// DirRestore is the journal direction of a restore.
const DirRestore = "restore"

// DirUpdate is the journal direction of an ordinary panel swap: it records the
// attempt's pre-upgrade copy, the only one a recovery may restore.
const DirUpdate = "update"

// InstallFile is the installer's answers file (installspec.File): its presence
// is what makes a box an install, and an install never starts a fresh database.
const InstallFile = "install.json"

// DirIn and DirBack are the journal directions of the database move onto a
// node disk and back to the system disk.
const (
	DirIn   = "in"
	DirBack = "back"
)

// DirAbandon is the journal direction of giving up a dead node disk.
const DirAbandon = "abandon"

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
	// Folder is the disk folder that holds the database (host path), OldCopy the
	// base name the system-disk copy is kept under once a move-in has passed.
	Folder  string `json:"folder,omitempty"`
	OldCopy string `json:"old_copy,omitempty"`
	// Moved: an ordinary Update is running on an install whose database is on a
	// node disk. The journal says so until the swap ends, and Finish writes the
	// moved journal back.
	Moved bool `json:"moved,omitempty"`
	// Aside is where a database found in db/ next to an old-path file is kept
	// (swap/db.superseded-<ts>, a base name); AsideDone says every file of that
	// set has moved. Recovery finishes exactly that set before anything else.
	// For a restore, Aside is the base name the replaced database is kept under.
	Aside     string `json:"aside,omitempty"`
	AsideDone bool   `json:"aside_done,omitempty"`
	// Ran: the new panel has been started on the database, so a recovery that
	// must roll back needs the pre-upgrade copy and refuses without it.
	Ran bool `json:"ran,omitempty"`
	// Image is the new panel's image (so an installer rerun can start it again),
	// Predecessors the exact containers (ids) that ran before it: the only ones
	// a rollback may start.
	Image        string   `json:"image,omitempty"`
	Predecessors []string `json:"predecessors,omitempty"`
	Time         string   `json:"time,omitempty"`
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

// MovedBase is the journal of an install whose database is on a node disk.
func (j Journal) MovedBase() Journal {
	return Journal{Phase: PhaseMoved, Direction: DirIn, DiskID: j.DiskID, InstallID: j.InstallID, Folder: j.Folder, MoveID: j.MoveID, OldCopy: j.OldCopy}
}

// Finish ends a swap's journal: an install whose database is on a disk goes
// back to moved, any other to none. The moved state is never lost to an Update.
func Finish(dataDir string, j Journal) error {
	if j.Moved || j.Phase == PhaseMoved {
		return WriteJournal(dataDir, j.MovedBase())
	}
	return ClearJournal(dataDir)
}

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
func Check(dataDir, dbPath string) error { return check(dataDir, dbPath, modeBoot) }

// CheckInstaller is Check for stackr-install, which is also the way out of an
// unfinished layout change (the recovery it runs resolves it) and of an old
// install the layout change has not reached yet.
func CheckInstaller(dataDir string) error { return check(dataDir, Path(dataDir), modeInstaller) }

// CheckRestore is Check for `stackr-install restore`: the restore is the way
// out of an interrupted restore and of a missing database.
func CheckRestore(dataDir string) error { return check(dataDir, Path(dataDir), modeRestore) }

type mode int

const (
	modeBoot mode = iota
	modeInstaller
	modeRestore
)

// Seeded is called once the panel has opened its database: a fresh start is
// over, so a database that goes missing later is refused, not recreated.
func Seeded(dataDir string) error {
	j, err := ReadJournal(dataDir)
	if err != nil || j.Phase != PhaseFresh {
		return err
	}
	return ClearJournal(dataDir)
}

func check(dataDir, dbPath string, m mode) error {
	if dataDir == "" {
		return nil
	}
	dbPath = filepath.Clean(dbPath)
	j, err := ReadJournal(dataDir)
	if err != nil {
		return refuse("the move journal cannot be read (%v). Not starting, so no empty database is made", err)
	}
	switch j.Phase {
	case PhaseNone, PhaseFresh:
	case PhaseMoved:
		if err := checkMounted(dataDir, j); err != nil {
			return err
		}
	case PhaseStartingNew, PhaseGating, PhasePassed:
		// The new panel is the one booting here: it must not refuse itself. A
		// move-back's panel runs on the system disk, so nothing is mounted.
		// The installer skips the mount check of a move-in: with the disk gone it is
		// the way out (its helper rolls back from swap/ or finishes the pass, neither
		// needs the disk).
		if (j.Direction == DirIn && m != modeInstaller) || j.Moved {
			if err := checkMounted(dataDir, j); err != nil {
				return err
			}
		}
	default:
		if m != modeBoot && (j.Direction == DirLayout || j.Direction == DirUpdate || j.Direction == DirIn || j.Direction == DirBack) {
			return nil // the recovery the installer runs resolves it
		}
		if m == modeRestore && j.Direction == DirRestore {
			return nil // running the restore again is the way out
		}
		if j.Direction == DirAbandon {
			if m == modeRestore {
				return nil // restore --abandon-disk again finishes it
			}
			return refuse("giving up the node disk was interrupted (phase %q). Not starting. Run stackr-install restore --abandon-disk <archive> to finish it", j.Phase)
		}
		if j.Direction == DirRestore {
			return refuse("a restore was interrupted (phase %q). Not starting, so no empty database is made. Run stackr-install restore <archive> again", j.Phase)
		}
		return refuse("the database is in phase %q (%s). Not starting, so no empty database is made. Run stackr-install on this node. It reads the journal and finishes or rolls back", j.Phase, j.Direction)
	}
	if m == modeRestore {
		return nil
	}
	// The database file must exist, on an install (install.json, whatever the
	// path is configured to) and wherever the canonical layout is established
	// (the placeholder is there). Only the explicit fresh phase, written by the
	// installer of a new install, may create one. An old-layout file still at
	// the old path is the swap's to move.
	if j.Phase != PhaseFresh {
		if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
			lfi, lerr := os.Lstat(Legacy(dataDir))
			_, ierr := os.Stat(filepath.Join(dataDir, InstallFile))
			canonical := dbPath == Path(dataDir)
			legacyFile := lerr == nil && lfi.Mode().IsRegular()
			if (ierr == nil || (canonical && lerr == nil && lfi.IsDir())) && !(canonical && legacyFile) {
				return refuse("%s is missing. Not starting, so no empty database is made. Restore a backup: stackr-install restore <archive>", dbPath)
			}
		}
	}
	if m == modeInstaller {
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
// IsMounted: db/ is a mount point of another device than the data dir.
func IsMounted(dataDir string) bool {
	a, err := stat(filepath.Join(dataDir, DirName))
	if err != nil {
		return false
	}
	p, err := stat(dataDir)
	if err != nil {
		return false
	}
	as, ok1 := a.Sys().(*syscall.Stat_t)
	ps, ok2 := p.Sys().(*syscall.Stat_t)
	return ok1 && ok2 && as.Dev != ps.Dev
}

// stat is os.Stat; tests swap it to give db/ another device.
var stat = os.Stat

func checkMounted(dataDir string, j Journal) error {
	dir := filepath.Join(dataDir, DirName)
	missing := refuse("disk missing: the database is on node disk %s and it is not mounted. Not starting, so no empty database is made. Attach the disk, then run stackr-install disk-check", j.DiskID)
	a, err := stat(dir)
	if err != nil {
		return missing
	}
	p, err := stat(dataDir)
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
		DiskID    string `json:"disk_id"`
	}
	if json.Unmarshal(b, &m) != nil || m.InstallID != j.InstallID || (j.DiskID != "" && m.DiskID != j.DiskID) {
		return refuse("the disk at %s belongs to another install. Not starting", dir)
	}
	return nil
}

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/FyrmForge/stackr/internal/hostdisk"
	"github.com/FyrmForge/stackr/internal/paneldb"
)

// `stackr-install restore --abandon-disk <archive>`: the panel database is on a
// node disk that is gone or failing, so the panel cannot start and the
// database cannot move back. This takes the database's bind unit out (the
// disk's own xfs unit file stays), lifts the immutable flag on the empty
// folder under it, writes the journal back to the unmoved layout, and the
// normal restore then writes db/ on the system disk. Each step can be repeated:
// the journal says "abandoning" until the last one is done.

var (
	// seams: tests need no systemd, chattr or mounts
	abandonSystemctl = func(ctx context.Context, args ...string) error {
		out, err := exec.CommandContext(ctx, "systemctl", args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("systemctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	abandonUnmount = func(ctx context.Context, dir string) error {
		return exec.CommandContext(ctx, "umount", "-l", dir).Run()
	}
	abandonUnlock = func(ctx context.Context, dir string) error {
		out, err := exec.CommandContext(ctx, "chattr", "-i", dir).CombinedOutput()
		if err != nil {
			return fmt.Errorf("chattr -i %s: %w: %s", dir, err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	isMounted    = paneldb.IsMounted
	probeWrite   = paneldb.ProbeWrite
	abandonCrash = func(string) error { return nil } // a test stops the run here
)

var errNotMoved = errors.New("the panel database is not on a node disk")

// abandonPrecheck says whether the disk may be given up. It refuses a disk that
// is mounted and answers a write: then moving the database back is the way. A
// journal that is not moved is errNotMoved (a restore that already gave the
// disk up carries on as a normal restore).
func abandonPrecheck(dataDir string) error {
	j, err := paneldb.ReadJournal(dataDir)
	if err != nil {
		return err
	}
	switch {
	case j.Phase == paneldb.PhaseAbandoning && j.Direction == paneldb.DirAbandon:
		return nil // resume
	case j.Phase == paneldb.PhaseMoved || j.Moved:
	case j.Phase == paneldb.PhaseNone || j.Phase == paneldb.PhaseFresh || j.Phase == paneldb.PhaseRestoring:
		return errNotMoved
	default:
		return fmt.Errorf("move.journal is in phase %q (%s): run stackr-install (no arguments) on this node first", j.Phase, j.Direction)
	}
	if isMounted(dataDir) {
		if err := probeWrite(filepath.Join(dataDir, paneldb.DirName)); err == nil {
			return errors.New("the node disk is mounted and answers writes: it is healthy, so move the database back instead (stkr node disk move-panel <node> --back)")
		}
	}
	return nil
}

// abandonDisk gives the disk up. It does nothing when the install is not moved.
func abandonDisk(ctx context.Context, dataDir string, out io.Writer) error {
	if err := abandonPrecheck(dataDir); err != nil {
		if errors.Is(err, errNotMoved) {
			return nil
		}
		return err
	}
	j, err := paneldb.ReadJournal(dataDir)
	if err != nil {
		return err
	}
	if j.Phase != paneldb.PhaseAbandoning {
		j.Phase, j.Direction, j.Moved = paneldb.PhaseAbandoning, paneldb.DirAbandon, false
		if err := paneldb.WriteJournal(dataDir, j); err != nil {
			return err
		}
	}
	say := func(a ...any) { _, _ = fmt.Fprintln(out, a...) }
	say("Giving up the node disk", j.DiskID, "for the panel database")
	if err := abandonCrash("journaled"); err != nil {
		return err
	}
	unit := hostdisk.BindUnitName(dataDir)
	haveUnit := exists(filepath.Join(systemdDir, unit))
	if haveUnit {
		// a stop that fails on a dead disk is not the end: the lazy unmount below
		_ = abandonSystemctl(ctx, "stop", unit)
	}
	if isMounted(dataDir) {
		_ = abandonUnmount(ctx, filepath.Join(dataDir, paneldb.DirName))
	}
	if isMounted(dataDir) {
		return errors.New("db/ is still mounted: it could not be unmounted")
	}
	if err := abandonCrash("stopped"); err != nil {
		return err
	}
	if haveUnit {
		if err := abandonSystemctl(ctx, "disable", unit); err != nil {
			return err
		}
	}
	if err := abandonCrash("disabled"); err != nil {
		return err
	}
	dir := filepath.Join(dataDir, paneldb.DirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if ents, err := os.ReadDir(dir); err != nil || len(ents) > 0 {
		return fmt.Errorf("%s is not an empty folder: not touching it (%v)", dir, err)
	}
	if err := abandonUnlock(ctx, dir); err != nil {
		return err
	}
	if err := abandonCrash("unlocked"); err != nil {
		return err
	}
	// the unmoved layout; the restore writes its own journal next
	if err := paneldb.ClearJournal(dataDir); err != nil {
		return err
	}
	say("The disk's own mount unit is kept; its database folder is not used any more.")
	return abandonCrash("journal-cleared")
}

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/paneldb"
)

// panelAnswers: the named panel container runs and its health route answers.
// A variable so a test needs no Docker.
var panelAnswers = func(ctx context.Context, in installspec.Input, name string) bool {
	if running, err := read(ctx, "docker", "inspect", "-f", "{{.State.Running}}", name); err != nil || running != "true" {
		return false
	}
	return waitHealthy(ctx, "http://"+net.JoinHostPort(in.Bind, installspec.PanelPort)+"/api/health") == nil
}

// panelDB runs before anything on the box is touched, before a key is made:
// checkPanelDB on the journal, the way out of a layout change a helper left
// unfinished, and the db/ folder of a fresh install. An install that still
// has its database at the old path is left to the panel swap, which owns the
// layout change.
func (r runner) panelDB(ctx context.Context, in installspec.Input) error {
	dataDir := in.DataDir
	// A rerun starts the disk units first, as disk-check does: a disk attached
	// since boot is mounted before anything judges the journal.
	if !r.dry {
		if err := startDiskUnits(ctx, dataDir, io.Discard); err != nil {
			r.say("  node disk units:", err.Error())
		}
	}
	if err := paneldb.CheckInstaller(dataDir); err != nil {
		return err
	}
	j, err := paneldb.ReadJournal(dataDir)
	if err != nil {
		return err
	}
	if (j.Direction == paneldb.DirIn || j.Direction == paneldb.DirBack) && j.Phase != paneldb.PhaseNone && j.Phase != paneldb.PhaseMoved {
		return r.resumeMove(ctx, in, j)
	}
	if j.Phase == paneldb.PhaseMoved {
		// the disk is the database's home: it must be mounted before anything else runs
		if err := paneldb.Check(dataDir, paneldb.Path(dataDir)); err != nil {
			return err
		}
	}
	if (j.Direction == paneldb.DirLayout || j.Direction == paneldb.DirUpdate) && j.Phase != paneldb.PhaseNone {
		if r.dry {
			r.say("  would undo the unfinished database layout change (move.journal: " + j.Phase + ")")
			return nil
		}
		// From starting-new on the new panel may have taken writes. One that is
		// running and answers is kept: the journal only missed its clearing.
		if (j.Phase == paneldb.PhaseStartingNew || j.Phase == paneldb.PhaseGating) && j.Container != "" && panelAnswers(ctx, in, j.Container) {
			r.say("  the new panel " + j.Container + " is up: finishing the database layout change")
			for _, id := range j.Predecessors {
				if err := r.docker(ctx, "rm", "-f", id); err != nil {
					return err
				}
			}
			if err := paneldb.Finish(dataDir, j); err != nil {
				return err
			}
			paneldb.DropCopy(j.Copy) // only after the journal is clear
			return nil
		}
		// The new panel ran and the copy is gone: the database cannot go back, so
		// the way out is forward, on the migrated database.
		if j.RollbackBlocked() {
			return r.keepNewPanel(ctx, in, j)
		}
		r.say("  undoing an unfinished database layout change (move.journal: " + j.Phase + ")")
		// The new panel of the dead helper goes first: nothing may write while the files move back.
		_ = r.quietDocker(ctx, "rm", "-f", upgraderName)
		if j.Container != "" {
			_ = r.quietDocker(ctx, "rm", "-f", j.Container)
		}
		if err := paneldb.Recover(dataDir); err != nil {
			return fmt.Errorf("undo the database layout change (the old panel is not started): %w", err)
		}
		// Only the predecessor the journal recorded runs again, on its own spec.
		ids := j.Predecessors
		if len(ids) == 0 { // a journal from before they were recorded
			all, _ := read(ctx, "docker", "ps", "-aq", "--filter", "label="+installspec.LabelRole+"="+"panel")
			ids = strings.Fields(all)
		}
		for _, id := range ids {
			if err := r.docker(ctx, "start", id); err != nil {
				return err
			}
		}
	}
	if r.dry {
		return nil
	}
	if fi, err := os.Lstat(paneldb.Legacy(dataDir)); err == nil && fi.Mode().IsRegular() {
		return nil // an old layout: the panel swap moves it
	}
	return paneldb.EnsureFresh(dataDir)
}

// keepNewPanel is the way out when a swap died after its new panel ran and the
// pre-upgrade copy is gone: start the recorded new panel again (its container,
// or its image under the same name) on the migrated database, and clear the
// journal only once it answers and its predecessors are removed.
func (r runner) keepNewPanel(ctx context.Context, in installspec.Input, j paneldb.Journal) error {
	dataDir := in.DataDir
	r.say("  the pre-upgrade database copy is gone, so the database cannot go back: starting the new panel", j.Container, "on it")
	if j.Container == "" {
		return errors.New("move.journal names no new panel to start: run stackr-install restore <archive>")
	}
	_ = r.quietDocker(ctx, "rm", "-f", upgraderName)
	j.Phase = paneldb.PhaseGating // the new panel is the one booting
	if err := paneldb.WriteJournal(dataDir, j); err != nil {
		return err
	}
	if _, err := read(ctx, "docker", "inspect", "-f", "{{.State.Running}}", j.Container); err == nil {
		if err := r.docker(ctx, "start", j.Container); err != nil {
			return err
		}
	} else {
		if j.Image == "" {
			return errors.New("move.journal names no image for the new panel: run stackr-install restore <archive>")
		}
		in.DisksBind = installspec.HostDisksBind("")
		c := installspec.Panel(j.Image, in)
		c.Name = j.Container
		if err := r.docker(ctx, c.RunArgs()...); err != nil {
			return err
		}
	}
	if !panelAnswers(ctx, in, j.Container) {
		return fmt.Errorf("the new panel %s did not come up on the migrated database; the journal stays. Check docker logs %s, or run stackr-install restore <archive>", j.Container, j.Container)
	}
	for _, id := range j.Predecessors {
		if err := r.docker(ctx, "rm", "-f", id); err != nil {
			return err
		}
	}
	return paneldb.Finish(dataDir, j)
}

// resumeMove finishes or rolls back a database move a helper left unfinished:
// the same helper, run in the foreground from the image the journal names, reads
// the journal and acts on its phase. This is the way out after the helper's
// three failures.
func (r runner) resumeMove(ctx context.Context, in installspec.Input, j paneldb.Journal) error {
	r.say("  an unfinished database move (" + j.Direction + ", move.journal phase " + j.Phase + "): finishing or rolling it back")
	if r.dry {
		return nil
	}
	ver, ok := strings.CutPrefix(j.Image, installspec.Repo+":")
	if !ok || j.Container == "" {
		return fmt.Errorf("move.journal names no release image (%q) or no new panel: run stackr-install restore <archive>", j.Image)
	}
	in.DisksBind = installspec.HostDisksBind("")
	spec, err := installspec.SwapSpecAs(j.Container, ver, in)
	if err != nil {
		return err
	}
	mv, err := json.Marshal(struct { // the helper's Move (leaf/panel), by its JSON
		Dir       string `json:"dir"`
		MoveID    string `json:"move_id"`
		DiskID    string `json:"disk_id"`
		Folder    string `json:"folder"`
		InstallID string `json:"install_id"`
	}{j.Direction, j.MoveID, j.DiskID, j.Folder, j.InstallID})
	if err != nil {
		return err
	}
	if running, err := read(ctx, "docker", "inspect", "-f", "{{.State.Running}}", upgraderName); err == nil {
		if running == "true" {
			return errors.New("an upgrade or move is already running (container " + upgraderName + ")")
		}
		if err := r.docker(ctx, "rm", "-f", upgraderName); err != nil {
			return err
		}
	}
	if err := r.docker(ctx, "run", "--name", upgraderName, "--network", "host", "--label", installspec.LabelRole+"=upgrader", "--label", installspec.LabelMove+"="+j.MoveID,
		"-v", "/var/run/docker.sock:/var/run/docker.sock", "-v", in.DataDir+":"+in.DataDir,
		"-e", installspec.SwapSpecEnv+"="+spec, "-e", "STACKR_PANEL_MOVE="+string(mv),
		j.Image, "upgrade-swap"); err != nil {
		return fmt.Errorf("%w; see docker logs %s and move.journal", err, upgraderName)
	}
	_ = r.docker(ctx, "rm", "-f", upgraderName)
	return nil
}

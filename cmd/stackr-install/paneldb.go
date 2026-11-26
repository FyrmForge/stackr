package main

import (
	"context"
	"fmt"
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
	if err := paneldb.CheckInstaller(dataDir); err != nil {
		return err
	}
	j, err := paneldb.ReadJournal(dataDir)
	if err != nil {
		return err
	}
	if j.Direction == paneldb.DirLayout && j.Phase != paneldb.PhaseNone {
		if r.dry {
			r.say("  would undo the unfinished database layout change (move.journal: " + j.Phase + ")")
			return nil
		}
		// From starting-new on the new panel may have taken writes. One that is
		// running and answers is kept: the journal only missed its clearing.
		if (j.Phase == paneldb.PhaseStartingNew || j.Phase == paneldb.PhaseGating) && j.Container != "" && panelAnswers(ctx, in, j.Container) {
			r.say("  the new panel " + j.Container + " is up: finishing the database layout change")
			paneldb.DropCopy(j.Copy)
			return paneldb.ClearJournal(dataDir)
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
		// The old panel was stopped by the swap; it runs on its own spec again.
		ids, _ := read(ctx, "docker", "ps", "-aq", "--filter", "label="+installspec.LabelRole+"="+"panel")
		for _, id := range strings.Fields(ids) {
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
	return paneldb.EnsureLayout(dataDir)
}

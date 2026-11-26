package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/FyrmForge/stackr/internal/paneldb"
)

// startDBTicker is the runtime half of "the database is on a node disk": a
// tiny write and fsync in db/ every few seconds. Docker does not restart a
// container that is running, so a panel on a failing disk would sit there
// hung; this one exits after consecutive failures, lands in the boot check and
// Docker's restart loop waits for the disk. Only when the journal says moved.
func startDBTicker(log *slog.Logger, dataDir string) {
	if !dbOnDisk(dataDir) {
		return
	}
	dir := filepath.Join(dataDir, paneldb.DirName)
	go paneldb.Watch(context.Background(), paneldb.TickEvery, paneldb.TickMax,
		func() error { return paneldb.ProbeWrite(dir) },
		func(err error) {
			log.Error("the database disk stopped answering: exiting so the boot check judges it", "err", err)
			os.Exit(1)
		})
}

// dbOnDisk: db/ is a mount, or the journal says the database is on a disk
// (moved, an Update on it, a move-in in progress). The panel a move or an
// Update just started runs under a journal that is not "moved" yet.
func dbOnDisk(dataDir string) bool {
	if paneldb.IsMounted(dataDir) {
		return true
	}
	j, err := paneldb.ReadJournal(dataDir)
	return err == nil && (j.Phase == paneldb.PhaseMoved || j.Moved || (j.Direction == paneldb.DirIn && j.Phase != paneldb.PhaseNone))
}

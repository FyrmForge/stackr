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
	j, err := paneldb.ReadJournal(dataDir)
	if err != nil || j.Phase != paneldb.PhaseMoved {
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

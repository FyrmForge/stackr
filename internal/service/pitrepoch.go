package service

import (
	"context"
	"log/slog"
	"slices"
	"strconv"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	mflow "github.com/FyrmForge/stackr/internal/service/internal/flow/operated"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/backup"
)

// A major change or its rollback starts a new archive epoch (pgupgrade.go
// swapMajor); the old epochs' WAL belongs to the old cluster and no restore
// reads it (a restore plans from the current epoch only). They are kept as
// long as a rollback is possible, then dropped: the remote objects under
// backup.PITREpochRoot, and the epoch's directory on the archive volume
// (nothing ships from it: the shipper reads the current epoch's only, so its
// unshipped WAL is of no use after the window either).

const epochKey = "pgepoch." // + instance id: the first epoch not dropped yet

// sweepArchiveEpochs is the hourly pass. An instance whose major change is in
// flight or still rollbackable keeps every epoch.
func (o *Orchestrator) sweepArchiveEpochs(ctx context.Context) {
	ms, err := o.store.OperatedInstances.ListEngine(ctx, "postgres")
	if err != nil {
		return
	}
	for _, m := range ms {
		if m.ArchiveEpoch <= 0 {
			continue
		}
		if st, ok := o.pgupOf(ctx, m.ID); ok && (!st.Done || time.Since(st.At) < pgupKeep) {
			continue
		}
		if err := o.dropOldEpochs(ctx, m); err != nil {
			slog.Warn("pitr: old archive epochs", "instance", m.ID, "err", err)
		}
	}
}

func (o *Orchestrator) dropOldEpochs(ctx context.Context, m OperatedInstance) error {
	from := 0
	if v, ok, err := o.store.Settings.Get(ctx, epochKey+m.ID); err == nil && ok {
		from, _ = strconv.Atoi(v)
	}
	if from >= m.ArchiveEpoch {
		return nil
	}
	it, err := o.tiles.Get(ctx, m.TileID)
	if err != nil {
		return err
	}
	v, err := o.dataVolume(ctx, it)
	if err != nil {
		return err
	}
	org, err := o.volumeOrg(ctx, v)
	if err != nil {
		return err
	}
	dest, err := o.pitrDest(ctx, org, v)
	if err != nil {
		return err
	}
	cs, err := o.tiles.Replicas(ctx, it)
	if err != nil {
		return err
	}
	var local *docker.Container
	if i := slices.IndexFunc(cs, func(c docker.Container) bool { return c.State == "running" }); i >= 0 {
		local = &cs[i]
	}
	for e := from; e < m.ArchiveEpoch; e++ {
		if n, err := o.backup.DropArchive(ctx, dest, backup.PITREpochRoot(org, v.ID, e)); err != nil {
			return err
		} else if n > 0 {
			slog.Info("pitr: dropped an old archive epoch", "instance", m.ID, "epoch", e, "objects", n)
		}
		if local == nil {
			return nil // the archive volume is read through the running container: next pass
		}
		// the first epoch's directory is the volume's root, with the later ones in it: only its files go
		rm := []string{"rm", "-rf", "--", mflow.ArchiveDirOf(e)}
		if e == 0 {
			rm = []string{"find", mflow.ArchiveDirOf(0), "-maxdepth", "1", "-type", "f", "-delete"}
		}
		if _, err := o.tiles.Exec(ctx, it.ID, local.ID, rm); err != nil {
			return err
		}
		if err := o.store.Settings.Set(ctx, epochKey+m.ID, strconv.Itoa(e+1)); err != nil {
			return err
		}
	}
	return nil
}

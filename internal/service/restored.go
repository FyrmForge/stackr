package service

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
)

// redeployRestored runs once, on the first boot after stackr-install
// restore: the restore leaves installspec.RestoredMarker in the data dir. A
// wiped controller lost its tile containers; every tile of an env placed on
// the controller that ran before and has none now gets a normal deploy job.
// Workers kept theirs; a tile stopped before stays stopped. Errors are
// logged, never fatal: the panel still serves.
func (o *Orchestrator) redeployRestored(ctx context.Context) {
	marker := filepath.Join(o.cfg.DataDir, installspec.RestoredMarker)
	if _, err := os.Stat(marker); err != nil {
		return
	}
	n, err := o.queueRestored(ctx)
	if err != nil {
		slog.Warn("restore: redeploy of the controller's tiles stopped", "err", err)
	}
	slog.Info("restore: deploys queued for the controller's tiles", "count", n)
	if err := os.Remove(marker); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("restore: marker not removed", "path", marker, "err", err)
	}
}

func (o *Orchestrator) queueRestored(ctx context.Context) (int, error) {
	ctl, err := o.controllerID(ctx)
	if err != nil {
		return 0, err
	}
	es, err := o.store.Environments.ListByNode(ctx, ctl)
	if err != nil {
		return 0, err
	}
	moves, err := o.store.Moves.ListUnfinished(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range es {
		if slices.ContainsFunc(moves, func(m EnvMove) bool { return m.EnvID == e.ID }) {
			continue // resumeMoves owns it
		}
		if e.PlacementID != "" {
			p, err := o.store.Placements.Get(ctx, e.PlacementID)
			if err != nil || (p.NodeID != "" && p.NodeID != ctl) {
				continue
			}
		}
		ts, err := o.store.Tiles.ListByEnv(ctx, e.ID)
		if err != nil {
			return n, err
		}
		for _, t := range ts {
			if t.Kind == tile.Slice || tile.RunToCompletion(t.Kind) {
				continue // no containers; crons come back through the scheduler
			}
			ran, err := o.ranBeforeRestore(ctx, t)
			if err != nil || !ran {
				if err != nil {
					slog.Warn("restore: tile skipped", "tile", t.ID, "err", err)
				}
				continue
			}
			if _, err := o.Deploy(ctx, t.ID); err != nil {
				slog.Warn("restore: deploy not queued", "tile", t.ID, "err", err)
				continue
			}
			n++
		}
	}
	return n, nil
}

// ranBeforeRestore: the tile has no replica now and the newest finished job
// that started or stopped its containers started them.
// ponytail: scans the tile's newest 100 jobs; a tile with more runs or
// failures than that since its last deploy reads as never deployed.
func (o *Orchestrator) ranBeforeRestore(ctx context.Context, t Tile) (bool, error) {
	cs, err := o.tiles.Replicas(ctx, t)
	if err != nil || len(cs) > 0 {
		return false, err
	}
	js, err := o.jobRows.History(ctx, []string{t.ID}, 100)
	if err != nil {
		return false, err
	}
	for _, j := range js {
		if j.State != job.Done {
			continue
		}
		switch j.Kind {
		case string(kindStop):
			return false, nil
		case string(kindDeploy), string(kindPromote), string(kindEnvSync), string(kindStart), string(kindRestart):
			return true, nil
		}
	}
	return false, nil
}

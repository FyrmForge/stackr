package service

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// How long finished work is kept.
// ponytail: constants, not settings; make them knobs if 30 and 90 days are wrong for someone.
const (
	jobKeep       = 30 * 24 * time.Hour // job rows, their logs, dead join tokens
	securityKeep  = 90 * 24 * time.Hour // security_events
	jobPruneBatch = 5000                // rows whose log files one pass removes by name
)

// pruneOld is the daily sweep of what only ever grows: finished jobs and
// their logs (<data>/jobs/*.log, also by age for logs with no row), security
// events, and join tokens that can no longer be used. Each step is logged and
// the next still runs.
func (o *Orchestrator) pruneOld(ctx context.Context) {
	now := time.Now().UTC()
	cut := now.Add(-jobKeep)
	if js, err := o.store.Jobs.ListFinishedBefore(ctx, cut, jobPruneBatch); err != nil {
		slog.Warn("prune: jobs not listed", "err", err)
	} else {
		for _, j := range js {
			if j.LogPath != "" {
				_ = os.Remove(j.LogPath)
			}
			if err := o.store.Jobs.Delete(ctx, j.ID); err != nil {
				slog.Warn("prune: job not deleted", "job", j.ID, "err", err)
			}
		}
	}
	// Logs whose row is already gone, or never had a finish time.
	dir := filepath.Join(o.cfg.DataDir, "jobs")
	if es, err := os.ReadDir(dir); err == nil {
		for _, e := range es {
			if fi, err := e.Info(); err == nil && !e.IsDir() && filepath.Ext(e.Name()) == ".log" && fi.ModTime().Before(cut) {
				_ = os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
	// security_events refuses deletes unless this row exists; the row is set
	// and cleared in the same transaction as the delete.
	err := o.store.Tx(ctx, func(tx store.Tx) error {
		if err := tx.Settings.Set(ctx, store.SecurityEventsPruning, "1"); err != nil {
			return err
		}
		if err := tx.SecurityEvents.DeleteBefore(ctx, now.Add(-securityKeep)); err != nil {
			return err
		}
		return tx.Settings.Delete(ctx, store.SecurityEventsPruning)
	})
	if err != nil {
		slog.Warn("prune: security events not pruned", "err", err)
	}
	if err := o.store.JoinTokens.DeleteDeadBefore(ctx, cut); err != nil {
		slog.Warn("prune: join tokens not pruned", "err", err)
	}
}

// sweepMoveTemps removes the temp files an env move leaves in the data dir
// when the panel dies mid-move (their defers never ran). Boot only: nothing
// is moving yet.
func (o *Orchestrator) sweepMoveTemps() {
	for _, pat := range []string{"rsync-build-*", "push-*.tar"} {
		ms, _ := filepath.Glob(filepath.Join(o.cfg.DataDir, pat))
		for _, m := range ms {
			if err := os.RemoveAll(m); err != nil {
				slog.Warn("move: temp not removed", "path", m, "err", err)
			}
		}
	}
}

// prunePulledImages removes, on every node, the pulled images stackr tracked
// that no tile names any more (the watch's rows, not built here), and their
// rows. A container still using one keeps it: Docker refuses. An image of a
// release that can still roll back stays (the row is pinned).
// ponytail: only images stackr tracked; a pulled image with no row (a tile
// deleted before its first check) is left for a manual docker prune.
func (o *Orchestrator) prunePulledImages(ctx context.Context, note func(what string, n int, size string, err error)) {
	rows, err := o.store.Images.List(ctx)
	if err != nil {
		note("pulled images", 0, "", err)
		return
	}
	ts, err := o.store.Tiles.ListAll(ctx)
	if err != nil {
		note("pulled images", 0, "", err)
		return
	}
	pinned, err := o.releases.ImageIDs(ctx)
	if err != nil {
		note("pulled images", 0, "", err)
		return
	}
	used := map[string]bool{}
	for _, t := range ts {
		used[t.ImageRef] = true
	}
	var stale []store.Image
	for _, r := range rows {
		if r.BuiltAt == nil && !used[r.Ref] && !slices.Contains(pinned, r.ID) {
			stale = append(stale, r)
		}
	}
	if len(stale) == 0 {
		return
	}
	n, size := 0, int64(0)
	var failed []error
	_ = o.eachNode(ctx, func(ctx context.Context, nd store.Node) error {
		ims, err := o.docker.ListImages(ctx, nil)
		if err != nil {
			failed = append(failed, err)
			return nil
		}
		for _, r := range stale {
			for _, im := range ims {
				if !slices.Contains(im.Tags, r.Ref) {
					continue
				}
				switch err := o.docker.RemoveImage(ctx, r.Ref); {
				case err == nil:
					n, size = n+1, size+im.Size
				case errors.Is(err, docker.ErrNotFound):
				default:
					failed = append(failed, err) // in use: the rows stay
				}
			}
		}
		return nil
	})
	if len(failed) == 0 {
		for _, r := range stale {
			_ = o.store.Images.Delete(ctx, r.ID)
		}
	}
	note("pulled images", n, byteSize(size), errors.Join(failed...))
}

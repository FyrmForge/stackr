package service

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// How long finished work is kept.
// ponytail: constants, not settings; make them knobs if 30 and 90 days are wrong for someone.
const (
	jobKeep            = 30 * 24 * time.Hour // job rows, their logs, dead join tokens
	securityKeep       = 90 * 24 * time.Hour // security_events
	securityPruneBatch = 1000                // security_events rows per delete statement
	jobPruneBatch      = 5000                // rows whose log files one pass removes by name
)

// pruneOld is the daily sweep of what only ever grows: finished jobs and
// their logs (<data>/jobs/*.log, also by age for logs with no row), security
// events, and join tokens that can no longer be used. Each step is logged and
// the next still runs.
func (o *Orchestrator) pruneOld(ctx context.Context) {
	now := time.Now().UTC()
	cut := now.Add(-jobKeep)
	keep := o.jobsToKeep(ctx)
	if js, err := o.store.Jobs.ListFinishedBefore(ctx, cut, jobPruneBatch); err != nil {
		slog.Warn("prune: jobs not listed", "err", err)
	} else {
		for _, j := range js {
			if keep[j.ID] {
				continue
			}
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
	o.pruneSecurityEvents(ctx, now.Add(-securityKeep))
	if err := o.store.JoinTokens.DeleteDeadBefore(ctx, cut); err != nil {
		slog.Warn("prune: join tokens not pruned", "err", err)
	}
}

// pruneSecurityEvents deletes events older than cut in batches. security_events
// refuses deletes unless the settings row exists, so each batch sets and clears
// it in its own short transaction: the append-only guard is never left lifted,
// and the single connection is free between batches.
func (o *Orchestrator) pruneSecurityEvents(ctx context.Context, cut time.Time) {
	for {
		n := 0
		err := o.store.Tx(ctx, func(tx store.Tx) (err error) {
			if err = tx.Settings.Set(ctx, store.SecurityEventsPruning, "1"); err != nil {
				return err
			}
			if n, err = tx.SecurityEvents.DeleteBefore(ctx, cut, securityPruneBatch); err != nil {
				return err
			}
			return tx.Settings.Delete(ctx, store.SecurityEventsPruning)
		})
		if err != nil {
			slog.Warn("prune: security events not pruned", "err", err)
			return
		}
		if n < securityPruneBatch || ctx.Err() != nil {
			return
		}
	}
}

// jobsToKeep are the finished jobs the rest of the panel reads as state, however
// old: each tile's or env's newest done job (what a tile last did: whether it
// ran before a panel restore, whether it was stopped), and the newest
// rollbackDepth done jobs that carry a release per tile or env (what the image
// sweep keeps for a rollback, and what a tile runs).
// ponytail: reads every done job, newest first; fine into the tens of
// thousands, and the 30 day prune keeps it there.
func (o *Orchestrator) jobsToKeep(ctx context.Context) map[string]bool {
	keep := map[string]bool{}
	js, err := o.jobRows.Recent(ctx, 0, job.Done)
	if err != nil {
		slog.Warn("prune: jobs not read, keeping them all", "err", err)
		return nil
	}
	newest := map[string]bool{}
	withRelease := map[string]int{}
	for _, j := range js { // newest first
		for _, id := range j.LockSet {
			if !newest[id] {
				newest[id], keep[j.ID] = true, true
			}
		}
		if j.ReleaseID == nil {
			continue
		}
		key := strings.Join(j.LockSet, ",")
		var p promoteJob
		if j.Kind == string(kindPromote) && json.Unmarshal([]byte(j.Payload), &p) == nil && p.EnvID != "" {
			key = p.EnvID
		}
		if withRelease[key] < rollbackDepth {
			keep[j.ID] = true
		}
		withRelease[key]++
	}
	return keep
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

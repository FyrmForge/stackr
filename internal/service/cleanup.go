package service

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/image"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
)

// rollbackDepth is how many releases each env stays rollback-reachable on,
// so their images survive a sweep. The release list shows them all; this is
// the cut for disk.
// ponytail: a constant, not a setting; make it a knob if five is wrong for someone.
const rollbackDepth = 5

// buildCacheAge is how old a build cache entry must be to go.
const buildCacheAge = 7 * 24 * time.Hour

// scheduleSettings is the effective flat knobs the cron table reads.
func (o *Orchestrator) scheduleSettings(ctx context.Context) (map[string]string, error) {
	out := map[string]string{}
	for k := range scheduleKnobs {
		v, err := o.settings.Get(ctx, k)
		if err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, nil
}

// runCleanup is the disk sweep: dangling stackr images, stackr builds no
// running container or recent release reaches, and old build cache. It never
// touches an image stackr did not build, nor a volume. A step that fails is
// logged and the next still runs.
func (o *Orchestrator) runCleanup(ctx context.Context, r *jobs.Run) error {
	on, err := o.settings.Get(ctx, "cleanup_enabled")
	if err != nil {
		return err
	}
	if v, _ := strconv.ParseBool(on); !v {
		return nil
	}
	built := map[string]string{image.LabelBuilt: "true"}
	var failed error
	note := func(what string, n int, size string, err error) {
		if err != nil {
			_, _ = fmt.Fprintf(r.Log, "%s: failed: %v\n", what, err)
			failed = err
			return
		}
		_, _ = fmt.Fprintf(r.Log, "%s: %d removed, %s reclaimed\n", what, n, size)
	}

	n, b, err := o.docker.PruneDangling(ctx, built)
	note("dangling images", n, byteSize(b), err)

	keep, pinned, running, err := o.cleanupKeep(ctx)
	if err == nil {
		var gone []string
		var freed int64
		gone, freed, err = o.images.Cleanup(ctx, keep, pinned, running)
		n, b = len(gone), freed
	}
	note("built images", n, "up to "+byteSize(b), err)

	n, total, err := o.docker.BuildCachePrune(ctx, image.Builder, buildCacheAge)
	if total == "" {
		total = "0 B"
	}
	note("build cache", n, total, err)
	return failed
}

// cleanupKeep is the image rows of every release a sweep must not strand:
// each env's current release and the last rollbackDepth it was on (done
// promote and rollback jobs), every release a live job targets (a parked
// promote deploys it later), and the newest release of each stack (the next
// promote). pinned is every image row any release references: those rows
// stay even when the Docker image goes. running is the refs of what any
// container on the box uses.
func (o *Orchestrator) cleanupKeep(ctx context.Context) (ids, pinned, running []string, err error) {
	orgs, err := o.orgs.ListAll(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	rels := map[string]bool{}
	depth := map[string][]string{} // env id -> releases it was on, newest first
	for _, org := range orgs {
		stacks, err := o.stacks.List(ctx, org.ID)
		if err != nil {
			return nil, nil, nil, err
		}
		for _, s := range stacks {
			rs, err := o.releases.List(ctx, s.ID) // newest first
			if err != nil {
				return nil, nil, nil, err
			}
			if len(rs) > 0 {
				rels[rs[0].ID] = true
			}
			envs, err := o.envs.List(ctx, s.ID)
			if err != nil {
				return nil, nil, nil, err
			}
			for _, e := range envs {
				depth[e.ID] = nil
				if e.ReleaseID != nil {
					depth[e.ID] = []string{*e.ReleaseID}
				}
			}
		}
	}
	js, err := o.jobRows.Recent(ctx, 0, job.Queued, job.Running, job.Waiting, job.Done) // newest first
	if err != nil {
		return nil, nil, nil, err
	}
	for _, j := range js {
		if j.ReleaseID == nil {
			continue
		}
		if job.Live(j.State) {
			rels[*j.ReleaseID] = true
			continue
		}
		var p promoteJob
		if j.Kind != string(kindPromote) || json.Unmarshal([]byte(j.Payload), &p) != nil {
			continue
		}
		if h, ok := depth[p.EnvID]; ok && len(h) < rollbackDepth && !slices.Contains(h, *j.ReleaseID) {
			depth[p.EnvID] = append(h, *j.ReleaseID)
		}
	}
	for _, h := range depth {
		for _, id := range h {
			rels[id] = true
		}
	}
	for id := range rels {
		pins, err := o.releases.Pins(ctx, id)
		if err != nil {
			return nil, nil, nil, err
		}
		for _, p := range pins {
			if p.ImageID != nil {
				ids = append(ids, *p.ImageID)
			}
		}
	}
	if pinned, err = o.releases.ImageIDs(ctx); err != nil {
		return nil, nil, nil, err
	}
	cs, err := o.docker.List(ctx, nil)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, c := range cs {
		running = append(running, c.Image)
	}
	return ids, pinned, running, nil
}

// byteSize is n in the largest binary unit that keeps it above 1.
func byteSize(n int64) string {
	const units = "KMGT"
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	f, u := float64(n)/1024, 0
	for f >= 1024 && u < len(units)-1 {
		f, u = f/1024, u+1
	}
	return fmt.Sprintf("%.1f %ciB", f, units[u])
}

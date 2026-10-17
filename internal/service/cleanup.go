package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/image"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
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
// running container or recent release reaches, stackr's own images of other
// builds than the running one and its rollback (stackrKeep), and old build
// cache. It never touches any other image stackr did not build, nor a volume.
// A step that fails is logged and the next still runs.
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

	// Every node prunes its own Docker; an image row is deleted only after
	// they all have (Settle).
	_ = o.eachNode(ctx, func(ctx context.Context, nd store.Node) error {
		n, b, err := o.docker.PruneDangling(ctx, built)
		note("dangling images on "+nd.Name, n, byteSize(b), err)
		return nil
	})

	if keep, err := o.stackrKeep(ctx); err != nil {
		note("old stackr images", 0, "", err)
	} else if keep != nil {
		_ = o.eachNode(ctx, func(ctx context.Context, nd store.Node) error {
			n, b, err := o.pruneStackrImages(ctx, keep)
			note("old stackr images on "+nd.Name, n, byteSize(b), err)
			return nil
		})
	}

	n, b := 0, int64(0)
	keep, pinned, running, err := o.cleanupKeep(ctx)
	var plan image.Plan
	if err == nil {
		plan, err = o.images.Plan(ctx, keep, running)
	}
	if err == nil {
		var gone []string
		pruneErr := o.eachNode(ctx, func(ctx context.Context, _ store.Node) error {
			g, freed, err := o.images.Prune(ctx, plan)
			gone, b = append(gone, g...), b+freed
			return err
		})
		// The rows of what any node removed go, a failed node's partial answer
		// included. What a failed node still holds has no row left to keep it,
		// so its next sweep prunes it by the build label.
		err = errors.Join(pruneErr, o.images.Settle(ctx, plan, pinned, gone))
		n = len(gone)
	}
	note("built images", n, "up to "+byteSize(b), err)

	_ = o.eachNode(ctx, func(ctx context.Context, nd store.Node) error {
		n, total, err := o.docker.BuildCachePrune(ctx, image.Builder, buildCacheAge)
		if total == "" {
			total = "0 B"
		}
		note("build cache on "+nd.Name, n, total, err)
		return nil
	})
	return failed
}

// keptReleases is the release ids a sweep must not strand: each env's current
// release, the last rollbackDepth it was on (done promote and rollback jobs)
// and the releases numbered between them (keepBetween), every release a live
// job targets (a parked promote deploys it later), and the newest release of
// each stack (the next promote). The image sweep and the registry sweep both
// keep what these pin.
func (o *Orchestrator) keptReleases(ctx context.Context) (map[string]bool, error) {
	orgs, err := o.orgs.ListAll(ctx)
	if err != nil {
		return nil, err
	}
	rels := map[string]bool{}
	depth := map[string][]string{} // env id -> releases it was on, newest first
	byStack := map[string][]store.Release{}
	stackOf := map[string]string{} // env id -> stack id
	for _, org := range orgs {
		stacks, err := o.stacks.List(ctx, org.ID)
		if err != nil {
			return nil, err
		}
		for _, s := range stacks {
			rs, err := o.releases.List(ctx, s.ID) // newest first
			if err != nil {
				return nil, err
			}
			if len(rs) > 0 {
				rels[rs[0].ID] = true
			}
			byStack[s.ID] = rs
			envs, err := o.envs.List(ctx, s.ID)
			if err != nil {
				return nil, err
			}
			for _, e := range envs {
				depth[e.ID] = nil
				stackOf[e.ID] = s.ID
				if e.ReleaseID != nil {
					depth[e.ID] = []string{*e.ReleaseID}
				}
			}
		}
	}
	js, err := o.jobRows.Recent(ctx, 0, job.Queued, job.Running, job.Waiting, job.Done) // newest first
	if err != nil {
		return nil, err
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
	for env, h := range depth {
		for _, id := range h {
			rels[id] = true
		}
		keepBetween(rels, byStack[stackOf[env]], h)
	}
	return rels, nil
}

// keepBetween adds to rels every release of rs numbered from the oldest of
// hist to its newest. An env also reaches releases no promote job targets
// (a redeploy's, a promote's re-pin), and Roll back offers any older one: the
// release it ran before the current is one of these. With only the current
// release known, the one numbered below it is kept too.
// ponytail: by number, so another env's releases in that span stay as well;
// an env release history would be exact.
func keepBetween(rels map[string]bool, rs []store.Release, hist []string) {
	lo, hi := 0, 0
	for _, r := range rs {
		if slices.Contains(hist, r.ID) {
			if lo == 0 || r.Number < lo {
				lo = r.Number
			}
			hi = max(hi, r.Number)
		}
	}
	if lo == 0 {
		return
	}
	if len(hist) == 1 {
		lo--
	}
	for _, r := range rs {
		if r.Number >= lo && r.Number <= hi {
			rels[r.ID] = true
		}
	}
}

// cleanupKeep is the image rows the releases of keptReleases pin. pinned is
// every image row any release references: those rows stay even when the Docker
// image goes. running is the refs of what any container on the box uses.
func (o *Orchestrator) cleanupKeep(ctx context.Context) (ids, pinned, running []string, err error) {
	rels, err := o.keptReleases(ctx)
	if err != nil {
		return nil, nil, nil, err
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
	// What any container on any node uses stays: an image ref is the same
	// on every node.
	err = o.eachNode(ctx, func(ctx context.Context, _ store.Node) error {
		cs, err := o.docker.List(ctx, nil)
		for _, c := range cs {
			running = append(running, c.Image)
		}
		return err
	})
	if err != nil {
		return nil, nil, nil, err
	}
	return ids, pinned, running, nil
}

// stackrImageRepos are the names a stackr image goes by on a node: the
// release on ghcr (the installer's and the panel upgrade's pull) and the
// cluster registry copy (the controller's publish tag, a worker's load).
var stackrImageRepos = []string{installspec.Repo, docker.ClusterPrefix + stackrRepo}

// splitTag cuts ref into repo and tag; ok is false for a ref with no tag.
func splitTag(ref string) (repo, tag string, ok bool) {
	i := strings.LastIndex(ref, ":")
	if i <= strings.LastIndex(ref, "/") {
		return ref, "", false
	}
	return ref[:i], ref[i+1:], true
}

// stackrKeep is the stackr image names every node keeps: the running build
// (the controller proxy's tag and cfg.Version) and the one before it
// (panel_prev_version, the build workers may still run and an upgrade rolls
// back to), under both repos and under the registry digest a worker loads it
// by (docker localName). nil, nil = remove nothing: the running build is not
// known, or a panel upgrade is under way (its new image is neither yet).
func (o *Orchestrator) stackrKeep(ctx context.Context) ([]string, error) {
	img, err := o.controllerStackrImage(ctx)
	if err != nil {
		return nil, err
	}
	var vers []string
	if repo, tag, ok := splitTag(img); ok && slices.Contains(stackrImageRepos, repo) {
		vers = append(vers, tag)
	}
	if v := strings.TrimPrefix(o.cfg.Version, "v"); v != "" {
		vers = append(vers, v)
	}
	if len(vers) == 0 {
		return nil, nil
	}
	js, err := o.store.Jobs.ListByState(ctx, "queued", "running", "waiting")
	if err != nil {
		return nil, err
	}
	if slices.ContainsFunc(js, func(j store.Job) bool { return j.Kind == string(kindUpgrade) }) {
		return nil, nil
	}
	prev, _, err := o.store.Settings.Get(ctx, keyPanelPrevVersion)
	if err != nil {
		return nil, err
	}
	if prev = strings.TrimPrefix(prev, "v"); prev != "" {
		vers = append(vers, prev)
	}
	var keep []string
	for _, v := range vers {
		for _, repo := range stackrImageRepos {
			keep = append(keep, repo+":"+v)
		}
		dg, err := o.regs.Resolve(ctx, stackrRepo, v)
		if err != nil {
			return nil, fmt.Errorf("the registry digest of stackr %s: %w", v, err)
		}
		pushed, _ := stackrDigests.Load(v)
		for _, d := range []any{dg, pushed} {
			if d, ok := d.(string); ok && d != "" {
				keep = append(keep, docker.ClusterPrefix+stackrRepo+":sha256-"+strings.TrimPrefix(d, "sha256:"))
			}
		}
	}
	return keep, nil
}

// pruneStackrImages removes, on the node in ctx, every stackr image (by
// stackrImageRepos) none of whose names is in keep and that no container,
// running or stopped, was made from. Only the stackr names are untagged: an
// image also tagged otherwise stays under that name. Returns how many images
// went and their size.
func (o *Orchestrator) pruneStackrImages(ctx context.Context, keep []string) (int, int64, error) {
	cs, err := o.docker.List(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	used := map[string]bool{}
	for _, c := range cs {
		used[c.Image], used[c.ImageID] = true, true
	}
	ims, err := o.docker.ListImages(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	n, size := 0, int64(0)
	var failed []error
	for _, im := range ims {
		var mine []string
		for _, t := range im.Tags {
			if repo, _, ok := splitTag(t); ok && slices.Contains(stackrImageRepos, repo) {
				mine = append(mine, t)
			}
		}
		if len(mine) == 0 || used[im.ID] ||
			slices.ContainsFunc(im.Tags, func(t string) bool { return used[t] || slices.Contains(keep, t) }) {
			continue
		}
		gone := 0
		for _, t := range mine {
			switch err := o.docker.RemoveImage(ctx, t); {
			case err == nil, errors.Is(err, docker.ErrNotFound):
				gone++
			default:
				failed = append(failed, err)
			}
		}
		if gone == len(im.Tags) {
			n, size = n+1, size+im.Size
		}
	}
	return n, size, errors.Join(failed...)
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

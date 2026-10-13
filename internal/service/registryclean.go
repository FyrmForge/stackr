package service

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/image"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/securitylog"
	"github.com/FyrmForge/stackr/internal/service/internal/regfront"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// registrySweepTimeout bounds the gated part: the mark, the garbage collect
// and the size reads. The gate opens when it ends, however it ends.
const registrySweepTimeout = 10 * time.Minute

// registryDataDir and registryConfig are where the registry:3 image keeps its
// blobs and its config (the config installspec.Registry leaves in place).
const (
	registryDataDir = "/var/lib/registry"
	registryConfig  = "/etc/distribution/config.yml"
)

// RegistryClean is what one registry sweep did, for the admin view.
type RegistryClean struct {
	At     time.Time
	Took   time.Duration
	Manual bool
	// Deleted is the versions the mark removed (manifest and row).
	Deleted int
	// Freed is the registry volume's bytes before minus after the garbage
	// collect; when the volume could not be measured, the deleted versions'
	// sizes by their rows (Measured false).
	Freed    int64
	Measured bool
	Errors   []string
}

// registryCleanJob is the payload of kindRegistryClean; By is the user who
// pressed "Clean up now", "" for the schedule.
type registryCleanJob struct {
	By string `json:"by"`
}

// lastRegistryClean is the newest sweep's result.
// ponytail: in memory, so an admin view after a restart shows none until the
// next sweep; the security log row has it either way.
type lastRegistryClean struct {
	mu  sync.Mutex
	res RegistryClean
	ok  bool
}

// LastRegistryClean is the newest sweep's result since the panel started; ok
// is false before the first one.
func (o *Orchestrator) LastRegistryClean() (RegistryClean, bool) {
	o.regCleaned.mu.Lock()
	defer o.regCleaned.mu.Unlock()
	return o.regCleaned.res, o.regCleaned.ok
}

// CleanRegistryNow queues a sweep ("Clean up now"). It returns at once with
// the job; the result shows in LastRegistryClean when it finishes.
func (o *Orchestrator) CleanRegistryNow(ctx context.Context, by string) (Job, error) {
	return o.enqueue(ctx, kindRegistryClean, registryCleanJob{By: by}, "registry-clean")
}

// runRegistryClean is the nightly registry sweep. With the registry's gate
// shut (pushes through the front get 503 and Retry-After, imports wait):
// delete every version no env uses beyond the newest registry_keep per repo,
// then run the registry's garbage collect, which frees the disk. The result is
// kept and written to the security log.
func (o *Orchestrator) runRegistryClean(ctx context.Context, r *jobs.Run, p registryCleanJob) error {
	keep, err := o.settings.Int(ctx, "registry_keep")
	if err != nil {
		return err
	}
	res := RegistryClean{At: time.Now().UTC(), Manual: p.By != ""}
	sweepErr := o.regs.WhileGated(ctx, func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, registrySweepTimeout)
		defer cancel()
		used, err := o.registryInUse(ctx)
		if err != nil {
			return fmt.Errorf("what is in use: %w", err)
		}
		before, berr := o.registryBytes(ctx)
		system := o.systemInUse()
		c, err := o.regs.Clean(ctx, func(repo string) int {
			if strings.HasPrefix(repo, systemRepoPrefix) {
				return 0 // system decides alone, see systemInUse
			}
			return keep
		}, func(ctx context.Context, repo, digest string) (bool, error) {
			if strings.HasPrefix(repo, systemRepoPrefix) {
				return system(ctx, repo, digest)
			}
			return used[digest], nil
		})
		res.Deleted, res.Errors = c.Deleted, c.Errors
		if err != nil {
			return err
		}
		out, err := o.docker.Exec(ctx, installspec.RegistryName,
			[]string{"registry", "garbage-collect", "--delete-untagged", registryConfig})
		_, _ = fmt.Fprintf(r.Log, "garbage collect:\n%s\n", gcSummary(out))
		if err != nil {
			return fmt.Errorf("garbage collect: %w", err)
		}
		after, aerr := o.registryBytes(ctx)
		if berr == nil && aerr == nil {
			res.Freed, res.Measured = max(before-after, 0), true
		} else {
			res.Freed = c.Freed
		}
		return nil
	})
	if sweepErr != nil {
		res.Errors = append(res.Errors, sweepErr.Error())
	}
	res.Took = time.Since(res.At)
	o.regCleaned.mu.Lock()
	o.regCleaned.res, o.regCleaned.ok = res, true
	o.regCleaned.mu.Unlock()
	_, _ = fmt.Fprintf(r.Log, "%s\n", res.summary())
	if err := o.store.Tx(ctx, func(tx store.Tx) error {
		return audit(tx).Append(ctx, securitylog.Event{
			Action: securitylog.RegistryClean, ActorID: p.By, TargetKind: "registry", Detail: res.summary(),
		})
	}); err != nil {
		slog.Warn("registry sweep: security log not written", "error", err)
	}
	return sweepErr
}

// summary is the one line the log and the admin view show.
func (c RegistryClean) summary() string {
	how := "by row sizes"
	if c.Measured {
		how = "on disk"
	}
	s := fmt.Sprintf("registry sweep: %d versions deleted, %s freed (%s), %d errors, took %s",
		c.Deleted, byteSize(c.Freed), how, len(c.Errors), c.Took.Round(time.Second))
	if len(c.Errors) > 0 {
		s += ": " + strings.Join(c.Errors, "; ")
	}
	return s
}

// gcSummary is the garbage collect's own count lines, without its debug noise.
func gcSummary(out string) string {
	var keep []string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "eligible for deletion") && !strings.HasPrefix(strings.TrimSpace(l), "time=") {
			keep = append(keep, strings.TrimSpace(l))
		}
	}
	return strings.Join(keep, "\n")
}

// registryBytes is the registry volume's size, by du in the registry container.
func (o *Orchestrator) registryBytes(ctx context.Context) (int64, error) {
	out, err := o.docker.Exec(ctx, installspec.RegistryName, []string{"du", "-sk", registryDataDir})
	if err != nil {
		return 0, err
	}
	f := strings.Fields(out)
	if len(f) == 0 {
		return 0, fmt.Errorf("du answered %q", out)
	}
	kb, err := strconv.ParseInt(f[0], 10, 64)
	return kb * 1024, err
}

// registryInUse is the manifest digests the sweep must keep: what every env's
// current release, its rollback targets, any release a live job targets and
// the newest release of each stack pin (a built image by its row, a pulled one
// by its pinned digest), and what each env's tiles name now, resolved through
// the registry (an outside ref by its import repo and tag). A digest is kept
// whole: every tag on it counts, since deleting by digest drops them all.
// stackr.system repos are systemInUse's, not listed here.
// ponytail: digests match across repos; the same image in two repos keeps
// both. Key by repo too if that ever holds back real disk.
func (o *Orchestrator) registryInUse(ctx context.Context) (map[string]bool, error) {
	used := map[string]bool{}
	refs := map[string]bool{} // "repo:tag" the registry must answer for
	rels, err := o.keptReleases(ctx)
	if err != nil {
		return nil, err
	}
	for id := range rels {
		pins, err := o.releases.Pins(ctx, id)
		if err != nil {
			return nil, err
		}
		for _, p := range pins {
			if p.Digest != "" {
				used[p.Digest] = true
			}
			if p.ImageID == nil {
				continue
			}
			im, err := o.store.Images.Get(ctx, *p.ImageID)
			if err != nil {
				continue // the row is gone with its image: nothing to keep
			}
			if im.Digest != "" {
				used[im.Digest] = true
			}
			if path, ok := strings.CutPrefix(im.Ref, image.ClusterRegistry); ok {
				refs[path] = true
			}
		}
	}
	orgs, err := o.orgs.ListAll(ctx)
	if err != nil {
		return nil, err
	}
	for _, og := range orgs {
		stacks, err := o.stacks.List(ctx, og.ID)
		if err != nil {
			return nil, err
		}
		for _, s := range stacks {
			envs, err := o.envs.List(ctx, s.ID)
			if err != nil {
				return nil, err
			}
			for _, e := range envs {
				ts, err := o.tiles.List(ctx, e.ID)
				if err != nil {
					return nil, err
				}
				for _, t := range ts {
					if t.ImageRef != "" {
						repo, tag := regfront.ImportName(og.Slug, t.ImageRef)
						refs[repo+":"+tag] = true
					}
				}
			}
		}
	}
	for ref := range refs {
		i := strings.LastIndex(ref, ":")
		if i < 0 {
			continue
		}
		d, err := o.regs.Resolve(ctx, ref[:i], ref[i+1:])
		if err != nil {
			return nil, err
		}
		if d != "" {
			used[d] = true
		}
	}
	return used, nil
}

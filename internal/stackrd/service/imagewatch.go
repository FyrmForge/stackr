package service

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/cluster"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/githubapp"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/imagewatch"
	"github.com/FyrmForge/stackr/internal/stackrd/service/notify"
	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Settings keys: check cadence in minutes ("" = 5, "0" = the watch off) and
// the RFC3339 stamp of the last sweep (the janitor ticks every minute; this is
// what makes the cadence runtime-changeable).
const (
	SettingImageInterval = "image_check_interval"
	settingImageLast     = "image_check_last"
)

// defaultImageInterval is the cadence with nothing configured.
const defaultImageInterval = 5 * time.Minute

// ImageWatchService owns the registry watch: which tiles are watched, the
// image_digest baseline its three writers share, the check history, the
// cadence setting, and what an auto policy actually does when a tag moves.
//
// The last part is why it is a service and not a janitor task any more. The
// auto branch used to enqueue on the deploy engine directly and carry its own
// copy of the upper-environment rule, and it recreated a managed instance
// through the engine below DeployService — the one of six call sites that
// wrote no status afterwards, so a database the watch auto-updated sat on the
// canvas as whatever it had been before.
type ImageWatchService struct {
	Store     repo.Store
	Notifier  *notify.Notifier
	Cluster   *cluster.Cluster
	GH        *githubapp.Client
	Client    *imagewatch.Client
	Deploys   *DeployService
	Instances *ManagedInstanceService

	// in-memory error edge guard, a restart repeats one failure
	// notification per broken tile, nothing worse. Success edges are guarded
	// durably by latest_digest.
	mu      sync.Mutex
	lastErr map[string]string
}

func (w *ImageWatchService) Name() string { return "image-watch" }

// Run is one janitor tick: skip until the configured interval has passed,
// then check every watchable tile, deduping registry calls by image ref.
func (w *ImageWatchService) Run(ctx context.Context) (int64, error) {
	iv := w.imageInterval(ctx)
	if iv <= 0 {
		return 0, nil
	}
	if last, _ := w.Store.GetSetting(ctx, settingImageLast); last != "" {
		if t, err := time.Parse(time.RFC3339, last); err == nil && time.Since(t) < iv {
			return 0, nil
		}
	}
	_ = w.Store.SetSetting(ctx, settingImageLast, time.Now().UTC().Format(time.RFC3339))

	tiles, err := w.Store.ListTiles(ctx)
	if err != nil {
		return 0, err
	}
	digests := map[string]string{}
	failed := map[string]error{}
	var checked int64
	for i := range tiles {
		t := &tiles[i]
		if !imageWatchable(t) {
			continue
		}
		if ferr, bad := failed[t.ImageRef]; bad {
			w.watchError(ctx, t, ferr)
			continue
		}
		dg, ok := digests[t.ImageRef]
		if !ok {
			user, pass := w.registryCreds(ctx, t)
			var derr error
			if dg, derr = w.Client.Digest(ctx, t.ImageRef, user, pass); derr != nil {
				failed[t.ImageRef] = derr
				w.watchError(ctx, t, derr)
				continue
			}
			digests[t.ImageRef] = dg
		}
		checked++
		w.clearWatchErr(t.ID)
		w.apply(ctx, t, dg)
	}
	return checked, nil
}

// watchable: opted in, not paused, image-based, and a ref we can poll.
func imageWatchable(t *repo.Tile) bool {
	if t.UpdatePolicy != "notify" && t.UpdatePolicy != "auto" {
		return false
	}
	if t.Status == "paused" || t.IsVolume() {
		return false
	}
	if !t.IsManaged() && t.SourceType != "image" {
		return false
	}
	_, _, _, ok := imagewatch.ParseRef(t.ImageRef)
	return ok
}

// apply compares the registry's digest with the tile's state and acts on the
// change edge only, a failed auto-deploy is not retried until the registry
// moves again.
func (w *ImageWatchService) apply(ctx context.Context, t *repo.Tile, dg string) {
	if t.ImageDigest == "" {
		// First sight of a pre-existing tile: baseline from what is actually
		// running so a genuinely stale deploy still surfaces; silent either way.
		local, _ := w.Cluster.LocalDigest(ctx, t.ImageRef)
		if local == "" {
			local = dg
		}
		_ = w.Store.SetTileImageDigest(ctx, t.ID, local)
		t.ImageDigest = local
	}
	if dg == t.LatestDigest {
		return
	}
	_ = w.Store.SetTileLatestDigest(ctx, t.ID, dg)
	t.LatestDigest = dg
	if dg == t.ImageDigest {
		return // registry matches what runs (badge clears); not a new version
	}

	w.recordWatch(ctx, t, "ok", "new digest "+shortDigest(dg))
	link := "/apps/" + t.ID
	switch t.UpdatePolicy {
	case "auto":
		w.autoUpdate(ctx, t)
		w.Notifier.Push(ctx, notify.KindImageUpdate,
			"New image version: "+t.Name, "auto-updating to "+shortDigest(dg), link)
	default: // notify
		w.Notifier.Push(ctx, notify.KindImageUpdate,
			"New image version: "+t.Name, t.ImageRef+" moved to "+shortDigest(dg), link)
	}
}

// autoUpdate redeploys through whichever path owns the tile.
//
// The three branches are three genuinely different deploy paths, not three
// copies of one policy: a managed instance is recreated rather than built, a
// cron or function tile has no long-running container to replace and only
// needs the local tag refreshed, and everything else is an ordinary deploy.
// The rules about *whether* a deploy may happen — the upper-environment gate
// above all — are DeployService's, so this no longer carries its own copy.
func (w *ImageWatchService) autoUpdate(ctx context.Context, t *repo.Tile) {
	switch {
	case t.IsManaged():
		tile := *t
		go func() {
			bg := context.Background()
			// Through the instance service: it writes the row's status, which
			// this path used to skip entirely.
			if err := w.Instances.Deploy(bg, &tile); err != nil {
				w.recordWatch(bg, &tile, "error", "auto-update: "+err.Error())
				return
			}
			w.recordLocalDigest(bg, &tile)
		}()
	case t.Kind == "cron" || t.Kind == "function":
		tile := *t
		go func() {
			ctx := context.Background()
			if err := w.Cluster.PullImage(ctx, tile.ImageRef, io.Discard); err != nil {
				w.recordWatch(ctx, &tile, "error", "auto-update pull: "+err.Error())
				return
			}
			w.recordLocalDigest(ctx, &tile)
			w.recordWatch(ctx, &tile, "ok", "pulled "+shortDigest(tile.LatestDigest)+"; next run uses it")
		}()
	default:
		if _, err := w.Deploys.Trigger(ctx, t, "image-update"); err != nil {
			// A refusal is not a failure of the watch. The upper-environment
			// one is the expected case: that environment receives promotions
			// and nothing else, so a new tag there is news, not work.
			if _, refused := svcerr.IsConflict(err); refused {
				w.recordWatch(ctx, t, "ok", "new image available; "+err.Error())
				return
			}
			w.recordWatch(ctx, t, "error", "auto-update: "+err.Error())
		}
	}
}

// recordLocalDigest re-baselines image_digest from the local docker image
// for deploy paths that don't go through the engine's OnFinish hook.
func (w *ImageWatchService) recordLocalDigest(ctx context.Context, t *repo.Tile) {
	if dg, err := w.Cluster.LocalDigest(ctx, t.ImageRef); err == nil && dg != "" {
		_ = w.Store.SetTileImageDigest(ctx, t.ID, dg)
	}
}

// tileError surfaces a registry failure on the tile once per distinct message
// (the app page shows the latest watch run).
func (w *ImageWatchService) watchError(ctx context.Context, t *repo.Tile, err error) {
	msg := err.Error()
	w.mu.Lock()
	if w.lastErr == nil {
		w.lastErr = map[string]string{}
	}
	prev := w.lastErr[t.ID]
	w.lastErr[t.ID] = msg
	w.mu.Unlock()
	if prev == msg {
		return
	}
	w.recordWatch(ctx, t, "error", msg)
	w.Notifier.Push(ctx, notify.KindImageCheckFailed,
		"Image check failed: "+t.Name, msg, "/apps/"+t.ID)
}

func (w *ImageWatchService) clearWatchErr(tileID string) {
	w.mu.Lock()
	delete(w.lastErr, tileID)
	w.mu.Unlock()
}

// record writes a cron_runs row under the tile's watch ref, check history
// with retention for free, rendered on the app page.
func (w *ImageWatchService) recordWatch(ctx context.Context, t *repo.Tile, status, output string) {
	now := time.Now().UTC()
	if err := w.Store.CreateCronRun(ctx, &repo.CronRun{
		ID: uuid.New().String(), Ref: ImageWatchRef(t.ID),
		Status: status, Output: output, StartedAt: now,
		FinishedAt: sql.NullTime{Time: now, Valid: true},
	}); err != nil {
		slog.Error("image watch result not recorded", "tile", t.ID, "status", status, "error", err)
	}
}

// ImageWatchRef is the cron_runs namespace for a tile's registry checks.
func ImageWatchRef(tileID string) string { return "watch:" + tileID }

// Interval is the configured cadence in minutes, as the settings page shows
// it. Empty configuration reads as the default rather than as off.
func (w *ImageWatchService) Interval(ctx context.Context) int {
	return int(w.imageInterval(ctx) / time.Minute)
}

// SetInterval writes the cadence. Zero turns the watch off; anything that is
// not a whole non-negative number of minutes is refused rather than coerced,
// which is what a bad value used to become on its way through Atoi.
func (w *ImageWatchService) SetInterval(ctx context.Context, minutes string) error {
	n, err := strconv.Atoi(strings.TrimSpace(minutes))
	if err != nil || n < 0 {
		return svcerr.Invalidf("interval_minutes", "the check interval is a non-negative number of minutes")
	}
	return w.Store.SetSetting(ctx, SettingImageInterval, strconv.Itoa(n))
}

func (w *ImageWatchService) imageInterval(ctx context.Context) time.Duration {
	v, _ := w.Store.GetSetting(ctx, SettingImageInterval)
	if strings.TrimSpace(v) == "" {
		return defaultImageInterval
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return defaultImageInterval
	}
	return time.Duration(n) * time.Minute
}

func (w *ImageWatchService) registryCreds(ctx context.Context, t *repo.Tile) (string, string) {
	if strings.HasPrefix(t.ImageRef, "ghcr.io/") && w.GH != nil {
		if u, p := w.GH.RegistryAuth(ctx, t); u != "" {
			return u, p
		}
	}
	return "", ""
}

func shortDigest(digest string) string {
	s := strings.TrimPrefix(digest, "sha256:")
	if len(s) > 12 {
		s = s[:12]
	}
	return s
}

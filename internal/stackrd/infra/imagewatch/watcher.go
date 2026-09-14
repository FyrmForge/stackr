package imagewatch

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

	"github.com/FyrmForge/stackr/internal/stackrd/handlers/notify"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/cluster"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/deploy"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/githubapp"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/managedtiles"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Settings keys: check cadence in minutes ("" = 5, "0" = watcher off) and the
// RFC3339 stamp of the last sweep (the janitor ticks every minute; this is
// what makes the cadence runtime-changeable).
const (
	SettingInterval = "image_check_interval"
	settingLast     = "image_check_last"
)

// Watcher is the janitor task that polls registries for tiles whose
// update_policy asks for it, records new digests and applies the policy.
type Watcher struct {
	Store    repo.Store
	Notifier *notify.Notifier
	Engine   *deploy.Engine
	DB       *managedtiles.Service
	Cluster  *cluster.Cluster
	GH       *githubapp.Client
	Client   *Client

	// in-memory error edge guard, a restart repeats one failure
	// notification per broken tile, nothing worse. Success edges are guarded
	// durably by latest_digest.
	mu      sync.Mutex
	lastErr map[string]string
}

func (w *Watcher) Name() string { return "image-watch" }

// Run is one janitor tick: skip until the configured interval has passed,
// then check every watchable tile, deduping registry calls by image ref.
func (w *Watcher) Run(ctx context.Context) (int64, error) {
	iv := w.interval(ctx)
	if iv <= 0 {
		return 0, nil
	}
	if last, _ := w.Store.GetSetting(ctx, settingLast); last != "" {
		if t, err := time.Parse(time.RFC3339, last); err == nil && time.Since(t) < iv {
			return 0, nil
		}
	}
	_ = w.Store.SetSetting(ctx, settingLast, time.Now().UTC().Format(time.RFC3339))

	tiles, err := w.Store.ListTiles(ctx)
	if err != nil {
		return 0, err
	}
	digests := map[string]string{}
	failed := map[string]error{}
	var checked int64
	for i := range tiles {
		t := &tiles[i]
		if !watchable(t) {
			continue
		}
		if ferr, bad := failed[t.ImageRef]; bad {
			w.tileError(ctx, t, ferr)
			continue
		}
		dg, ok := digests[t.ImageRef]
		if !ok {
			user, pass := w.creds(ctx, t)
			var derr error
			if dg, derr = w.Client.Digest(ctx, t.ImageRef, user, pass); derr != nil {
				failed[t.ImageRef] = derr
				w.tileError(ctx, t, derr)
				continue
			}
			digests[t.ImageRef] = dg
		}
		checked++
		w.clearErr(t.ID)
		w.apply(ctx, t, dg)
	}
	return checked, nil
}

// watchable: opted in, not paused, image-based, and a ref we can poll.
func watchable(t *repo.Tile) bool {
	if t.UpdatePolicy != "notify" && t.UpdatePolicy != "auto" {
		return false
	}
	if t.Status == "paused" || t.IsVolume() {
		return false
	}
	if !t.IsManaged() && t.SourceType != "image" {
		return false
	}
	_, _, _, ok := ParseRef(t.ImageRef)
	return ok
}

// apply compares the registry's digest with the tile's state and acts on the
// change edge only, a failed auto-deploy is not retried until the registry
// moves again.
func (w *Watcher) apply(ctx context.Context, t *repo.Tile, dg string) {
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

	w.record(ctx, t, "ok", "new digest "+short(dg))
	link := "/apps/" + t.ID
	switch t.UpdatePolicy {
	case "auto":
		w.autoUpdate(ctx, t)
		w.Notifier.Push(ctx, notify.KindImageUpdate,
			"New image version: "+t.Name, "auto-updating to "+short(dg), link)
	default: // notify
		w.Notifier.Push(ctx, notify.KindImageUpdate,
			"New image version: "+t.Name, t.ImageRef+" moved to "+short(dg), link)
	}
}

// autoUpdate redeploys through whichever path owns the tile: managed tiles
// bypass the deploy engine, one-shots just need the local tag refreshed.
func (w *Watcher) autoUpdate(ctx context.Context, t *repo.Tile) {
	switch {
	case t.IsManaged():
		tile := *t
		go func() {
			if err := w.DB.Deploy(context.Background(), &tile); err != nil {
				w.record(context.Background(), &tile, "error", "auto-update: "+err.Error())
				return
			}
			w.recordLocalDigest(context.Background(), &tile)
		}()
	case t.Kind == "cron" || t.Kind == "function":
		tile := *t
		go func() {
			ctx := context.Background()
			if err := w.Cluster.PullImage(ctx, tile.ImageRef, io.Discard); err != nil {
				w.record(ctx, &tile, "error", "auto-update pull: "+err.Error())
				return
			}
			w.recordLocalDigest(ctx, &tile)
			w.record(ctx, &tile, "ok", "pulled "+short(tile.LatestDigest)+"; next run uses it")
		}()
	default:
		if _, err := w.Engine.Enqueue(ctx, t, "image-update"); err != nil {
			w.record(ctx, t, "error", "auto-update: "+err.Error())
		}
	}
}

// recordLocalDigest re-baselines image_digest from the local docker image
// for deploy paths that don't go through the engine's OnFinish hook.
func (w *Watcher) recordLocalDigest(ctx context.Context, t *repo.Tile) {
	if dg, err := w.Cluster.LocalDigest(ctx, t.ImageRef); err == nil && dg != "" {
		_ = w.Store.SetTileImageDigest(ctx, t.ID, dg)
	}
}

// tileError surfaces a registry failure on the tile once per distinct message
// (the app page shows the latest watch run).
func (w *Watcher) tileError(ctx context.Context, t *repo.Tile, err error) {
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
	w.record(ctx, t, "error", msg)
	w.Notifier.Push(ctx, notify.KindImageUpdate,
		"Image check failed: "+t.Name, msg, "/apps/"+t.ID)
}

func (w *Watcher) clearErr(tileID string) {
	w.mu.Lock()
	delete(w.lastErr, tileID)
	w.mu.Unlock()
}

// record writes a cron_runs row under the tile's watch ref, check history
// with retention for free, rendered on the app page.
func (w *Watcher) record(ctx context.Context, t *repo.Tile, status, output string) {
	now := time.Now().UTC()
	if err := w.Store.CreateCronRun(ctx, &repo.CronRun{
		ID: uuid.New().String(), Ref: WatchRef(t.ID),
		Status: status, Output: output, StartedAt: now,
		FinishedAt: sql.NullTime{Time: now, Valid: true},
	}); err != nil {
		slog.Error("image watch result not recorded", "tile", t.ID, "status", status, "error", err)
	}
}

// WatchRef is the cron_runs namespace for a tile's registry checks.
func WatchRef(tileID string) string { return "watch:" + tileID }

func (w *Watcher) interval(ctx context.Context) time.Duration {
	v, _ := w.Store.GetSetting(ctx, SettingInterval)
	if strings.TrimSpace(v) == "" {
		return 5 * time.Minute
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return 5 * time.Minute
	}
	return time.Duration(n) * time.Minute
}

func (w *Watcher) creds(ctx context.Context, t *repo.Tile) (string, string) {
	if strings.HasPrefix(t.ImageRef, "ghcr.io/") && w.GH != nil {
		if u, p := w.GH.RegistryAuth(ctx, t); u != "" {
			return u, p
		}
	}
	return "", ""
}

func short(digest string) string {
	s := strings.TrimPrefix(digest, "sha256:")
	if len(s) > 12 {
		s = s[:12]
	}
	return s
}

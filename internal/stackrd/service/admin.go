package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/swarm"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/backup"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/cluster"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Settings the upgrade writes, read back by the update page.
const (
	SettingUpgradeLatest    = "upgrade_latest"
	SettingUpgradeCheckedAt = "upgrade_checked_at"
	SettingUpgradePrevious  = "upgrade_previous_image"
	SettingUpgradeArchive   = "upgrade_archive"
)

const (
	// panelService is the swarm service install.sh creates.
	panelService = "stackr"
	panelRepo    = "ghcr.io/fyrmforge/stackr"
	relayRepo    = "ghcr.io/fyrmforge/stackr-proxyrelay"
	releasesURL  = "https://api.github.com/repos/FyrmForge/stackr/releases/latest"
)

var ErrUpgradeRunning = errors.New("upgrade already running")

// AdminService is installation-wide operations an admin triggers from the
// panel. Upgrade is the first; maintenance and the manual panel backup belong
// here too.
type AdminService struct {
	store   repo.Store
	rt      *runtime.Runtime
	clus    *cluster.Cluster
	backups *backup.Service
	dataDir string
	version string

	upgrading sync.Mutex
}

// NewAdminService creates a new admin service.
func NewAdminService(store repo.Store, rt *runtime.Runtime, clus *cluster.Cluster, backups *backup.Service, dataDir, version string) *AdminService {
	return &AdminService{store: store, rt: rt, clus: clus, backups: backups, dataDir: dataDir, version: version}
}

// Version is the running build: a release tag like v0.1.1, or "dev".
func (s *AdminService) Version() string { return s.version }

// Upgradable is false for dev builds. They have no release to compare against
// and no published image of their own to roll back to.
func (s *AdminService) Upgradable() bool { return parseTag(s.version) != nil }

// CheckUpgrade asks GitHub for the latest release and caches it in settings.
func (s *AdminService) CheckUpgrade(ctx context.Context) (string, error) {
	if !s.Upgradable() {
		return "", nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releasesURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("check for updates: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("check for updates: github answered %s", res.Status)
	}
	var rel struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(res.Body).Decode(&rel); err != nil {
		return "", fmt.Errorf("check for updates: %w", err)
	}
	if parseTag(rel.TagName) == nil {
		return "", fmt.Errorf("check for updates: unexpected release tag %q", rel.TagName)
	}
	if err := s.store.SetSetting(ctx, SettingUpgradeLatest, rel.TagName); err != nil {
		return "", err
	}
	_ = s.store.SetSetting(ctx, SettingUpgradeCheckedAt, time.Now().UTC().Format(time.RFC3339))
	return rel.TagName, nil
}

// Available returns the cached latest release when it is newer than this build.
func (s *AdminService) Available(ctx context.Context) (string, bool) {
	if !s.Upgradable() {
		return "", false
	}
	latest, _ := s.store.GetSetting(ctx, SettingUpgradeLatest)
	return latest, newer(latest, s.version)
}

// Upgrade moves the panel to the release tag: pull both images, retag the
// relay, write a restorable archive, then point the panel's own service at
// the new image. Swarm replaces this task, and the new panel's boot rolls the
// node agents to the same build.
//
// Everything that can fail runs before the service update. A new task that
// does not stay up is rolled back by swarm; the data is not, which is what the
// archive is for.
func (s *AdminService) Upgrade(ctx context.Context, tag string) error {
	if !s.upgrading.TryLock() {
		return ErrUpgradeRunning
	}
	defer s.upgrading.Unlock()
	if !s.Upgradable() {
		return errors.New("dev build, no upgrades")
	}
	if !newer(tag, s.version) {
		return fmt.Errorf("%s is not newer than %s", tag, s.version)
	}
	// The request that started this dies with the old task. A pull cut off
	// half way is a failed upgrade for no reason.
	ctx = context.WithoutCancel(ctx)

	image, relay := imageRef(panelRepo, tag), imageRef(relayRepo, tag)
	for _, ref := range []string{image, relay} {
		var out bytes.Buffer
		// The panel, its relays and its update all live on the manager.
		if err := s.clus.PullImage(ctx, ref, &out); err != nil {
			return fmt.Errorf("pull %s: %s", ref, lastLine(out.String(), err))
		}
	}
	// Port forwarding creates relays by this local tag only; skipping the
	// retag leaves them on the old relay with no error anywhere.
	if err := s.rt.TagImage(ctx, relay, runtime.ProxyRelayImage); err != nil {
		return fmt.Errorf("tag relay: %w", err)
	}

	archive := path.Join(s.dataDir, "backups", "pre-upgrade-"+s.version+".tar.gz")
	if err := s.backups.WritePanelArchiveTo(archive); err != nil {
		return fmt.Errorf("pre-upgrade backup: %w", err)
	}

	// STACKR_IMAGE is what the agent service is built from. Leaving it on the
	// old tag gives a new panel that refuses every old agent.
	prev, err := s.rt.UpdateServiceImage(ctx, panelService, image, map[string]string{"STACKR_IMAGE": image}, &swarm.UpdateConfig{
		Parallelism: 1,
		// One panel on one sqlite file: the old task has to be gone first.
		Order:         swarm.UpdateOrderStopFirst,
		FailureAction: swarm.UpdateFailureActionRollback,
		Monitor:       60 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("update service: %w", err)
	}
	// Only now: the page offers this archive as the way back, which means
	// nothing for an upgrade that never reached the swap.
	_ = s.store.SetSetting(ctx, SettingUpgradeArchive, archive)
	_ = s.store.SetSetting(ctx, SettingUpgradePrevious, prev)
	return nil
}

// imageRef maps a release tag (v0.1.1) to its image. CI tags images without
// the v.
func imageRef(repo, tag string) string {
	return repo + ":" + strings.TrimPrefix(tag, "v")
}

// parseTag reads vMAJOR.MINOR.PATCH, nil for anything else.
func parseTag(tag string) []int {
	parts := strings.Split(strings.TrimPrefix(tag, "v"), ".")
	if !strings.HasPrefix(tag, "v") || len(parts) != 3 {
		return nil
	}
	out := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nil
		}
		out[i] = n
	}
	return out
}

// newer reports whether tag a is a later release than b.
func newer(a, b string) bool {
	x, y := parseTag(a), parseTag(b)
	if x == nil || y == nil {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return x[i] > y[i]
		}
	}
	return false
}

func lastLine(out string, err error) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if l := strings.TrimSpace(lines[len(lines)-1]); l != "" {
		return l
	}
	return err.Error()
}

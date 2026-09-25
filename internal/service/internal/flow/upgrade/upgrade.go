// Package upgrade moves the panel to a newer release: check GitHub's latest
// release tag, compare, back up, hand the swap to the helper.
package upgrade

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/panel"
)

const ReleasesURL = "https://api.github.com/repos/FyrmForge/stackr/releases/latest"

type Flow struct {
	Panel   *panel.Leaf
	Version string // the running build: a release tag like v0.1.1, or "dev"
	// Archive writes the pre-upgrade panel archive and returns its path.
	Archive func(ctx context.Context, log io.Writer) (string, error)
	// Spec is the panel's container spec for image: the installer's one
	// spec, so a new env var reaches upgrades as well as fresh installs.
	Spec func(image string) docker.ContainerSpec
	URL  string // "" = ReleasesURL; tests point it at a fake

	upgrading sync.Mutex
}

// Upgradable is false for dev builds: no release to compare, no image of
// their own to go back to. Checked by every verb, so no path reaches a swap.
func (f *Flow) Upgradable() bool { return parseTag(f.Version) != nil }

// Check asks GitHub for the latest release tag. The caller stores it
// (upgrade_latest) with the time (upgrade_checked_at).
func (f *Flow) Check(ctx context.Context) (string, error) {
	if !f.Upgradable() {
		return "", nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	url := f.URL
	if url == "" {
		url = ReleasesURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
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
	return rel.TagName, nil
}

// Available: the cached tag (upgrade_latest) is newer than this build.
func (f *Flow) Available(latest string) (string, bool) {
	if !f.Upgradable() {
		return "", false
	}
	return latest, Newer(latest, f.Version)
}

// Upgrade: validate, pull, archive, then launch the helper that swaps the
// panel and gates the new one, keeping the old until it passes. Everything
// that can fail runs before the launch. Returns the archive path.
// ponytail: the caller records upgrade_archive at launch, not after the
// swap: the old panel is gone by then, and the archive restores either way.
func (f *Flow) Upgrade(ctx context.Context, tag string, log io.Writer) (string, error) {
	if !f.upgrading.TryLock() {
		return "", errs.Conflictf("an upgrade is already running")
	}
	defer f.upgrading.Unlock()
	if !f.Upgradable() {
		return "", errs.Conflictf("dev build, no upgrades")
	}
	if !Newer(tag, f.Version) {
		return "", errs.Conflictf("%s is not newer than %s", tag, f.Version)
	}
	// The request that started this dies with the old container.
	ctx = context.WithoutCancel(ctx)
	image := ImageRef(tag)
	if err := f.Panel.Pull(ctx, image, log); err != nil {
		return "", fmt.Errorf("pull %s: %w", image, err)
	}
	archive, err := f.Archive(ctx, log)
	if err != nil {
		return "", fmt.Errorf("pre-upgrade backup: %w", err)
	}
	spec := f.Spec(image)
	spec.Image = image
	// The old container still holds its name until the new one passes.
	spec.Name = "stackr-" + strings.TrimPrefix(tag, "v")
	spec.Env = append(spec.Env, "STACKR_IMAGE="+image)
	if err := f.Panel.Launch(ctx, spec); err != nil {
		return "", err
	}
	return archive, nil
}

// ImageRef maps a release tag (v0.1.1) to its image, as the installer does.
func ImageRef(tag string) string { return installspec.Image(tag) }

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

// Newer: tag a is a later release than b, field by field ("v0.10.0" >
// "v0.9.0" is what a string compare gets wrong).
func Newer(a, b string) bool {
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

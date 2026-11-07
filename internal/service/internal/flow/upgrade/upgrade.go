// Package upgrade moves the panel to a newer release: check GitHub's latest
// release tag, compare, back up, hand the swap to the helper.
package upgrade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/panel"
)

const ReleasesURL = "https://api.github.com/repos/FyrmForge/stackr/releases/latest"

// DigestURL is where a release's image digest asset sits (%s = the tag).
const DigestURL = "https://github.com/FyrmForge/stackr/releases/download/%s/" + installspec.DigestAsset

type Flow struct {
	Panel   *panel.Leaf
	Version string // the running build: a release tag like v0.1.1, or "dev"
	// Archive writes the pre-upgrade panel archive and returns its path.
	Archive func(ctx context.Context, log io.Writer) (string, error)
	// Spec is the panel's container spec for image: the installer's one
	// spec, so a new env var reaches upgrades as well as fresh installs.
	Spec func(image string) docker.ContainerSpec
	URL  string // "" = ReleasesURL; tests point it at a fake
	// DigestFmt is DigestURL ("" = DigestURL); tests point it at a fake.
	DigestFmt string
	// Docker and ProxySpec let an upgrade bring stackr-proxy along: its spec
	// for image. Nil = the proxy is left alone.
	Docker    ProxyDocker
	ProxySpec func(image string) docker.ContainerSpec
	// DataDir is the install's data dir, where the registry's socket sits;
	// "" = the registry is left alone (dev, tests).
	DataDir string
}

// ProxyDocker is the slice of the daemon the proxy recreate uses.
type ProxyDocker interface {
	List(ctx context.Context, labels map[string]string) ([]docker.Container, error)
	StopRemove(ctx context.Context, id string) error
	Rename(ctx context.Context, id, name string) error
	Stop(ctx context.Context, id string) error
	Start(ctx context.Context, id string) error
	Run(ctx context.Context, spec docker.ContainerSpec) (string, error)
}

// Upgradable is false for a build with no release tag ("dev"): no release to
// compare, no image of its own to go back to. Checked by every verb, so no path reaches a swap.
func (f *Flow) Upgradable() bool { return parseTag(f.Version) != nil }

// Check asks GitHub for the latest release tag. The caller stores it
// (upgrade_latest).
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
	// A dev tag is offered only to a build already on the dev channel.
	if parseTag(rel.TagName) == nil || isDevTag(rel.TagName) && !isDevTag(f.Version) {
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
// that can fail runs before the launch. Returns the archive path. Never two
// at once: the job takes the lock key that overlaps every job (job.All).
// ponytail: the caller records upgrade_archive at launch, not after the
// swap: the old panel is gone by then, and the archive restores either way.
func (f *Flow) Upgrade(ctx context.Context, tag string, log io.Writer) (string, error) {
	if !f.Upgradable() {
		return "", errs.Conflictf("A dev build cannot upgrade itself. Run stackr-install from the release to move this box onto it.")
	}
	if !Newer(tag, f.Version) {
		return "", errs.Conflictf("%s is not newer than %s", tag, f.Version)
	}
	// The request that started this dies with the old container.
	ctx = context.WithoutCancel(ctx)
	image := ImageRef(tag)
	// The release declares its image's digest. That is what is pulled, and
	// tagged as image so the rest of the upgrade runs it by the usual name; a
	// tag someone moved on the registry is never fetched.
	// ponytail: the digest comes from GitHub's release asset, the same trust
	// as the tag_name it came with; signing (cosign) comes later.
	dg, err := f.releaseDigest(ctx, tag)
	if err != nil {
		return "", err
	}
	if dg != "" {
		ref := installspec.DigestRef(dg)
		if err := f.Panel.Pull(ctx, ref, log); err != nil {
			return "", fmt.Errorf("pull %s: %w", ref, err)
		}
		if err := f.Panel.Tag(ctx, ref, image); err != nil {
			return "", fmt.Errorf("tag %s: %w", image, err)
		}
	} else if err := f.Panel.Pull(ctx, image, log); err != nil {
		return "", fmt.Errorf("pull %s: %w", image, err)
	}
	// The tag is resolved once, here. Everything after runs the local copy,
	// and the swap is refused if the tag points at another image by then.
	pinned, err := f.Panel.Digest(ctx, image)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", image, err)
	}
	if dg != "" && pinned != dg {
		return "", fmt.Errorf("%s is not the release's image %s on this box", image, dg)
	}
	if pinned != "" {
		_, _ = fmt.Fprintf(log, "upgrading to %s\n", pinned)
	}
	archive, err := f.Archive(ctx, log)
	if err != nil {
		return "", fmt.Errorf("pre-upgrade backup: %w", err)
	}
	if pinned != "" {
		if now, err := f.Panel.Digest(ctx, image); err != nil || now != pinned {
			return "", errs.Conflictf("%s changed on this box during the upgrade; run it again", image)
		}
	}
	// Before the panel moves: the new panel finds the admin socket mounted,
	// and stackrd re-pushes the config when it sees the proxy start (watchTick).
	if err := f.EnsureProxy(ctx, image); err != nil {
		return "", err
	}
	if err := f.EnsureRegistry(ctx); err != nil {
		return "", err
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

// releaseDigest reads the digest asset of release tag. A release without one
// is refused, except a dev tag (rig builds are never GitHub releases): that
// one pulls by tag, as the installer's dev builds do.
func (f *Flow) releaseDigest(ctx context.Context, tag string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	format := f.DigestFmt
	if format == "" {
		format = DigestURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf(format, tag), nil)
	if err != nil {
		return "", err
	}
	res, err := http.DefaultClient.Do(req)
	if err == nil {
		defer func() { _ = res.Body.Close() }()
		if res.StatusCode == http.StatusOK {
			b, rerr := io.ReadAll(io.LimitReader(res.Body, 256))
			if dg := strings.TrimSpace(string(b)); rerr == nil && installspec.ValidDigest(dg) {
				return dg, nil
			}
			err = errors.New("the digest asset is malformed")
		} else {
			err = fmt.Errorf("github answered %s", res.Status)
		}
	}
	if isDevTag(tag) {
		return "", nil
	}
	return "", errs.Conflictf("%s declares no image digest, so it is not pulled: %v", tag, err)
}

// EnsureProxy recreates stackr-proxy from image when the spec it was made
// from (installspec.LabelSpec) differs from the wanted one: a proxy from
// before the admin socket has no stamp, so it is always replaced. The named
// volume keeps certs and autosave; sites blip for seconds. A box with no
// proxy container (the installer owns creating it) is left alone.
// The swap is transactional: the old proxy is renamed aside and stopped (that
// frees the ports), and comes back if the new one will not run. A leftover
// aside from a crashed attempt is cleared or restored first.
func (f *Flow) EnsureProxy(ctx context.Context, image string) error {
	if f.Docker == nil || f.ProxySpec == nil {
		return nil
	}
	cs, err := f.Docker.List(ctx, map[string]string{installspec.LabelRole: "proxy"})
	if err != nil {
		return err
	}
	var cur, aside *docker.Container
	for i := range cs {
		switch cs[i].Name {
		case installspec.ProxyName + OldSuffix:
			aside = &cs[i]
		default:
			cur = &cs[i]
		}
	}
	if cur == nil && aside != nil { // crashed between the stop and the run
		if err := f.Docker.Rename(ctx, aside.ID, installspec.ProxyName); err != nil {
			return fmt.Errorf("restore old proxy: %w", err)
		}
		if err := f.Docker.Start(ctx, aside.ID); err != nil {
			return fmt.Errorf("restore old proxy: %w", err)
		}
		cur, aside = aside, nil
	}
	if cur == nil {
		return nil
	}
	if aside != nil {
		if err := f.Docker.StopRemove(ctx, aside.ID); err != nil {
			return fmt.Errorf("remove leftover proxy: %w", err)
		}
	}
	want := f.ProxySpec(image)
	if cur.Labels[installspec.LabelSpec] == want.Labels[installspec.LabelSpec] {
		return nil
	}
	if err := f.Docker.Rename(ctx, cur.ID, installspec.ProxyName+OldSuffix); err != nil {
		return fmt.Errorf("set old proxy aside: %w", err)
	}
	if err := f.Docker.Stop(ctx, cur.ID); err != nil {
		return f.rollback(ctx, cur.ID, want.Name, fmt.Errorf("stop old proxy: %w", err))
	}
	if _, err := f.Docker.Run(ctx, want); err != nil {
		return f.rollback(ctx, cur.ID, want.Name, fmt.Errorf("run new proxy: %w", err))
	}
	if err := f.Docker.StopRemove(ctx, cur.ID); err != nil {
		return fmt.Errorf("remove old proxy: %w", err)
	}
	return nil
}

// OldSuffix names the proxy set aside during a swap (stackr-proxy-old).
const OldSuffix = "-old"

// rollback drops whatever the failed run left under the proxy's name and
// puts the old container back.
func (f *Flow) rollback(ctx context.Context, oldID, name string, cause error) error {
	_ = f.Docker.StopRemove(ctx, name)
	if err := f.Docker.Rename(ctx, oldID, name); err != nil {
		return fmt.Errorf("%w; restore old proxy: %v", cause, err)
	}
	if err := f.Docker.Start(ctx, oldID); err != nil {
		return fmt.Errorf("%w; restart old proxy: %v", cause, err)
	}
	return cause
}

// ImageRef maps a release tag (v0.1.1) to its image, as the installer does.
func ImageRef(tag string) string { return installspec.Image(tag) }

// parseTag reads vMAJOR.MINOR.PATCH or a rig build vMAJOR.MINOR.PATCH-dev.N,
// nil for anything else. The fourth field ranks a dev build below its
// release (v0.8.10-dev.2 < v0.8.10) and above the release before it.
func parseTag(tag string) []int {
	core, dev, isDev := strings.Cut(strings.TrimPrefix(tag, "v"), "-dev.")
	parts := strings.Split(core, ".")
	if !strings.HasPrefix(tag, "v") || len(parts) != 3 {
		return nil
	}
	out := make([]int, 4)
	out[3] = math.MaxInt
	for i, p := range append(parts, dev) {
		if i == 3 && !isDev {
			break
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nil
		}
		out[i] = n
	}
	return out
}

func isDevTag(tag string) bool { return strings.Contains(tag, "-dev.") }

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

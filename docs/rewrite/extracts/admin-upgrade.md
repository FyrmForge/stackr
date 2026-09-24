# service/admin.go — the upgrade path

Source: `service/admin.go` (tests read: `service/admin_test.go`; verbs read: `handlers/web/handler/settings/upgrade.go`)
Commit: c2423f0
Taken: `CheckUpgrade`, `Available`, `Upgrade`, dev-build refusal, already-running guard, pre-upgrade archive, the image swap, `parseTag`/`newer`/`imageRef`, the three settings keys the update page reads back
Cut: the relay image pull + retag, "swarm replaces this task", "the new panel's boot rolls the node agents", `clus.PullImage`'s which-node question, `UpdateServiceImage`'s swarm service update
Cuts belong to: nothing — they go away with Swarm; the swap becomes a plain container replace in `service/internal/docker`

Target: `service/internal/flow/upgrade`. Settings values in and out as
arguments (no store call), admin-only is middleware (no auth check), Docker
calls are marked wrapper methods.

```go
// Package upgrade moves the panel to a newer release: check, compare, swap.
package upgrade

const (
	panelRepo   = "ghcr.io/fyrmforge/stackr"
	releasesURL = "https://api.github.com/repos/FyrmForge/stackr/releases/latest"
)

var ErrRunning = errors.New("upgrade already running")

type Flow struct {
	docker  Docker  // extract: docker call, wrapper method
	backups Backups // WritePanelArchiveTo(path) error
	dataDir string
	version string // the running build: a release tag like v0.1.1, or "dev"

	upgrading sync.Mutex
}

// Upgradable is false for dev builds. They have no release to compare
// against and no published image of their own to roll back to.
func (f *Flow) Upgradable() bool { return parseTag(f.version) != nil }

// Check asks GitHub for the latest release tag.
//
// extract: dropped the two store.SetSetting calls — returned instead. The
// caller writes SettingUpgradeLatest = tag and SettingUpgradeCheckedAt =
// time.Now().UTC().Format(time.RFC3339).
func (f *Flow) Check(ctx context.Context) (string, error) {
	if !f.Upgradable() {
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
	// Refused here, so nothing downstream defends against a junk tag.
	if parseTag(rel.TagName) == nil {
		return "", fmt.Errorf("check for updates: unexpected release tag %q", rel.TagName)
	}
	return rel.TagName, nil
}

// Available reports whether the cached tag is worth offering. latest is the
// stored SettingUpgradeLatest value.
//
// extract: dropped the store.GetSetting — latest is an argument now.
func (f *Flow) Available(latest string) (string, bool) {
	if !f.Upgradable() {
		return "", false
	}
	return latest, newer(latest, f.version)
}

// Upgrade moves the panel to the release tag: pull the image, write a
// restorable archive, then point the panel's own container at it.
//
// Everything that can fail runs before the swap. A container that does not
// stay up can be put back; the data cannot, which is what the archive is for.
//
// Returns the archive path, stored by the caller as SettingUpgradeArchive —
// and only on success, because the page offers that archive as the way back,
// which means nothing for an upgrade that never reached the swap. restore.sh
// moves the install back to the build inside the archive; nothing else about
// a failed attempt is kept.
func (f *Flow) Upgrade(ctx context.Context, tag string) (string, error) {
	if !f.upgrading.TryLock() {
		return "", ErrRunning
	}
	defer f.upgrading.Unlock()
	if !f.Upgradable() {
		return "", errors.New("dev build, no upgrades")
	}
	if !newer(tag, f.version) {
		return "", fmt.Errorf("%s is not newer than %s", tag, f.version)
	}
	// The request that started this dies with the old container. A pull cut
	// off half way is a failed upgrade for no reason.
	ctx = context.WithoutCancel(ctx)

	image := imageRef(panelRepo, tag)
	var out bytes.Buffer
	// extract: docker call, wrapper method. The message is the last line of
	// the pull output, not the wrapped error: "manifest unknown" is the
	// useful half.
	if err := f.docker.PullImage(ctx, image, &out); err != nil {
		return "", fmt.Errorf("pull %s: %s", image, lastLine(out.String(), err))
	}
	// extract: dropped the relay image pull and TagImage(relay,
	// ProxyRelayImage), Swarm — port-forward relays were a multi-node thing.

	archive := path.Join(f.dataDir, "backups", "pre-upgrade-"+f.version+".tar.gz")
	if err := f.backups.WritePanelArchiveTo(archive); err != nil {
		return "", fmt.Errorf("pre-upgrade backup: %w", err)
	}

	// The panel swaps itself through the same install spec the installer
	// wrote: same name, same spec, new image. STACKR_IMAGE is what every
	// child spec is built from — leaving it on the old tag gives a new panel
	// that disagrees with everything it starts.
	//
	// extract: docker call, wrapper method. Was UpdateServiceImage, a swarm
	// service update; here, stop-old / start-new on the panel container.
	if err := f.docker.UpdatePanelImage(ctx, installspec.ServiceName, image,
		map[string]string{"STACKR_IMAGE": image}); err != nil {
		return "", fmt.Errorf("update panel: %w", err)
	}
	return archive, nil
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

// newer reports whether tag a is a later release than b. Field by field, not
// lexical: "v0.10.0" > "v0.9.0" is the case a string compare gets wrong.
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
```

**The test to carry** is a table over `newer`: `v0.1.1 > v0.1.0`,
`v0.2.0 > v0.1.9`, `v0.10.0 > v0.9.0` true; equal and older false; every
unparseable input false in both directions — `"dev"`, a tag with no leading
`v`, `""`. Plus `imageRef(panelRepo, "v0.1.1")` is
`ghcr.io/fyrmforge/stackr:0.1.1`.

## Settings keys, and the verbs that read them

| key | written | read by |
|---|---|---|
| `SettingUpgradeLatest` | `Check`, on success | `Available`, the rail badge |
| `SettingUpgradeCheckedAt` | `Check`, RFC3339 UTC, best-effort | the page's "last checked" |
| `SettingUpgradeArchive` | `Upgrade`, **after** the swap only | the page's "restore" offer |

Four verbs for the surface. **page** renders at once from version, upgradable
flag and stored archive path, never waiting on GitHub. **check** is its own
request fired into that page: it keeps `Check`'s error as a displayable string
rather than failing the request, then answers with `Available` — the button,
"up to date", or the error. **run** takes the tag from the form and answers,
on success, with a fragment that polls for the new build; `ErrRunning` gets
its own message, any other error is a flash and a redirect back. **badge**
loads on every page, so it reads the cached tag only.

## Notes for the builder

- **The row said "registry tag list". The code does not do that** — it reads
  `tag_name` from GitHub's latest-release API. Listing `ghcr.io` tags is new
  work with a different auth story; the semver compare is unaffected either
  way. **DECIDE** before building.
- **`Upgradable()` is checked three times deliberately** — `Check`,
  `Available`, and again inside `Upgrade`. A dev build must not reach the
  swap by any path, a hand-posted form included.
- **`TryLock`, not `Lock`.** A second request gets a refusal the operator can
  see, not a queued swap that fires later on a page nobody is watching.
- **The order is the contract:** validate → pull → archive → swap → record.
- **The old rollback comment no longer holds.** It relied on Swarm rolling a
  failed task back; plain containers do not. If the new panel does not come
  up the operator is on `restore.sh` plus the archive, which is what the
  settings key points at. A health gate on the new panel container before the
  old one is discarded would fix that — but that is REWRITE's "Deploys"
  rollout work, not this flow's.

Size: source 301 lines, extract 230 lines

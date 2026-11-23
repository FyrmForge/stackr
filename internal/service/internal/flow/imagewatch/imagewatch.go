// Package imagewatch asks registries whether an image tile's tag moved
// (mode 1, digest) or a newer tag matches its policy (mode 2, semver), caches
// the answer on the image row and writes one release per env that takes
// releases directly. It never deploys: auto envs come back to the caller,
// which promotes them through the normal job.
package imagewatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sort"
	"strings"
	"time"

	"golang.org/x/mod/semver"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/image"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/org"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/release"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/settings"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/stack"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/registry"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

type Flow struct {
	Orgs     *org.Leaf
	Stacks   *stack.Leaf
	Envs     *environment.Leaf
	Tiles    *tile.Leaf
	Images   *image.Leaf
	Releases *release.Leaf
	Settings *settings.Leaf
	// Cred is the credential for an image ref (Orchestrator.pullCred, or
	// credential.Leaf.For); ok=false = anonymous.
	Cred func(ctx context.Context, orgID, ref string) (store.Credential, bool, error)

	// Registry calls; nil = the real registry package.
	Digest func(ctx context.Context, ref, user, pass string) (string, error)
	Tags   func(ctx context.Context, ref, user, pass string) ([]string, error)
	// Label reads the image's version label (org.opencontainers.image.version);
	// nil = the real registry package, at the panel's platform.
	Label func(ctx context.Context, ref, user, pass string) (string, error)

	// The panel's own registry, answered from its store (no HTTP, no
	// credential: an org has none for it). nil = the HTTP calls above.
	OwnDigest func(ctx context.Context, repo, tag string) (string, error)
	OwnTags   func(ctx context.Context, repo string) ([]string, error)

	// Promoting reports a promote into the env that is queued or running;
	// nil = none. An image watched in such an env is not asked this tick (so
	// nothing is recorded); once the promote lands, the base is what runs.
	Promoting func(ctx context.Context, envID string) (bool, error)

	last time.Time // the last timed sweep
}

// Scope narrows a check: "check now" per stack or per tile. Zero = all.
type Scope struct{ StackID, TileID string }

// Update is one release the sweep wrote. Auto: every tile swapped in it is
// update_policy auto, so the caller promotes it now.
type Update struct {
	EnvID, Env, ReleaseID string
	Number                int
	Auto                  bool
}

// Due reports whether the timed sweep should run now: the interval
// (minutes, 0 = off) has passed since the last one.
func (f *Flow) Due(ctx context.Context, now time.Time) (bool, error) {
	n, err := f.Settings.Int(ctx, "image_check_interval")
	if err != nil || n <= 0 {
		return false, err
	}
	if now.Sub(f.last) < time.Duration(n)*time.Minute {
		return false, nil
	}
	f.last = now
	return true, nil
}

type watched struct {
	t   store.Tile
	e   store.Environment
	org string
}

type answer struct {
	digest, tag, prev string
	version           string // the label of digest, "" = none or not read
	versionErr        string // why the label read failed, "" = fine
	err               error
	skip              bool // an env it is watched for has a promote running
}

// Check runs one sweep over scope.
func (f *Flow) Check(ctx context.Context, sc Scope, log io.Writer) ([]Update, error) {
	ws, err := f.scan(ctx, sc)
	if err != nil {
		return nil, err
	}
	// One registry call per unique image per org (credentials are per org),
	// a failure cached as well as an answer.
	answers := map[string]*answer{}
	prevs := map[string]string{} // the image row before this round, per ref
	byEnv := map[string][]watched{}
	var envOrder []string
	for _, w := range ws {
		k := w.org + "|" + w.t.ImageRef + "|" + w.t.TagPolicy
		a, ok := answers[k]
		if !ok {
			if busy, err := f.promoting(ctx, ws, k); err != nil {
				return nil, err
			} else if busy {
				answers[k] = &answer{skip: true}
				logf(log, "%s: promote running; image check waits\n", w.t.ImageRef)
				continue
			}
			if _, seen := prevs[w.t.ImageRef]; !seen {
				img, _ := f.Images.GetByRef(ctx, w.t.ImageRef)
				prevs[w.t.ImageRef] = img.LastDigest
			}
			a = f.ask(ctx, w, log)
			a.prev = prevs[w.t.ImageRef]
			answers[k] = a
			if a.err != nil {
				logf(log, "%s: %v\n", w.t.ImageRef, a.err)
			}
		}
		// Registry errors retry next round and never read as an update; an
		// unchanged answer is not news either (the chip already shows it).
		if a.skip || a.err != nil || a.digest == a.prev {
			continue
		}
		if _, seen := byEnv[w.e.ID]; !seen {
			envOrder = append(envOrder, w.e.ID)
		}
		byEnv[w.e.ID] = append(byEnv[w.e.ID], w)
	}

	var out []Update
	for _, envID := range envOrder {
		u, err := f.release(ctx, byEnv[envID], answers, log)
		if err != nil {
			return out, err
		}
		if u != nil {
			out = append(out, *u)
		}
	}
	return out, nil
}

// release writes the env's current release with the moved digests swapped,
// if any differs from what the env runs.
func (f *Flow) release(ctx context.Context, ws []watched, answers map[string]*answer, log io.Writer) (*Update, error) {
	e := ws[0].e
	if !f.direct(ctx, e) {
		return nil, nil
	}
	cur := map[string]release.Pin{}
	if e.ReleaseID != nil {
		var err error
		if cur, err = f.Releases.Pins(ctx, *e.ReleaseID); err != nil {
			return nil, err
		}
	}
	var swap []release.Pin
	auto := true
	for _, w := range ws {
		a := answers[w.org+"|"+w.t.ImageRef+"|"+w.t.TagPolicy]
		if cur[w.t.Slug].Digest == a.digest {
			continue // the registry caught up with what already runs
		}
		swap = append(swap, release.Pin{Slug: w.t.Slug, Repo: w.t.ImageRef, Digest: a.digest, Version: a.version, VersionErr: a.versionErr})
		auto = auto && w.t.UpdatePolicy == "auto"
		logf(log, "%s/%s: %s is now %s\n", e.Slug, w.t.Slug, w.t.ImageRef, short(a.digest))
	}
	if len(swap) == 0 {
		return nil, nil
	}
	// ponytail: one release per env, auto only when every swapped tile is;
	// a mixed env waits for the chip's button.
	// Build on the release that waits for this env, if any (a push, an earlier
	// watch), not on what the env runs; the digests above still compare to
	// what it runs, so a registry that caught up makes no release.
	base, err := f.Releases.Newest(ctx, e.StackID, deref(e.ReleaseID), release.Lines(e)...)
	if err != nil {
		return nil, err
	}
	r, err := f.Releases.Derive(ctx, e.StackID, base, "image-watch", release.EnvLine(e.ID), swap...)
	if err != nil {
		return nil, err
	}
	logf(log, "%s: wrote release #%d\n", e.Slug, r.Number)
	// Only a release that builds on what the env runs goes out unattended;
	// after a rollback the newest release can be the bad one.
	if auto && base != deref(e.ReleaseID) {
		auto = false
		logf(log, "%s: release #%d builds on a release the env does not run; not promoted automatically\n", e.Slug, r.Number)
	}
	return &Update{EnvID: e.ID, Env: e.Slug, ReleaseID: r.ID, Number: r.Number, Auto: auto}, nil
}

// promoting: a promote is queued or running in any env key's image is
// watched for.
func (f *Flow) promoting(ctx context.Context, ws []watched, key string) (bool, error) {
	if f.Promoting == nil {
		return false, nil
	}
	for _, w := range ws {
		if w.org+"|"+w.t.ImageRef+"|"+w.t.TagPolicy != key {
			continue
		}
		if busy, err := f.Promoting(ctx, w.e.ID); err != nil || busy {
			return busy, err
		}
	}
	return false, nil
}

// direct: a branch env or the bottom rung takes releases straight away;
// an env that promotes from below gets the release by promotion.
func (f *Flow) direct(ctx context.Context, e store.Environment) bool {
	if e.FromKind != environment.FromPromote {
		return true
	}
	_, err := f.Envs.Below(ctx, e)
	return errors.Is(err, errs.ErrNotFound)
}

// ask asks the registry and caches the answer on the image row.
func (f *Flow) ask(ctx context.Context, w watched, log io.Writer) *answer {
	a := &answer{}
	// The panel's own registry is the one private host an org may watch.
	panel, _ := f.Settings.Get(ctx, "panel_domain")
	if panel != "" {
		ctx = registry.Allowing(ctx, "registry."+installspec.CleanHost(panel))
	}
	user, pass := "", ""
	digest, tagsOf := f.digest(), f.tags()
	own := false // the panel's own registry: its images carry no label read
	if host, repo, _, ok := registry.ParseRef(w.t.ImageRef); ok && f.OwnDigest != nil && f.OwnTags != nil &&
		panel != "" && strings.EqualFold(host, "registry."+installspec.CleanHost(panel)) {
		digest = func(_ context.Context, ref, _, _ string) (string, error) {
			_, _, tag, _ := registry.ParseRef(ref)
			d, err := f.OwnDigest(ctx, repo, tag)
			if err == nil && d == "" {
				err = fmt.Errorf("%s:%s is not in the registry", repo, tag)
			}
			return d, err
		}
		tagsOf = func(context.Context, string, string, string) ([]string, error) { return f.OwnTags(ctx, repo) }
		own = true
	} else if c, ok, err := f.Cred(ctx, w.org, w.t.ImageRef); err != nil {
		a.err = err
	} else if ok {
		user, pass = c.Username, c.Password
	}
	ref := w.t.ImageRef
	if a.err == nil && w.t.TagPolicy != "" {
		var tags []string
		if tags, a.err = tagsOf(ctx, repoOf(ref), user, pass); a.err == nil {
			_, _, cur, _ := registry.ParseRef(ref)
			a.tag, a.err = Pick(w.t.TagPolicy, cur, tags)
			ref = repoOf(ref) + ":" + a.tag
		}
	}
	if a.err == nil {
		a.digest, a.err = digest(ctx, ref, user, pass)
	}
	var read bool
	var verr error
	if a.err == nil && !own {
		a.version, read, verr = f.version(ctx, w.t.ImageRef, ref, a.digest, user, pass, log)
	}
	if _, err := f.Images.Checked(ctx, w.t.ImageRef, a.digest, a.tag, a.version, a.err); err != nil && a.err == nil {
		a.err = err
	}
	if verr != nil {
		a.versionErr = verr.Error()
	}
	if read {
		_ = f.Images.LabelRead(ctx, w.t.ImageRef, verr)
	}
	return a
}

// version is the version label of ref at digest. It is read once per digest:
// the image row holds the last one seen with its label. A failed read is
// logged and reads as no label; the row then names the tag or the digest.
// read says a label read happened (not a cache hit); err is its failure.
func (f *Flow) version(ctx context.Context, row, ref, digest, user, pass string, log io.Writer) (v string, read bool, err error) {
	if img, err := f.Images.GetByRef(ctx, row); err == nil && img.LastDigest == digest {
		if img.VersionErr != "" { // the read of this digest failed; the pin says so too
			return img.LastVersion, false, errors.New(img.VersionErr)
		}
		return img.LastVersion, false, nil
	}
	readLabel := f.Label
	if readLabel == nil {
		readLabel = func(ctx context.Context, ref, user, pass string) (string, error) {
			return registry.VersionLabel(ctx, ref, user, pass, runtime.GOARCH)
		}
	}
	v, err = readLabel(ctx, ref+"@"+digest, user, pass)
	if err != nil {
		logf(log, "%s: version label could not be read: %v\n", ref, err)
		return "", true, err
	}
	return v, true, nil
}

// scan lists the image tiles in scope (the tile leaf refuses digest-pinned
// refs, so every one has a tag to watch).
func (f *Flow) scan(ctx context.Context, sc Scope) ([]watched, error) {
	var stacks []store.Stack
	if sc.TileID != "" {
		t, err := f.Tiles.Get(ctx, sc.TileID)
		if err != nil {
			return nil, err
		}
		sc.StackID = t.StackID
	}
	if sc.StackID != "" {
		st, err := f.Stacks.Get(ctx, sc.StackID)
		if err != nil {
			return nil, err
		}
		stacks = append(stacks, st)
	} else {
		orgs, err := f.Orgs.ListAll(ctx)
		if err != nil {
			return nil, err
		}
		for _, o := range orgs {
			ss, err := f.Stacks.List(ctx, o.ID)
			if err != nil {
				return nil, err
			}
			stacks = append(stacks, ss...)
		}
	}
	var out []watched
	for _, st := range stacks {
		ts, err := f.Tiles.ListByStack(ctx, st.ID)
		if err != nil {
			return nil, err
		}
		envs := map[string]store.Environment{}
		for _, t := range ts {
			if t.Kind != tile.Image || (sc.TileID != "" && t.ID != sc.TileID) {
				continue
			}
			e, ok := envs[t.EnvironmentID]
			if !ok {
				if e, err = f.Envs.Get(ctx, t.EnvironmentID); err != nil {
					return nil, err
				}
				envs[e.ID] = e
			}
			out = append(out, watched{t: t, e: e, org: st.OrgID})
		}
	}
	return out, nil
}

// Chip: the registry has something the env does not run yet.
func Chip(img store.Image, pin release.Pin) bool {
	return img.LastDigest != "" && img.LastDigest != pin.Digest
}

// Pick is mode 2: the newest tag matching the policy. Grammar (DECIDE 32):
// "[semver] <constraint>" with ^1.2 (same major, >= 1.2.0), ~1.2 (same
// minor, >= 1.2.0), 1 or 1.2 (prefix), * (any); bare "semver" means ^ the
// ref's own tag, so a watch never jumps a major unasked. Pre-releases never
// match.
func Pick(policy, current string, tags []string) (string, error) {
	c := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(policy), "semver"))
	if c == "" {
		c = "*"
		if semver.IsValid("v" + strings.TrimPrefix(current, "v")) {
			c = "^" + current
		}
	}
	match, err := registry.Constraint(c)
	if err != nil {
		return "", err
	}
	var ok []string
	byV := map[string]string{}
	for _, t := range tags {
		v := "v" + strings.TrimPrefix(t, "v")
		if !semver.IsValid(v) || semver.Prerelease(v) != "" || !match(v) {
			continue
		}
		cv := semver.Canonical(v)
		if prev, dup := byV[cv]; !dup || len(t) > len(prev) { // 1.2 vs 1.2.0: the fuller tag
			byV[cv] = t
		}
		ok = append(ok, cv)
	}
	if len(ok) == 0 {
		return "", fmt.Errorf("no tag matches %q", policy)
	}
	sort.Slice(ok, func(i, j int) bool { return semver.Compare(ok[i], ok[j]) < 0 })
	return byV[ok[len(ok)-1]], nil
}

func (f *Flow) digest() func(context.Context, string, string, string) (string, error) {
	if f.Digest != nil {
		return f.Digest
	}
	return registry.Digest
}

func (f *Flow) tags() func(context.Context, string, string, string) ([]string, error) {
	if f.Tags != nil {
		return f.Tags
	}
	return registry.Tags
}

// repoOf drops the digest and the tag (not a registry port's colon).
func repoOf(ref string) string {
	ref, _, _ = strings.Cut(ref, "@")
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		return ref[:i]
	}
	return ref
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func short(d string) string {
	return d[:min(19, len(d))]
}

func logf(w io.Writer, format string, a ...any) {
	_, _ = fmt.Fprintf(w, format, a...)
}

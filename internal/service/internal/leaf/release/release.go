// Package release owns releases: per-stack numbered snapshots of what every
// tile runs (repo, branch, commit, built image, digest), mapped by slug. A
// release is desired state; every change to what runs is a new release.
// Build the leaf on a store.Tx's tables when a release and its tiles must
// land together.
package release

import (
	"context"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

type Leaf struct {
	releases store.ReleaseStore
	tiles    store.ReleaseTileStore
}

func New(releases store.ReleaseStore, tiles store.ReleaseTileStore) *Leaf {
	return &Leaf{releases: releases, tiles: tiles}
}

// ConfigSlug is the config repo's pin: a tile slug is [a-z0-9-], so it can
// never collide with one.
const ConfigSlug = "_config"

// Pin is one tile in a release. Built tiles carry ImageID; image and
// operated tiles carry the digest (never just a tag), and an image tile's
// Repo is the ref it was pinned from, tag included; the config repo's pin
// carries only its commit.
type Pin struct {
	Slug      string  `json:"slug"`
	Repo      string  `json:"repo"`
	Branch    string  `json:"branch"`
	CommitSHA string  `json:"commit_sha"`
	ImageID   *string `json:"image_id"`
	Digest    string  `json:"digest"`
	// Version is the image's version label at Digest ("" = none read).
	Version string `json:"version"`
	// VersionErr is why the label read failed when it was cut ("" = fine).
	VersionErr string `json:"version_err,omitempty"`
}

func (l *Leaf) Get(ctx context.Context, id string) (store.Release, error) {
	r, err := l.releases.Get(ctx, id)
	if err != nil {
		return r, err
	}
	return l.withCommit(ctx, r)
}

func (l *Leaf) GetByNumber(ctx context.Context, stackID string, n int) (store.Release, error) {
	r, err := l.releases.GetByNumber(ctx, stackID, n)
	if err != nil {
		return r, err
	}
	return l.withCommit(ctx, r)
}

func (l *Leaf) withCommit(ctx context.Context, r store.Release) (store.Release, error) {
	cs, err := l.tiles.ConfigCommits(ctx, []string{r.ID})
	r.Commit = cs[r.ID]
	return r, err
}

// List is the stack's releases, newest first. Commit is the config pin's
// commit; a release without one shows none.
func (l *Leaf) List(ctx context.Context, stackID string) ([]store.Release, error) {
	rs, err := l.releases.ListByStack(ctx, stackID)
	if err != nil {
		return rs, err
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].Number > rs[j].Number })
	ids := make([]string, len(rs))
	for i := range rs {
		ids[i] = rs[i].ID
	}
	cs, err := l.tiles.ConfigCommits(ctx, ids)
	for i := range rs {
		rs[i].Commit = cs[rs[i].ID]
	}
	return rs, err
}

// ListByOrg is List for every stack of the org at once, by stack id.
func (l *Leaf) ListByOrg(ctx context.Context, orgID string) (map[string][]store.Release, error) {
	all, err := l.releases.ListByOrg(ctx, orgID)
	if err != nil {
		return nil, err
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Number > all[j].Number })
	ids := make([]string, len(all))
	for i := range all {
		ids[i] = all[i].ID
	}
	cs, err := l.tiles.ConfigCommits(ctx, ids)
	out := map[string][]store.Release{}
	for _, r := range all {
		r.Commit = cs[r.ID]
		out[r.StackID] = append(out[r.StackID], r)
	}
	return out, err
}

// SetMessage stores the first line of the commit message on r (capped), and
// returns r with it. "" is a no-op.
func (l *Leaf) SetMessage(ctx context.Context, r store.Release, msg string) (store.Release, error) {
	msg, _, _ = strings.Cut(strings.TrimSpace(msg), "\n")
	msg = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, msg)
	if rs := []rune(strings.TrimSpace(msg)); len(rs) > 200 {
		msg = string(rs[:200])
	}
	if msg == "" {
		return r, nil
	}
	r.Message = strings.TrimSpace(msg)
	return r, l.releases.Update(ctx, r)
}

// SetAuthor stores a push's commit author on r and returns r with it. Both ""
// is a no-op.
func (l *Leaf) SetAuthor(ctx context.Context, r store.Release, name, login string) (store.Release, error) {
	if name == "" && login == "" {
		return r, nil
	}
	r.AuthorName, r.AuthorLogin = name, login
	return r, l.releases.Update(ctx, r)
}

// Digest is the digest a release pins for slug: what a pulled tile runs
// (the images table only knows builds). "" when nothing is pinned.
func (l *Leaf) Digest(ctx context.Context, releaseID *string, slug string) (string, error) {
	if releaseID == nil {
		return "", nil
	}
	pins, err := l.Pins(ctx, *releaseID)
	return pins[slug].Digest, err
}

// Pins are a release's tiles, by slug.
func (l *Leaf) Pins(ctx context.Context, releaseID string) (map[string]Pin, error) {
	rts, err := l.tiles.ListByRelease(ctx, releaseID)
	out := make(map[string]Pin, len(rts))
	for _, r := range rts {
		out[r.Slug] = Pin{
			Slug:       r.Slug,
			Repo:       r.Repo,
			Branch:     r.Branch,
			CommitSHA:  r.CommitSHA,
			ImageID:    r.ImageID,
			Digest:     r.Digest,
			Version:    r.Version,
			VersionErr: r.VersionErr,
		}
	}
	return out, err
}

// Create writes release #n+1 for the stack. by is who or what made it
// ("user:<id>", "push", "image-watch").
// ponytail: the number is max+1; two creates racing on one stack hit the
// unique index and the second fails. Release-writing jobs are per stack.
func (l *Leaf) Create(ctx context.Context, stackID, by string, pins []Pin) (store.Release, error) {
	return l.create(ctx, stackID, by, "", "", pins)
}

// EnvLine and BranchLine name the line a release is cut for: an env's own
// (image watch, deploy, rebuild, repin) or the branch a push came in on.
func EnvLine(envID string) string   { return "env:" + envID }
func BranchLine(name string) string { return "branch:" + name }

// Lines are the lines a cut for env e builds on: its own, and its branch's
// for a branch env.
func Lines(e store.Environment) []string {
	if e.FromKind == "branch" { // environment.FromBranch; a leaf never imports a sibling
		return []string{EnvLine(e.ID), BranchLine(e.FromBranch)}
	}
	return []string{EnvLine(e.ID)}
}

func (l *Leaf) create(ctx context.Context, stackID, by, builtOn, line string, pins []Pin) (store.Release, error) {
	seen := map[string]bool{}
	for _, p := range pins {
		if p.Slug == "" || seen[p.Slug] {
			return store.Release{}, errs.Invalidf("tiles", "every tile appears once, by slug (%q)", p.Slug)
		}
		if p.Digest != "" && !strings.HasPrefix(p.Digest, "sha256:") {
			return store.Release{}, errs.Invalidf("digest", "%s: a release pins a sha256 digest, not %q", p.Slug, p.Digest)
		}
		seen[p.Slug] = true
	}
	rs, err := l.releases.ListByStack(ctx, stackID)
	if err != nil {
		return store.Release{}, err
	}
	n := 0
	for _, r := range rs {
		n = max(n, r.Number)
	}
	r := store.Release{
		ID:        uuid.NewString(),
		StackID:   stackID,
		Number:    n + 1,
		CreatedAt: time.Now().UTC(),
		CreatedBy: by,
	}
	if builtOn != "" {
		r.BuiltOn = &builtOn
	}
	if line != "" {
		r.Line = &line
	}
	if err := l.releases.Create(ctx, r); err != nil {
		return r, err
	}
	for _, p := range pins {
		rt := store.ReleaseTile{
			ID:         uuid.NewString(),
			ReleaseID:  r.ID,
			Slug:       p.Slug,
			Repo:       p.Repo,
			Branch:     p.Branch,
			CommitSHA:  p.CommitSHA,
			ImageID:    p.ImageID,
			Digest:     p.Digest,
			Version:    p.Version,
			VersionErr: p.VersionErr,
		}
		if err := l.tiles.Create(ctx, rt); err != nil {
			return r, err
		}
	}
	return r, nil
}

// Derive is a new release copied from base with some pins swapped or added
// (a push to one tile's repo, an image-watch bump, a per-tile rollback).
// base "" starts from nothing: a stack's first release. line (EnvLine or
// BranchLine) is stored with the base, for Newest.
func (l *Leaf) Derive(ctx context.Context, stackID, baseID, by, line string, swap ...Pin) (store.Release, error) {
	pins := map[string]Pin{}
	if baseID != "" {
		var err error
		if pins, err = l.Pins(ctx, baseID); err != nil {
			return store.Release{}, err
		}
	}
	for _, p := range swap {
		pins[p.Slug] = p
	}
	list := make([]Pin, 0, len(pins))
	for _, p := range pins {
		list = append(list, p)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Slug < list[j].Slug })
	return l.create(ctx, stackID, by, baseID, line, list)
}

// Newest is the base a new cut on a line should build on: the highest-numbered
// release cut for one of lines that was built, directly or through others, on
// runs (what the env runs); else runs itself. With runs "" (the env runs
// nothing yet) a line's own root counts as built on it. A release cut before
// migration 019 has no base or line, so it is never on a line: the first cut
// after the upgrade builds on runs, as before. After a rollback the releases
// the env left chain to what it runs again, so they count as waiting, on
// purpose: a push is the branch head, and the branch still holds what they
// built. Waits is the safety for an auto promotion.
func (l *Leaf) Newest(ctx context.Context, stackID, runs string, lines ...string) (string, error) {
	return l.newest(ctx, stackID, runs, "", lines)
}

// newest is Newest that never returns skip (the cut Carry runs after).
func (l *Leaf) newest(ctx context.Context, stackID, runs, skip string, lines []string) (string, error) {
	rs, err := l.releases.ListByStack(ctx, stackID)
	if err != nil {
		return "", err
	}
	return l.newestIn(ctx, rs, runs, skip, lines)
}

// NewestIn is Newest over a stack's releases the caller already holds, for a
// loop over envs that would otherwise read the history once each.
func (l *Leaf) NewestIn(ctx context.Context, rs []store.Release, runs string, lines ...string) (string, error) {
	return l.newestIn(ctx, rs, runs, "", lines)
}

func (l *Leaf) newestIn(ctx context.Context, rs []store.Release, runs, skip string, lines []string) (string, error) {
	byID := make(map[string]store.Release, len(rs))
	for _, r := range rs {
		byID[r.ID] = r
	}
	onLine := func(r store.Release) bool { return r.Line != nil && slices.Contains(lines, *r.Line) }
	best := store.Release{}
	for _, r := range rs {
		if !onLine(r) || r.ID == skip || r.Number <= best.Number {
			continue
		}
		if _, ok := chain(byID, r, runs); ok {
			best = r
		}
	}
	if best.ID == "" {
		return runs, nil
	}
	// A waiting release that pins what the env runs holds nothing back.
	if runs != "" {
		a, err := l.Pins(ctx, runs)
		if err != nil {
			return "", err
		}
		b, err := l.Pins(ctx, best.ID)
		if err != nil {
			return "", err
		}
		if len(Diff(a, b)) == 0 {
			return runs, nil
		}
	}
	return best.ID, nil
}

// Carry keeps a waiting release in front after a cut that moved the env: was
// is what the env ran before, now the release just cut on it. When a release
// waited on was (Newest), one more is cut on now with the waiting release's
// pins plus what the env's cut changed, and its line, creator and message; it
// is never promoted by itself. On a clash the env's fresh pin wins, unless the
// waiting release changed that tile to a different commit (newer code): then
// the waiting pin stays. Image pins carry no commit, so a fresh digest wins.
func (l *Leaf) Carry(ctx context.Context, stackID, was, now string, lines ...string) error {
	wid, err := l.newest(ctx, stackID, was, now, lines)
	if err != nil || wid == was {
		return err
	}
	w, err := l.releases.Get(ctx, wid)
	if err != nil {
		return err
	}
	var old map[string]Pin
	if was != "" {
		if old, err = l.Pins(ctx, was); err != nil {
			return err
		}
	}
	waiting, err := l.Pins(ctx, wid)
	if err != nil {
		return err
	}
	fresh, err := l.Pins(ctx, now)
	if err != nil {
		return err
	}
	theirs := map[string]bool{}
	for _, c := range Diff(old, waiting) {
		theirs[c.Slug] = true
	}
	pins := waiting
	for _, c := range Diff(old, fresh) {
		if c.Kind == "removed" {
			continue
		}
		wp := waiting[c.Slug]
		if theirs[c.Slug] && wp.CommitSHA != "" && wp.CommitSHA != fresh[c.Slug].CommitSHA {
			continue
		}
		pins[c.Slug] = fresh[c.Slug]
	}
	if len(Diff(fresh, pins)) == 0 {
		return nil // the env's cut already holds everything that waited
	}
	list := make([]Pin, 0, len(pins))
	for _, p := range pins {
		list = append(list, p)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Slug < list[j].Slug })
	r, err := l.create(ctx, stackID, w.CreatedBy, now, *w.Line, list)
	if err != nil {
		return err
	}
	r, err = l.SetMessage(ctx, r, w.Message)
	if err != nil {
		return err
	}
	_, err = l.SetAuthor(ctx, r, w.AuthorName, w.AuthorLogin)
	return err
}

// chain walks r's base chain back to runs: the releases passed (r first, runs
// itself left out) and whether it got there. A missing release or a loop ends
// it. With runs "" a lined root counts as reaching it.
func chain(byID map[string]store.Release, r store.Release, runs string) ([]store.Release, bool) {
	var path []store.Release
	for c, n := r, 0; n <= len(byID); n++ {
		if c.ID == runs {
			return path, true
		}
		path = append(path, c)
		if c.BuiltOn == nil {
			return path, runs == "" && c.Line != nil
		}
		var ok bool
		if c, ok = byID[*c.BuiltOn]; !ok {
			break
		}
	}
	return path, false
}

// Chain is chain over releases the caller holds: the releases from r back to
// runs (r first, runs left out) and whether r reaches it.
func Chain(byID map[string]store.Release, r store.Release, runs string) ([]store.Release, bool) {
	return chain(byID, r, runs)
}

// Between are the releases from base back to runs, base first and runs left
// out, and whether base chains to runs at all.
func (l *Leaf) Between(ctx context.Context, stackID, base, runs string) ([]store.Release, bool, error) {
	rs, err := l.releases.ListByStack(ctx, stackID)
	if err != nil {
		return nil, false, err
	}
	byID := make(map[string]store.Release, len(rs))
	for _, r := range rs {
		byID[r.ID] = r
	}
	b, ok := byID[base]
	if !ok {
		return nil, base == runs, nil
	}
	path, ok := chain(byID, b, runs)
	return path, ok, nil
}

// Waits reports whether a cut built on base must not be promoted by itself
// into an env that runs runs: base is not what it runs, and either the env is
// not auto or something other than a push sits between. Auto envs let quick
// pushes stack up (the second push's base is the first, which has not been
// promoted yet). A base that does not chain to runs is not held. Returns the
// release it builds on, for the log line.
func (l *Leaf) Waits(ctx context.Context, stackID, base, runs string, envAuto bool) (store.Release, bool, error) {
	if base == runs {
		return store.Release{}, false, nil
	}
	between, ok, err := l.Between(ctx, stackID, base, runs)
	if err != nil || !ok || len(between) == 0 {
		return store.Release{}, false, err
	}
	held := !envAuto || slices.ContainsFunc(between, func(r store.Release) bool { return r.CreatedBy != "push" })
	return between[0], held, nil
}

// Change is one slug that differs between two snapshots.
type Change struct {
	Slug string
	Kind string // added | removed | changed
}

// Diff compares two snapshots. A tile whose image, digest and commit match
// is unchanged, so promoting or rolling back restarts only what differs.
func Diff(from, to map[string]Pin) []Change {
	var out []Change
	for s, p := range to {
		o, ok := from[s]
		switch {
		case !ok:
			out = append(out, Change{s, "added"})
		case o.CommitSHA != p.CommitSHA || o.Digest != p.Digest || ptr(o.ImageID) != ptr(p.ImageID):
			out = append(out, Change{s, "changed"})
		}
	}
	for s := range from {
		if _, ok := to[s]; !ok {
			out = append(out, Change{s, "removed"})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out
}

// MovedOf says in a line what a release changed against the one it was built
// on, "immich-server 3.4.1 to 3.4.2"; "" when it has no base or nothing moved.
func (l *Leaf) MovedOf(ctx context.Context, id string) string {
	r, err := l.Get(ctx, id)
	if err != nil || r.BuiltOn == nil {
		return ""
	}
	from, err := l.Pins(ctx, *r.BuiltOn)
	if err != nil {
		return ""
	}
	to, err := l.Pins(ctx, r.ID)
	if err != nil {
		return ""
	}
	return Moved(from, to)
}

// CauseOf says in a line what cut release id, for the kinds that carry no
// commit: an image watch names what moved ("immich-server 3.4.1 to 3.4.2"), a
// rebuild or a promote the release it built on ("Rebuild of release 23",
// "Images updated after promoting release 21"), a deploy the tile it pinned
// ("bazarr:latest first deploy", "bazarr tag changed to 2.0"). "" for a push
// or a person, and when the base is gone.
func (l *Leaf) CauseOf(ctx context.Context, id string) string {
	r, err := l.releases.Get(ctx, id)
	if err != nil {
		return ""
	}
	if r.CreatedBy == "image-watch" {
		return l.MovedOf(ctx, id)
	}
	var base store.Release // none for the first release of a stack
	if r.BuiltOn != nil {
		if base, err = l.releases.Get(ctx, *r.BuiltOn); err != nil {
			return ""
		}
	}
	n := strconv.Itoa(base.Number)
	if base.ID == "" && r.CreatedBy != "deploy" {
		return ""
	}
	switch r.CreatedBy {
	case "rebuild":
		return "Rebuild of release " + n
	case "promote":
		return "Images updated after promoting release " + n
	case "deploy":
		from := map[string]Pin{}
		if base.ID != "" {
			if from, err = l.Pins(ctx, base.ID); err != nil {
				return ""
			}
		}
		to, err := l.Pins(ctx, r.ID)
		if err != nil {
			return ""
		}
		for _, c := range Diff(from, to) {
			a, b := from[c.Slug], to[c.Slug]
			switch {
			case c.Kind == "added" && b.Branch == "":
				return b.Repo + " first deploy"
			case c.Kind == "added":
				return c.Slug + " first deploy"
			case c.Kind == "changed" && tagOf(b.Repo) != "" && tagOf(b.Repo) != tagOf(a.Repo):
				return c.Slug + " tag changed to " + tagOf(b.Repo)
			case c.Kind == "changed":
				return c.Slug + " deployed again"
			}
		}
	}
	return ""
}

// Moved is the line of Diff's changed tiles, at most two named. Each says
// the version change (change).
func Moved(from, to map[string]Pin) string {
	var parts []string
	n := 0
	for _, c := range Diff(from, to) {
		if c.Kind != "changed" {
			continue
		}
		n++
		if n > 2 {
			continue
		}
		if ch := change(from[c.Slug], to[c.Slug]); ch != "" {
			parts = append(parts, c.Slug+" "+ch)
		}
	}
	if n > 2 {
		parts = append(parts, "and "+strconv.Itoa(n-2)+" more")
	}
	return strings.Join(parts, ", ")
}

// change is what moved in one tile, in the order of what a person knows: the
// version label ("3.4.1 to 3.4.2", or "now 3.4.2" when the old pin has none),
// else a tag that moved, else "new image" and the short digest.
func change(a, b Pin) string {
	ta, tb := tagOf(a.Repo), tagOf(b.Repo)
	switch {
	case a.Version != "" && b.Version != "" && a.Version != b.Version:
		return a.Version + " to " + b.Version
	case ta != "" && tb != "" && ta != tb:
		return ta + " to " + tb
	case b.Version != "" && b.Version != a.Version:
		return "now " + b.Version
	}
	if d := shortDigest(b); d != "" {
		return "new image " + d
	}
	return ""
}

// tagOf is the tag of an image ref, "" when it names none.
func tagOf(ref string) string {
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		return ref[i+1:]
	}
	return ""
}

// shortDigest is the first seven characters of a pin's digest ("" = none).
func shortDigest(p Pin) string {
	d := strings.TrimPrefix(p.Digest, "sha256:")
	return d[:min(7, len(d))]
}

func ptr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ImageIDs is every image any release pins, the keep list for image cleanup.
func (l *Leaf) ImageIDs(ctx context.Context) ([]string, error) { return l.tiles.ImageIDs(ctx) }

// Package release owns releases: per-stack numbered snapshots of what every
// tile runs (repo, branch, commit, built image, digest), mapped by slug. A
// release is desired state; every change to what runs is a new release.
// Build the leaf on a store.Tx's tables when a release and its tiles must
// land together.
package release

import (
	"context"
	"sort"
	"strings"
	"time"

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
// managed tiles carry the digest (never just a tag), and an image tile's
// Repo is the ref it was pinned from, tag included; the config repo's pin
// carries only its commit.
type Pin struct {
	Slug      string  `json:"slug"`
	Repo      string  `json:"repo"`
	Branch    string  `json:"branch"`
	CommitSHA string  `json:"commit_sha"`
	ImageID   *string `json:"image_id"`
	Digest    string  `json:"digest"`
}

func (l *Leaf) Get(ctx context.Context, id string) (store.Release, error) { return l.releases.Get(ctx, id) }

func (l *Leaf) GetByNumber(ctx context.Context, stackID string, n int) (store.Release, error) {
	return l.releases.GetByNumber(ctx, stackID, n)
}

// List is the stack's releases, newest first.
func (l *Leaf) List(ctx context.Context, stackID string) ([]store.Release, error) {
	rs, err := l.releases.ListByStack(ctx, stackID)
	sort.Slice(rs, func(i, j int) bool { return rs[i].Number > rs[j].Number })
	return rs, err
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
		out[r.Slug] = Pin{Slug: r.Slug, Repo: r.Repo, Branch: r.Branch, CommitSHA: r.CommitSHA, ImageID: r.ImageID, Digest: r.Digest}
	}
	return out, err
}

// Create writes release #n+1 for the stack. by is who or what made it
// ("user:<id>", "push", "image-watch").
// ponytail: the number is max+1; two creates racing on one stack hit the
// unique index and the second fails. Release-writing jobs are per stack.
func (l *Leaf) Create(ctx context.Context, stackID, by string, pins []Pin) (store.Release, error) {
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
	r := store.Release{ID: uuid.NewString(), StackID: stackID, Number: n + 1, CreatedAt: time.Now().UTC(), CreatedBy: by}
	if err := l.releases.Create(ctx, r); err != nil {
		return r, err
	}
	for _, p := range pins {
		rt := store.ReleaseTile{ID: uuid.NewString(), ReleaseID: r.ID, Slug: p.Slug, Repo: p.Repo, Branch: p.Branch,
			CommitSHA: p.CommitSHA, ImageID: p.ImageID, Digest: p.Digest}
		if err := l.tiles.Create(ctx, rt); err != nil {
			return r, err
		}
	}
	return r, nil
}

// Derive is a new release copied from base with some pins swapped or added
// (a push to one tile's repo, an image-watch bump, a per-tile rollback).
// base "" starts from nothing: a stack's first release.
func (l *Leaf) Derive(ctx context.Context, stackID, baseID, by string, swap ...Pin) (store.Release, error) {
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
	return l.Create(ctx, stackID, by, list)
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

func ptr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ImageIDs is every image any release pins, the keep list for image cleanup.
func (l *Leaf) ImageIDs(ctx context.Context) ([]string, error) { return l.tiles.ImageIDs(ctx) }

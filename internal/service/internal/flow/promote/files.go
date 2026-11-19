package promote

import (
	"context"
	"maps"
	"slices"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/release"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
)

// planFiles checks every tile's files: lines against the config repo at the
// release's commit; nothing is written at plan time (deploy writes them).
func (f *Flow) planFiles(ctx context.Context, p *Plan, w *work) error {
	if w.list == nil || w.re == nil {
		return nil
	}
	pins, err := f.D.Releases.Pins(ctx, w.rel.ID)
	if err != nil {
		return err
	}
	at := short(pins[release.ConfigSlug].CommitSHA)
	for _, name := range slices.Sorted(maps.Keys(w.re.Tiles)) {
		for _, l := range w.re.Tiles[name].Files {
			src, _, err := tile.ParseFileMount(l)
			if err != nil {
				continue // the line check owns a malformed line
			}
			if got, _ := w.list(src); len(got) == 0 {
				p.block("tile %s: files: %s is not in the repo at %s", name, src, at)
			}
		}
	}
	f.planFileChanges(ctx, p, w, pins[release.ConfigSlug].CommitSHA)
	return nil
}

// listOf turns the config Fetcher into a Lister: a folder answers a trailing
// slash with its files NUL-joined (see Orchestrator.stackFile).
func listOf(fetch Fetcher) Lister {
	if fetch == nil {
		return nil
	}
	return func(p string) ([]string, error) {
		if b, err := fetch(p + "/"); err == nil && len(b) > 0 {
			return strings.Split(string(b), "\x00"), nil
		}
		if _, err := fetch(p); err != nil {
			return nil, nil
		}
		return []string{p}, nil
	}
}

// planFileChanges: a files: source whose git object id differs between the
// config commit the env runs and the release's one is a change, and its tile
// redeploys (the file volume is only rebuilt by a deploy). A commit the clone
// lacks counts as changed. A tile the plan creates deploys anyway.
func (f *Flow) planFileChanges(ctx context.Context, p *Plan, w *work, to string) {
	if f.FileID == nil || w.e.ReleaseID == nil || to == "" {
		return
	}
	cur, pinsErr := f.D.Releases.Pins(ctx, *w.e.ReleaseID)
	from := cur[release.ConfigSlug].CommitSHA
	// a Pins error means the running commit is unknown: count it as changed
	if pinsErr == nil && (from == "" || from == to) {
		return
	}
	made := map[string]bool{}
	for _, t := range w.creates {
		made[t.Slug] = true
	}
	for _, name := range slices.Sorted(maps.Keys(w.re.Tiles)) {
		if made[name] {
			continue
		}
		for _, l := range w.re.Tiles[name].Files {
			src, _, err := tile.ParseFileMount(l)
			if err != nil {
				continue
			}
			if pinsErr == nil {
				a, errA := f.FileID(ctx, w.st, from, src)
				b, errB := f.FileID(ctx, w.st, to, src)
				if errA == nil && errB == nil && a == b {
					continue
				}
			}
			p.add(Change{Kind: "image", Tile: name, Note: "file changed: " + src})
			w.redeploy[name] = true
		}
	}
}

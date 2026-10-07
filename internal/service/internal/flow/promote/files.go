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

package promote

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/release"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Event is one push, already verified: repo as sent, the branch name, the
// head commit and the changed paths (nil = unknown, build everything).
type Event struct {
	Repo, Branch, Commit string
	Changed              []string
}

// Push turns a push into a release: the config pin moves when the push is
// to the config repo, and every service tile of an env built from that
// branch whose repo and watch paths match is built. Returns the zero
// release when nothing followed from the push, and the envs that take it
// automatically (the caller promotes into those).
func (f *Flow) Push(ctx context.Context, stackID string, ev Event, log io.Writer) (store.Release, []store.Environment, error) {
	d := f.D
	st, err := d.Stacks.Get(ctx, stackID)
	if err != nil {
		return store.Release{}, nil, err
	}
	envs, err := d.Envs.List(ctx, stackID)
	if err != nil {
		return store.Release{}, nil, err
	}
	var from []store.Environment
	for _, e := range envs {
		if e.FromKind == environment.FromBranch && e.FromBranch == ev.Branch {
			from = append(from, e)
		}
	}
	if len(from) == 0 {
		return store.Release{}, nil, nil
	}
	repo := NormalizeRepo(ev.Repo)
	isConfig := st.ConfigRepo != "" && NormalizeRepo(st.ConfigRepo) == repo
	var pins []release.Pin
	if isConfig {
		pins = append(pins, release.Pin{Slug: release.ConfigSlug, Repo: st.ConfigRepo, Branch: ev.Branch, CommitSHA: ev.Commit})
	}
	rows, err := f.candidates(ctx, st, from, ev, isConfig, log)
	if err != nil {
		return store.Release{}, nil, err
	}
	onlyConfig := isConfig && configOnly(ev.Changed, st.ConfigPath)
	for _, t := range rows {
		if onlyConfig || NormalizeRepo(t.GitURL) != repo || t.GitBranch != ev.Branch ||
			!watchMatch(tile.Lines(t.WatchPaths), ev.Changed) {
			continue
		}
		if f.Build == nil {
			return store.Release{}, nil, fmt.Errorf("promote: no builder wired")
		}
		logf(log, "building %s at %s\n", t.Slug, short(ev.Commit))
		id, err := f.Build(ctx, st, t, ev.Commit, log)
		if err != nil {
			return store.Release{}, nil, fmt.Errorf("build %s: %w", t.Slug, err)
		}
		pins = append(pins, release.Pin{Slug: t.Slug, Repo: t.GitURL, Branch: ev.Branch, CommitSHA: ev.Commit, ImageID: &id})
	}
	if len(pins) == 0 {
		return store.Release{}, nil, nil
	}
	// ponytail: the base is the first matching env's release; two envs on
	// one branch drifting apart on other pins is not reconciled here.
	base := deref(from[0].ReleaseID)
	if base == "" && from[0].BaseEnvID != nil {
		// A new PR env starts from what its base env runs.
		if be, err := d.Envs.Get(ctx, *from[0].BaseEnvID); err == nil {
			base = deref(be.ReleaseID)
		}
	}
	r, err := d.Releases.Derive(ctx, stackID, base, "push", pins...)
	if err != nil {
		return store.Release{}, nil, err
	}
	var auto []store.Environment
	for _, e := range from {
		if e.Auto {
			auto = append(auto, e)
		}
	}
	return r, auto, nil
}

// candidates are the service tiles the push may build: the envs' live rows,
// and on a config push the rows the stack file at this commit asks for (so
// a tile added in this very commit gets its first build).
func (f *Flow) candidates(ctx context.Context, st store.Stack, envs []store.Environment, ev Event, isConfig bool, log io.Writer) ([]store.Tile, error) {
	seen := map[string]bool{}
	var out []store.Tile
	keep := func(t store.Tile) {
		if t.Kind == tile.Service && !seen[t.Slug] {
			seen[t.Slug] = true
			out = append(out, t)
		}
	}
	if isConfig && f.Config != nil {
		data, fetch, err := f.Config(ctx, st, ev.Commit, log)
		if err != nil {
			return nil, err
		}
		r, err := Load(data, fetch)
		if err != nil {
			return nil, err
		}
		for _, e := range envs {
			re := r.Envs[e.Slug]
			for _, n := range sortedKeys(re.Tiles) {
				keep(toRow(n, re.Tiles[n], st, e))
			}
		}
		return out, nil
	}
	for _, e := range envs {
		ts, err := f.D.Tiles.List(ctx, e.ID)
		if err != nil {
			return nil, err
		}
		for _, t := range ts {
			keep(t)
		}
	}
	return out, nil
}

// NormalizeRepo folds the spellings of one GitHub repo to owner/name.
func NormalizeRepo(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimSuffix(strings.TrimSuffix(s, "/"), ".git")
	for _, p := range []string{"https://", "http://", "ssh://", "git://"} {
		s = strings.TrimPrefix(s, p)
	}
	s = strings.TrimPrefix(s, "git@")
	s = strings.Replace(s, ":", "/", 1)
	s = strings.TrimPrefix(s, "github.com/")
	return strings.Trim(s, "/")
}

// watchMatch: build when any changed path matches a watch regex and no
// "!" ignore. No watch list, or an unknown change list, always builds.
// ponytail: patterns compile per call; cache them if pushes get hot.
func watchMatch(watch, changed []string) bool {
	if len(watch) == 0 || len(changed) == 0 {
		return true
	}
	var in, out []*regexp.Regexp
	for _, w := range watch {
		neg := strings.HasPrefix(w, "!")
		re, err := regexp.Compile(strings.TrimPrefix(w, "!"))
		if err != nil {
			return true // tile.Validate refuses these; never skip a build over one
		}
		if neg {
			out = append(out, re)
		} else {
			in = append(in, re)
		}
	}
	for _, c := range changed {
		if (len(in) == 0 || anyMatch(in, c)) && !anyMatch(out, c) {
			return true
		}
	}
	return false
}

func anyMatch(rs []*regexp.Regexp, s string) bool {
	for _, r := range rs {
		if r.MatchString(s) {
			return true
		}
	}
	return false
}

// configOnly: every changed path is the stack file, so nothing rebuilds.
// An unknown change list is never config-only.
func configOnly(changed []string, path string) bool {
	if len(changed) == 0 {
		return false
	}
	path = strings.TrimPrefix(or(path, DefaultPath), "/")
	for _, c := range changed {
		if strings.TrimPrefix(c, "/") != path {
			return false
		}
	}
	return true
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

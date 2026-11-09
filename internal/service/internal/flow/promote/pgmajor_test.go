package promote

import (
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/release"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

func TestMajorKeyIsRefusedWhereItMeansNothing(t *testing.T) {
	for name, c := range map[string]struct{ file, want string }{
		"on an image": {tiles("    api:\n      image: x\n      major: 16\n"), "tile api: major: only an operated tile takes it"},
		"on s3":       {tiles("    s:\n      engine: s3\n      major: 16\n"), "tile s: major: only postgres takes it"},
	} {
		if _, err := Load([]byte(c.file), nil, "testorg"); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
	if _, err := Load([]byte(tiles("    db:\n      engine: postgres\n      major: 16\n")), nil, "testorg"); err != nil {
		t.Errorf("a valid major: %v", err)
	}
}

// A new postgres tile takes the file's major (the newest when it names none)
// pinned to an exact minor, and its instance records the major; an existing
// instance keeps the image it has when the file names none; an unsupported
// major blocks the plan.
func TestNewOperatedPostgresTakesMajor(t *testing.T) {
	w := newSliceWorld(t)
	rel := func(commit, file string) store.Release {
		w.files[commit] = file
		r, err := w.f.D.Releases.Create(ctx, w.infra.ID, "test", []release.Pin{
			{Slug: release.ConfigSlug, Repo: w.infra.ConfigRepo, CommitSHA: commit},
		})
		must(t, err)
		return r
	}
	staging := w.envs["staging"].ID
	add := func(extra string) string { return infraFile + extra }
	const pg16 = "    pg16:\n      engine: postgres\n      major: 16\n"
	const pgNew = "    pgnew:\n      engine: postgres\n"

	p, err := w.f.Plan(ctx, staging, rel("m0", add(pg16+"    pgbad:\n      engine: postgres\n      major: 13\n")).ID, io.Discard)
	must(t, err)
	if len(p.Blockers) == 0 || !strings.Contains(strings.Join(p.Blockers, "|"), "pgbad") {
		t.Fatalf("an unsupported major did not block: %+v", p.Blockers)
	}

	_, err = w.f.Apply(ctx, staging, rel("m1", add(pg16+pgNew)).ID, io.Discard, nil)
	if err != nil && !strings.Contains(err.Error(), "deploy ") {
		t.Fatal(err)
	}
	for slug, want := range map[string]int{"pg16": 16, "pgnew": 18} {
		tl, err := w.f.D.Tiles.GetBySlug(ctx, staging, slug)
		must(t, err)
		m, err := w.f.D.Operated.GetByTile(ctx, tl.ID)
		must(t, err)
		if m.Major != want || !strings.HasPrefix(tl.ImageRef, "postgres:"+strconv.Itoa(want)+".") {
			t.Errorf("%s: image %q, major %d", slug, tl.ImageRef, m.Major)
		}
	}

	// the file names no image: the pinned one stays
	tl, err := w.f.D.Tiles.GetBySlug(ctx, staging, "pg16")
	must(t, err)
	old := tl
	tl.ImageRef = "postgres:16.1"
	_, _, err = w.f.D.Tiles.Update(ctx, old, tl)
	must(t, err)
	p, err = w.f.Plan(ctx, staging, rel("m2", add(pg16+pgNew)).ID, io.Discard)
	must(t, err)
	if strings.Contains(kinds(p), "update:pg16") {
		t.Errorf("an omitted image moved the pin: %s", kinds(p))
	}
}

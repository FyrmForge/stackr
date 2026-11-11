package promote

import (
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/opmember"
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

// A file may not move an existing instance to another major, by major: or by
// image:, and the plan blocks before anything is written.
func TestExistingPostgresMajorChangeBlocks(t *testing.T) {
	w := newSliceWorld(t)
	staging := w.envs["staging"].ID
	w.instance(t, "staging", func(m *store.OperatedInstance) { m.Major = 17 })
	rel := func(commit, file string) store.Release {
		w.files[commit] = file
		r, err := w.f.D.Releases.Create(ctx, w.infra.ID, "test", []release.Pin{
			{Slug: release.ConfigSlug, Repo: w.infra.ConfigRepo, CommitSHA: commit},
		})
		must(t, err)
		return r
	}
	for name, extra := range map[string]string{
		"major":      "      major: 16\n",
		"image":      "      image: postgres:16.2\n",
		"no major":   "      image: postgres:latest\n",
		"both agree": "",
	} {
		p, err := w.f.Plan(ctx, staging, rel("x"+name, infraFile+extra).ID, io.Discard)
		must(t, err)
		if blocked := strings.Contains(strings.Join(p.Blockers, "|"), "pg-db"); blocked != (extra != "") {
			t.Errorf("%s: blockers %v", name, p.Blockers)
		}
	}
	p, err := w.f.Plan(ctx, staging, rel("ok", infraFile+"      major: 17\n      image: postgres:17.1\n").ID, io.Discard)
	must(t, err)
	if len(p.Blockers) != 0 {
		t.Errorf("same major blocked: %v", p.Blockers)
	}
}

// A synced clone whose source has no image of its own still records the
// major of the engine's default image.
func TestCreateInstanceWithoutImageRecordsMajor(t *testing.T) {
	w := newSliceWorld(t)
	staging := w.envs["staging"]
	tl, err := w.f.D.Tiles.Create(ctx, store.Tile{StackID: staging.StackID, EnvironmentID: staging.ID, Name: "bare", Slug: "bare", Kind: "operated"})
	must(t, err)
	must(t, w.f.createInstance(ctx, tl, "postgres"))
	m, err := w.f.D.Operated.GetByTile(ctx, tl.ID)
	must(t, err)
	if m.Major == 0 {
		t.Error("the major was lost")
	}
	if ms, err := w.f.Members.List(ctx, m.ID); err != nil || len(ms) != 1 || ms[0].Role != opmember.Primary {
		t.Errorf("a new postgres instance needs its primary member: %v %v", ms, err)
	}
}

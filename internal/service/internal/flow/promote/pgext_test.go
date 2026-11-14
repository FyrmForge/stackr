package promote

import (
	"context"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/release"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

type pgExtCall struct {
	slug          string
	image         *string
	preload, exts *[]string
}

// pgExtWorld is the slice world with a fake PGExt hook that records its calls.
type pgExtWorld struct {
	*sliceWorld
	calls []pgExtCall
}

func newPGExtWorld(t *testing.T) *pgExtWorld {
	w := &pgExtWorld{sliceWorld: newSliceWorld(t)}
	w.f.PGExt = func(_ context.Context, tl store.Tile, _ store.OperatedInstance, image *string, preload, exts *[]string, _ io.Writer) error {
		w.calls = append(w.calls, pgExtCall{tl.Slug, image, preload, exts})
		return nil
	}
	w.instance(t, "staging", func(m *store.OperatedInstance) { m.Major = 17 })
	return w
}

func (w *pgExtWorld) rel(t *testing.T, commit, file string) store.Release {
	w.files[commit] = file
	r, err := w.f.D.Releases.Create(ctx, w.infra.ID, "test", []release.Pin{
		{Slug: release.ConfigSlug, Repo: w.infra.ConfigRepo, CommitSHA: commit},
	})
	must(t, err)
	return r
}

func (w *pgExtWorld) plan(t *testing.T, commit, file string) *Plan {
	p, err := w.f.Plan(ctx, w.envs["staging"].ID, w.rel(t, commit, file).ID, io.Discard)
	must(t, err)
	return p
}

func (w *pgExtWorld) setImage(t *testing.T, ref string) {
	old, err := w.f.D.Tiles.GetBySlug(ctx, w.envs["staging"].ID, "pg-db")
	must(t, err)
	nw := old
	nw.ImageRef = ref
	_, _, err = w.f.D.Tiles.Update(ctx, old, nw)
	must(t, err)
}

func blocked(p *Plan, want string) bool { return strings.Contains(strings.Join(p.Blockers, "|"), want) }

const sliceInInfra = "    s1:\n      kind: slice\n      provision_from: infra:staging:pg-db\n"

// An official image to another official image is still a minor move: refused.
func TestPGExtOfficialMinorStillBlocked(t *testing.T) {
	w := newPGExtWorld(t)
	w.setImage(t, "postgres:17.4")
	if p := w.plan(t, "o1", infraFile+"      image: postgres:17.5\n"); !blocked(p, "operated update") {
		t.Errorf("blockers = %v", p.Blockers)
	}
	if p := w.plan(t, "o2", infraFile+"      image: postgres:17.4\n"); p.Blocked() || strings.Contains(kinds(p), "pg-db") {
		t.Errorf("same image: %s %v", kinds(p), p.Blockers)
	}
}

// A custom image is planned and applied through the hook, and the tile row
// keeps its image (the hook stores it).
func TestPGExtCustomImageGoesThroughHook(t *testing.T) {
	w := newPGExtWorld(t)
	w.setImage(t, "postgres:17.4")
	file := infraFile + "      image: postgis/postgis:17-3.5\n"
	p := w.plan(t, "i1", file)
	if p.Blocked() || !strings.Contains(kinds(p), "operated:pg-dbimagepostgis/postgis:17-3.5postgres:17.4") {
		t.Fatalf("plan = %s %v", kinds(p), p.Blockers)
	}
	if strings.Contains(kinds(p), "update:pg-db") {
		t.Errorf("the row image moved with the plan: %s", kinds(p))
	}
	_, err := w.f.Apply(ctx, w.envs["staging"].ID, w.rel(t, "i1", file).ID, io.Discard, nil)
	must(t, err)
	if len(w.calls) != 1 || w.calls[0].slug != "pg-db" || *w.calls[0].image != "postgis/postgis:17-3.5" ||
		w.calls[0].preload != nil || w.calls[0].exts != nil {
		t.Fatalf("calls = %+v", w.calls)
	}
	if tl, _ := w.f.D.Tiles.GetBySlug(ctx, w.envs["staging"].ID, "pg-db"); tl.ImageRef != "postgres:17.4" {
		t.Errorf("the row image = %q", tl.ImageRef)
	}

	// custom back to official is a hook call too
	w.setImage(t, "postgis/postgis:17-3.5")
	w.calls = nil
	if p := w.plan(t, "i2", infraFile+"      image: postgres:17.5\n"); p.Blocked() || !strings.Contains(kinds(p), "operated:pg-dbimage") {
		t.Errorf("plan = %s %v", kinds(p), p.Blockers)
	}
}

// A new instance calls the hook once with its lists; a file with no new key
// calls it never, and plans and applies as before.
func TestPGExtNewInstanceCallsHookOnce(t *testing.T) {
	w := newPGExtWorld(t)
	if _, err := w.f.Apply(ctx, w.envs["staging"].ID, w.rel(t, "n0", infraFile).ID, io.Discard, nil); err != nil {
		t.Fatal(err)
	}
	if len(w.calls) != 0 {
		t.Fatalf("a file with no new key called the hook: %+v", w.calls)
	}
	file := infraFile + "    pgnew:\n      engine: postgres\n      preload: [pg_cron]\n      extensions: [vector, pg_cron]\n"
	p := w.plan(t, "n1", file)
	if p.Blocked() || !strings.Contains(kinds(p), "operated:pgnewpreload+pg_cron") || !strings.Contains(kinds(p), "operated:pgnewextensions+pg_cron +vector") {
		t.Fatalf("plan = %s %v", kinds(p), p.Blockers)
	}
	// the fake docker cannot roll an instance out; the hook has run by then
	_, err := w.f.Apply(ctx, w.envs["staging"].ID, w.rel(t, "n1", file).ID, io.Discard, nil)
	if err != nil && !strings.Contains(err.Error(), "deploy ") {
		t.Fatal(err)
	}
	if len(w.calls) != 1 || w.calls[0].slug != "pgnew" || w.calls[0].image != nil ||
		!slices.Equal(*w.calls[0].preload, []string{"pg_cron"}) || !slices.Equal(*w.calls[0].exts, []string{"vector", "pg_cron"}) {
		t.Fatalf("calls = %+v", w.calls)
	}
}

// An existing instance: a changed list is a row and a hook call; an unchanged
// or omitted list is neither.
func TestPGExtListsPlanAndApply(t *testing.T) {
	w := newPGExtWorld(t)
	w.instance(t, "staging", func(m *store.OperatedInstance) { m.Extensions = store.NameList{"vector"} })
	if p := w.plan(t, "l1", infraFile); strings.Contains(kinds(p), "pg-db") {
		t.Errorf("omitted lists moved: %s", kinds(p))
	}
	if p := w.plan(t, "l2", infraFile+"      extensions: [vector]\n"); strings.Contains(kinds(p), "pg-db") {
		t.Errorf("unchanged list moved: %s", kinds(p))
	}
	file := infraFile + "      extensions: [vector, postgis]\n      preload: [pg_cron]\n"
	p := w.plan(t, "l3", file)
	if p.Blocked() || !strings.Contains(kinds(p), "operated:pg-dbextensions+postgis") {
		t.Fatalf("plan = %s %v", kinds(p), p.Blockers)
	}
	if !strings.Contains(strings.Join(p.Warnings, "|"), "pg-db restarts. A startup module runs in every database") {
		t.Errorf("warnings = %v", p.Warnings)
	}
	if _, err := w.f.Apply(ctx, w.envs["staging"].ID, w.rel(t, "l3", file).ID, io.Discard, nil); err != nil {
		t.Fatal(err)
	}
	if len(w.calls) != 1 || !slices.Equal(*w.calls[0].exts, []string{"vector", "postgis"}) || !slices.Equal(*w.calls[0].preload, []string{"pg_cron"}) {
		t.Fatalf("calls = %+v", w.calls)
	}
	// a hook refusal fails the apply
	w.f.PGExt = func(context.Context, store.Tile, store.OperatedInstance, *string, *[]string, *[]string, io.Writer) error {
		return io.ErrUnexpectedEOF
	}
	if _, err := w.f.Apply(ctx, w.envs["staging"].ID, w.rel(t, "l3", file).ID, io.Discard, nil); err == nil || !strings.Contains(err.Error(), "pg-db: extensions:") {
		t.Errorf("apply = %v", err)
	}
}

func TestPGExtBlockers(t *testing.T) {
	t.Run("11 no extensions on s3", func(t *testing.T) {
		w := newPGExtWorld(t)
		p := w.plan(t, "b11", infraFile+"    store:\n      engine: s3\n      extensions: [vector]\n")
		if !blocked(p, "s3 has no extensions; only postgres does") {
			t.Errorf("blockers = %v", p.Blockers)
		}
	})
	t.Run("9 and 10 bad names", func(t *testing.T) {
		w := newPGExtWorld(t)
		if p := w.plan(t, "b9", infraFile+"      extensions: [\"Bad;name\"]\n"); !blocked(p, `"Bad;name" is not an extension name`) {
			t.Errorf("blockers = %v", p.Blockers)
		}
		if p := w.plan(t, "b10", infraFile+"      preload: [pg_stat_statements]\n"); !blocked(p, "pg_stat_statements is always loaded") {
			t.Errorf("blockers = %v", p.Blockers)
		}
	})
	t.Run("2b custom image major", func(t *testing.T) {
		w := newPGExtWorld(t)
		w.setImage(t, "postgis/postgis:17-3.5")
		const want = "runs a custom image; its major cannot change. Make a new instance on postgres 18 and branch the slices to it"
		if p := w.plan(t, "b2", infraFile+"      major: 18\n"); !blocked(p, want) {
			t.Errorf("blockers = %v", p.Blockers)
		}
		// an official image with a startup module is the same
		w.setImage(t, "postgres:17.4")
		w.instance(t, "staging", func(m *store.OperatedInstance) { m.Preload = store.NameList{"pg_cron"} })
		if p := w.plan(t, "b2b", infraFile+"      major: 18\n"); !blocked(p, want) {
			t.Errorf("blockers = %v", p.Blockers)
		}
		// no startup module, official: the old text
		w.instance(t, "staging", func(m *store.OperatedInstance) { m.Preload = nil })
		if p := w.plan(t, "b2c", infraFile+"      major: 18\n"); !blocked(p, "stackr operated upgrade") {
			t.Errorf("blockers = %v", p.Blockers)
		}
	})
	t.Run("3 slice lists a name the instance does not", func(t *testing.T) {
		w := newPGExtWorld(t)
		// the instance is in the file
		in := infraFile + "      extensions: [vector]\n" + sliceInInfra + "      extensions: [postgis]\n"
		if p := w.plan(t, "b3", in); !blocked(p, "slice s1: postgis is not enabled on pg-db. Add it to the instance's extensions first.") {
			t.Errorf("blockers = %v", p.Blockers)
		}
		in = infraFile + "      extensions: [vector, postgis]\n" + sliceInInfra + "      extensions: [postgis]\n"
		if p := w.plan(t, "b3ok", in); blocked(p, "not enabled") {
			t.Errorf("blockers = %v", p.Blockers)
		}
		// the instance is read from its row
		shop := strings.Replace(sliceShopFile, "      default_access: write\n", "      default_access: write\n      extensions: [vector]\n", 1)
		plan := func(commit string) *Plan {
			p, err := w.f.Plan(ctx, w.dev.ID, w.shopRelease(t, commit, shop).ID, io.Discard)
			must(t, err)
			return p
		}
		if p := plan("b3s"); !blocked(p, "slice api-db: vector is not enabled on pg-db") {
			t.Errorf("blockers = %v", p.Blockers)
		}
		w.instance(t, "staging", func(m *store.OperatedInstance) { m.Extensions = store.NameList{"vector"} })
		p := plan("b3t")
		if blocked(p, "not enabled") || !strings.Contains(kinds(p), "extensions vector") {
			t.Errorf("plan = %s %v", kinds(p), p.Blockers)
		}
	})
	t.Run("5 a name leaves the list while a slice lists it", func(t *testing.T) {
		w := newPGExtWorld(t)
		w.instance(t, "staging", func(m *store.OperatedInstance) { m.Extensions = store.NameList{"vector", "postgis"} })
		// a slice in the file
		in := infraFile + "      extensions: [postgis]\n" + sliceInInfra + "      extensions: [vector]\n"
		if p := w.plan(t, "b5", in); !blocked(p, "vector is listed by slice s1. Take it off the slice first.") {
			t.Errorf("blockers = %v", p.Blockers)
		}
		// the same file takes it off the slice too
		in = infraFile + "      extensions: [postgis]\n" + sliceInInfra + "      extensions: []\n"
		if p := w.plan(t, "b5ok", in); blocked(p, "is listed by slice") {
			t.Errorf("blockers = %v", p.Blockers)
		}
		// a live slice row outside the file
		m, err := w.f.D.Operated.GetByTile(ctx, w.pg["staging"].ID)
		must(t, err)
		from, acc, keep, url := "infra:staging:pg-db", "write", "keep", "direct"
		sl, err := w.f.D.Tiles.Create(ctx, store.Tile{
			StackID: w.st.ID, EnvironmentID: w.dev.ID, Name: "live", Slug: "live", Kind: "slice",
			ProvisionFrom: &from, DefaultAccess: &acc, OnRemove: &keep, SliceURL: &url,
			Extensions: store.NameList{"vector"},
		})
		must(t, err)
		must(t, w.s.Provisions.Create(ctx, store.Provision{ID: uuid.NewString(), TileID: sl.ID, InstanceID: m.ID, DBName: "live", DBUser: "live"}))
		p := w.plan(t, "b5l", infraFile+"      extensions: [postgis]\n")
		if !blocked(p, "vector is listed by slice live. Take it off the slice first.") {
			t.Errorf("blockers = %v", p.Blockers)
		}
		// several: the count shows
		in = infraFile + "      extensions: [postgis]\n" + sliceInInfra + "      extensions: [vector]\n"
		if p := w.plan(t, "b5n", in); !blocked(p, "vector is listed by slice live and 1 more.") {
			t.Errorf("blockers = %v", p.Blockers)
		}
	})
	t.Run("1 standby", func(t *testing.T) {
		w := newPGExtWorld(t)
		w.f.StandbyNames = func(context.Context, store.Tile) ([]Standby, error) { return []Standby{{Node: "w1"}}, nil }
		const want = "pg-db has a standby. Remove the standby, change the image or the startup modules, then add the standby again."
		if p := w.plan(t, "b1", infraFile+"      image: postgis/postgis:17-3.5\n"); !blocked(p, want) {
			t.Errorf("custom image: %v", p.Blockers)
		}
		if p := w.plan(t, "b1p", infraFile+"      preload: [pg_cron]\n"); !blocked(p, want) {
			t.Errorf("preload: %v", p.Blockers)
		}
		// extensions alone are no startup change
		if p := w.plan(t, "b1e", infraFile+"      extensions: [vector]\n"); blocked(p, "standby") {
			t.Errorf("extensions only: %v", p.Blockers)
		}
	})
}

// A file's own settings are untouched by none of the new keys, and the keys
// are refused where they mean nothing.
func TestPGExtKeysAreRefusedWhereTheyMeanNothing(t *testing.T) {
	for name, c := range map[string]struct{ file, want string }{
		"preload on an image":    {tiles("    api:\n      image: x\n      preload: [pg_cron]\n"), "tile api: preload: only an operated tile takes it"},
		"preload on a slice":     {tiles("    s:\n      kind: slice\n      provision_from: a:b:c\n      preload: [pg_cron]\n"), "tile s: preload: only an operated tile takes it"},
		"extensions on an image": {tiles("    api:\n      image: x\n      extensions: [vector]\n"), "tile api: extensions: only an operated or slice tile takes it"},
	} {
		if _, err := Load([]byte(c.file), nil, "testorg"); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
	if _, err := Load([]byte(tiles("    s:\n      kind: slice\n      provision_from: a:b:c\n      extensions: [vector]\n")), nil, "testorg"); err != nil {
		t.Errorf("a slice's extensions: %v", err)
	}
}

// Export writes preload and extensions, and the file plans clean again.
func TestPGExtExportRoundTrip(t *testing.T) {
	w := newPGExtWorld(t)
	w.instance(t, "staging", func(m *store.OperatedInstance) {
		m.Preload, m.Extensions = store.NameList{"pg_cron"}, store.NameList{"vector", "pg_cron"}
	})
	from, acc, keep, url := "infra:staging:pg-db", "write", "keep", "direct"
	_, err := w.f.D.Tiles.Create(ctx, store.Tile{
		StackID: w.infra.ID, EnvironmentID: w.envs["staging"].ID, Name: "s1", Slug: "s1", Kind: "slice",
		ProvisionFrom: &from, DefaultAccess: &acc, OnRemove: &keep, SliceURL: &url,
		Extensions: store.NameList{"vector"},
	})
	must(t, err)
	out, _, err := w.f.Export(ctx, w.infra.ID, w.envs["staging"].ID)
	must(t, err)
	for _, want := range []string{"preload:\n", "- pg_cron", "extensions:\n", "- vector"} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("export lacks %q:\n%s", want, out)
		}
	}
	r, err := Load(out, nil, "testorg")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	tc := firstEnv(r).Tiles
	if !slices.Equal(tc["pg-db"].Preload, []string{"pg_cron"}) || !slices.Equal(tc["pg-db"].Extensions, []string{"vector", "pg_cron"}) ||
		!slices.Equal(tc["s1"].Extensions, []string{"vector"}) {
		t.Errorf("tiles = %+v", tc)
	}
	if p := w.plan(t, "rt", string(out)); strings.Contains(kinds(p), "pg-db") || strings.Contains(kinds(p), "s1") || p.Blocked() {
		t.Errorf("plan of the export = %s %v", kinds(p), p.Blockers)
	}
}

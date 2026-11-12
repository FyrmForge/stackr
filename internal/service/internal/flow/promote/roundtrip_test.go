package promote

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/release"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

func firstEnv(r *Resolved) ResolvedEnv {
	for _, e := range r.Envs {
		return e
	}
	return ResolvedEnv{}
}

// A standbys entry is a node or {node, sync}; a bad mode is refused at load.
func TestStandbyEntryForms(t *testing.T) {
	r, err := Load([]byte(tiles("    db:\n      engine: postgres\n      standbys:\n        - w1\n        - {node: w2, sync: always}\n")), nil, "testorg")
	must(t, err)
	got := *firstEnv(r).Tiles["db"].Standbys
	if !slices.Equal(got, []Standby{{Node: "w1"}, {Node: "w2", Sync: "always"}}) {
		t.Errorf("standbys = %+v", got)
	}
	_, err = Load([]byte(tiles("    db:\n      engine: postgres\n      standbys: [{node: w1, sync: sometimes}]\n")), nil, "testorg")
	if err == nil || !strings.Contains(err.Error(), "sync is auto, always or never") {
		t.Errorf("bad mode: %v", err)
	}
}

// One apply creates the instance and then its standbys, each with its mode.
func TestStandbysOnNewInstance(t *testing.T) {
	w := newSliceWorld(t)
	var set []StandbyDiff
	var seenID []string
	w.f.StandbyPlan = func(_ context.Context, tl store.Tile, want []Standby) (StandbyDiff, error) {
		d := StandbyDiff{Add: want, Text: "+" + want[0].Node}
		return d, nil
	}
	w.f.SetStandbys = func(_ context.Context, tl store.Tile, d StandbyDiff) error {
		seenID = append(seenID, tl.ID)
		set = append(set, d)
		return nil
	}
	file := infraFile + "    pgnew:\n      engine: postgres\n      standbys:\n        - w1\n        - {node: w2, sync: always}\n"
	w.files["n1"] = file
	rel, err := w.f.D.Releases.Create(ctx, w.infra.ID, "test", []release.Pin{{Slug: release.ConfigSlug, Repo: w.infra.ConfigRepo, CommitSHA: "n1"}})
	must(t, err)
	staging := w.envs["staging"].ID
	p, err := w.f.Plan(ctx, staging, rel.ID, io.Discard)
	must(t, err)
	if p.Blocked() || !strings.Contains(kinds(p), "operated:pgnewstandbys") {
		t.Fatalf("plan = %s %v", kinds(p), p.Blockers)
	}
	// the fake docker cannot roll an instance out, so the apply stops there;
	// the standbys have to wait for a running primary and are not added yet
	if _, err := w.f.Apply(ctx, staging, rel.ID, io.Discard, nil); err == nil || !strings.Contains(err.Error(), "deploy pgnew") {
		t.Fatalf("apply = %v", err)
	} else {
		t.Log(err)
	}
	if len(set) != 0 {
		t.Fatalf("standbys added before the primary ran: %+v", set)
	}
	// what apply does once the rollout is through
	wk := &work{newStandbys: map[string]StandbyDiff{"pgnew": {Add: []Standby{{Node: "w1"}, {Node: "w2", Sync: "always"}}}}}
	must(t, w.f.applyNewStandbys(ctx, wk, w.envs["staging"]))
	if len(set) != 1 || seenID[0] == "" || len(set[0].Add) != 2 || set[0].Add[1].Sync != "always" {
		t.Errorf("SetStandbys = %+v ids %v", set, seenID)
	}
}

// Export writes major: and no image: for operated postgres, so a minor moved
// by `operated update` is not rolled back; the file plans clean; an image: that
// would move the minor is refused.
func TestExportOperatedPostgresMajor(t *testing.T) {
	w := newSliceWorld(t)
	staging := w.envs["staging"].ID
	old, err := w.f.D.Tiles.GetBySlug(ctx, staging, "pg-db")
	must(t, err)
	nw := old
	nw.ImageRef = "postgres:17.4"
	_, _, err = w.f.D.Tiles.Update(ctx, old, nw)
	must(t, err)
	w.instance(t, "staging", func(m *store.OperatedInstance) { m.Major = 17 })

	out, _, err := w.f.Export(ctx, w.infra.ID, staging)
	must(t, err)
	if !strings.Contains(string(out), "major: 17") || strings.Contains(string(out), "image:") {
		t.Fatalf("export:\n%s", out)
	}
	rel := func(commit, file string) store.Release {
		w.files[commit] = file
		r, err := w.f.D.Releases.Create(ctx, w.infra.ID, "test", []release.Pin{{Slug: release.ConfigSlug, Repo: w.infra.ConfigRepo, CommitSHA: commit}})
		must(t, err)
		return r
	}
	p, err := w.f.Plan(ctx, staging, rel("r1", string(out)).ID, io.Discard)
	must(t, err)
	if len(p.Changes) != 0 || p.Blocked() {
		t.Errorf("plan of the export = %s %v", kinds(p), p.Blockers)
	}
	p, err = w.f.Plan(ctx, staging, rel("r2", infraFile+"      image: postgres:17.5\n").ID, io.Discard)
	must(t, err)
	if !strings.Contains(strings.Join(p.Blockers, "|"), "operated update") {
		t.Errorf("a minor change by image: was not refused: %v", p.Blockers)
	}
	p, err = w.f.Plan(ctx, staging, rel("r3", infraFile+"      image: postgres:17.4\n").ID, io.Discard)
	must(t, err)
	if p.Blocked() {
		t.Errorf("the same image blocked: %v", p.Blockers)
	}
}

// Export writes standbys with their sync mode in the map form only when it is
// not auto, and version_days: 0 stays.
func TestExportStandbyModeAndVersionDays(t *testing.T) {
	w := newSliceWorld(t)
	staging := w.envs["staging"].ID
	w.f.StandbyNames = func(context.Context, store.Tile) ([]Standby, error) {
		return []Standby{{Node: "w1"}, {Node: "w2", Sync: "always"}}, nil
	}
	out, _, err := w.f.Export(ctx, w.infra.ID, staging)
	must(t, err)
	r, err := Load(out, nil, "testorg")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got := *firstEnv(r).Tiles["pg-db"].Standbys; !slices.Equal(got, []Standby{{Node: "w1"}, {Node: "w2", Sync: "always"}}) {
		t.Errorf("standbys = %+v\n%s", got, out)
	}
	if !strings.Contains(string(out), "- w1\n") {
		t.Errorf("auto standby not a plain string:\n%s", out)
	}

	_, err = w.f.Apply(ctx, w.dev.ID, w.shopRelease(t, "v1", strings.Replace(sliceFile(t, false, false),
		"      default_access: write\n", "      default_access: write\n      version_days: 0\n", 1)).ID, io.Discard, nil)
	must(t, err)
	out, _, err = w.f.Export(ctx, w.st.ID, w.dev.ID)
	must(t, err)
	if !strings.Contains(string(out), "version_days: 0") {
		t.Errorf("version_days: 0 dropped:\n%s", out)
	}
}

// A custom postgres image (postgis) exports as image:, not major:, so a new
// env does not get the stock image; the same image on an existing instance
// plans clean.
func TestExportCustomPostgresImage(t *testing.T) {
	w := newSliceWorld(t)
	staging := w.envs["staging"].ID
	old, err := w.f.D.Tiles.GetBySlug(ctx, staging, "pg-db")
	must(t, err)
	nw := old
	nw.ImageRef = "postgis/postgis:16-3.4"
	_, _, err = w.f.D.Tiles.Update(ctx, old, nw)
	must(t, err)
	w.instance(t, "staging", func(m *store.OperatedInstance) { m.Major = 16 })

	out, _, err := w.f.Export(ctx, w.infra.ID, staging)
	must(t, err)
	if !strings.Contains(string(out), "image: postgis/postgis:16-3.4") || strings.Contains(string(out), "major:") {
		t.Fatalf("export:\n%s", out)
	}
	w.files["c1"] = string(out)
	r, err := w.f.D.Releases.Create(ctx, w.infra.ID, "test", []release.Pin{{Slug: release.ConfigSlug, Repo: w.infra.ConfigRepo, CommitSHA: "c1"}})
	must(t, err)
	p, err := w.f.Plan(ctx, staging, r.ID, io.Discard)
	must(t, err)
	if len(p.Changes) != 0 || p.Blocked() {
		t.Errorf("plan of the export = %s %v", kinds(p), p.Blockers)
	}
}

// The instance row and its primary member are written in one transaction.
func TestCreateInstanceIsOneTransaction(t *testing.T) {
	w := newSliceWorld(t)
	staging := w.envs["staging"]
	tl, err := w.f.D.Tiles.Create(ctx, store.Tile{StackID: staging.StackID, EnvironmentID: staging.ID, Name: "atomic", Slug: "atomic", Kind: "operated"})
	must(t, err)
	w.f.Tx = func(ctx context.Context, fn func(store.Tx) error) error {
		return w.s.Tx(ctx, func(tx store.Tx) error {
			must(t, fn(tx))
			return errors.New("rolled back")
		})
	}
	if err := w.f.createInstance(ctx, tl, "postgres"); err == nil {
		t.Fatal("the rollback did not surface")
	}
	if _, err := w.f.D.Operated.GetByTile(ctx, tl.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("an instance row outlived its rolled back transaction: %v", err)
	}
}

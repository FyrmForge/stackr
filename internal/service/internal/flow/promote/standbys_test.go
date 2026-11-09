package promote

import (
	"context"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/release"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// standbys: on an operated postgres tile: omitted leaves them, a list is a
// plan row for what differs and a SetStandbys call on apply, an empty list
// removes them all, and the primary's own node is refused at plan (R4).
func TestOperatedStandbys(t *testing.T) {
	w := newSliceWorld(t)
	have := []string{}
	var set [][]string
	w.f.StandbyPlan = func(_ context.Context, _ store.Tile, want []string) ([]string, []string, error) {
		if slices.Contains(want, "primary") {
			return nil, nil, errs.Conflictf("primary is the primary's node: a standby runs on another")
		}
		return have, slices.Sorted(slices.Values(want)), nil
	}
	w.f.SetStandbys = func(_ context.Context, _ store.Tile, nodes []string) error {
		set = append(set, nodes)
		return nil
	}
	rel := func(commit, file string) store.Release {
		w.files[commit] = file
		r, err := w.f.D.Releases.Create(ctx, w.infra.ID, "test", []release.Pin{
			{Slug: release.ConfigSlug, Repo: w.infra.ConfigRepo, CommitSHA: commit},
		})
		must(t, err)
		return r
	}
	staging := w.envs["staging"].ID

	p, err := w.f.Plan(ctx, staging, rel("b0", infraFile).ID, io.Discard)
	must(t, err)
	if strings.Contains(kinds(p), "standbys") {
		t.Errorf("omitted standbys moved: %s", kinds(p))
	}
	on := infraFile + "      standbys: [w1, w2]\n"
	p, err = w.f.Plan(ctx, staging, rel("b1", on).ID, io.Discard)
	must(t, err)
	if got := kinds(p); !strings.Contains(got, "operated:pg-dbstandbys+w1 +w2") {
		t.Fatalf("plan = %s", got)
	}
	if _, err := w.f.Apply(ctx, staging, rel("b1", on).ID, io.Discard, nil); err != nil {
		t.Fatal(err)
	}
	if len(set) != 1 || !slices.Equal(set[0], []string{"w1", "w2"}) {
		t.Errorf("SetStandbys got %v", set)
	}

	have = []string{"w1", "w2"}
	p, err = w.f.Plan(ctx, staging, rel("b2", on).ID, io.Discard)
	must(t, err)
	if strings.Contains(kinds(p), "standbys") {
		t.Errorf("unchanged standbys moved: %s", kinds(p))
	}
	p, err = w.f.Plan(ctx, staging, rel("b3", infraFile+"      standbys: []\n").ID, io.Discard)
	must(t, err)
	if got := kinds(p); !strings.Contains(got, "operated:pg-dbstandbys") || !strings.Contains(got, "w1") {
		t.Errorf("empty list = %s", got)
	}
	p, err = w.f.Plan(ctx, staging, rel("b4", infraFile+"      standbys: [primary]\n").ID, io.Discard)
	must(t, err)
	if !p.Blocked() || !strings.Contains(strings.Join(p.Blockers, "|"), "the primary's node") {
		t.Errorf("R4 at plan: blockers %v", p.Blockers)
	}
}

func TestStandbysAreRefusedWhereTheyMeanNothing(t *testing.T) {
	for name, c := range map[string]struct{ file, want string }{
		"on s3":       {tiles("    s:\n      engine: s3\n      standbys: [w1]\n"), "tile s: standbys: only postgres has standbys"},
		"three":       {tiles("    db:\n      engine: postgres\n      standbys: [a, b, c]\n"), "at most 2"},
		"twice":       {tiles("    db:\n      engine: postgres\n      standbys: [a, a]\n"), "listed twice"},
		"on an image": {tiles("    api:\n      image: x\n      standbys: [a]\n"), "tile api"},
	} {
		_, err := Load([]byte(c.file), nil, "testorg")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
}

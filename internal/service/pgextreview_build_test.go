package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	mflow "github.com/FyrmForge/stackr/internal/service/internal/flow/operated"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/image"
	"github.com/FyrmForge/stackr/internal/service/internal/regfront"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// A rollback to an imported (outside) image keeps the org's import copy in the
// registry sweep, as the current-tile path does.
func TestRegistryCleanKeepsImportedRollbackImage(t *testing.T) {
	t.Parallel()
	r := newRecipeRig(t)
	ctx := context.Background()
	tl, _, err := r.orch.instanceOf(ctx, r.pg.ID)
	must(t, err)
	must(t, r.orch.setPGUp(ctx, r.inst.ID, pgUp{FromImage: "postgres:16.4", ToImage: image.ClusterRegistry + "o/s/pg:to"}))
	refs := map[string]bool{}
	r.orch.keepOperatedRefs(ctx, tl, "o", refs)
	repo, tag := regfront.ImportName("o", "postgres:16.4")
	if !refs[repo+":"+tag] || !refs["o/s/pg:to"] {
		t.Errorf("refs = %v: the imported rollback image is swept", refs)
	}
}

// After an org rename a recipe-built instance's build still sits under the old
// slug; the instance's own recorded ref passes, any other ref under that name
// does not.
func TestOwnClusterRefAcceptsRecordedBuildAfterRename(t *testing.T) {
	t.Parallel()
	r := newRecipeRig(t)
	ctx := context.Background()
	built := image.ClusterRegistry + "oldslug/s/pg:17.5-ext-amd64-aaaaaaaa"
	eb := mflow.ExtBuild{Base: pgStock, Ref: built}
	must(t, r.st.OperatedInstances.SetBuild(ctx, r.inst.ID, nil, nil, eb.String()))
	if err := r.orch.ownClusterRef(ctx, r.org, built); err != nil {
		t.Errorf("the recorded build is refused: %v", err)
	}
	if err := r.orch.ownClusterRef(ctx, r.org, image.ClusterRegistry+"oldslug/s/pg:other"); err == nil {
		t.Error("another ref under the old slug passed")
	}
	if err := r.orch.ownClusterRef(ctx, uuid.NewString(), built); err == nil {
		t.Error("another org used the build")
	}
}

// Only BuildKit's checksum-mismatch text is a bad sha256; any other error that
// merely mentions one is not.
func TestIsChecksumMismatch(t *testing.T) {
	for msg, want := range map[string]bool{
		"failed to solve: digest mismatch sha256:aa: sha256:bb (expected: sha256:cc)": true,
		"checksum mismatch: expected sha256:aa":                                       true,
		"pull access denied for registry, sha256:aa not found":                        false,
		"failed to compute cache key: sha256 layer missing":                           false,
	} {
		if got := isChecksumMismatch(errors.New(msg)); got != want {
			t.Errorf("%q = %v, want %v", msg, got, want)
		}
	}
}

func (r *recipeRig) stop() {
	r.fake.Edit(func() { r.fake.Containers = nil })
}

func (r *recipeRig) creates() int {
	n := 0
	for _, c := range r.fake.Calls() {
		if c.Method == "Create" {
			n++
		}
	}
	return n
}

// The stack file's plan step runs no container and stores nothing: a panel plan
// of the instance is left as it was.
func TestPGExtPlanFromFileIsSideEffectFree(t *testing.T) {
	t.Parallel()
	r := newRecipeRig(t)
	ctx := context.Background()
	panel := r.plan(t, "vector")
	before := r.creates()
	tl, m, err := r.orch.instanceOf(ctx, r.pg.ID)
	must(t, err)
	lines, build, err := r.orch.pgExtPlanFromFile(ctx, tl, m, []string{"vector"}, io.Discard)
	if err != nil || !build || len(lines) == 0 {
		t.Fatalf("lines %v, build %v, %v", lines, build, err)
	}
	if got := r.creates(); got != before {
		t.Errorf("the plan step ran %d containers", got-before)
	}
	if cur, _ := r.orch.LastPGExtPlan(ctx, r.inst.ID); cur == nil || cur.ID != panel.ID {
		t.Errorf("the panel plan was replaced: %+v", cur)
	}
	// a recipe the stack file names that does not exist is refused at plan time
	if _, _, err := r.orch.pgExtPlanFromFile(ctx, tl, m, []string{"vector", "nope"}, io.Discard); err != nil {
		t.Errorf("a name no recipe brings is the image's own: %v", err)
	}
}

// The apply resolves, builds and stores in the apply job, and leaves the
// panel's stored plan alone; the resolve container of every plan has its own
// name, so two never remove each other.
func TestPGExtApplyFromFileBuilds(t *testing.T) {
	t.Parallel()
	r := newRecipeRig(t)
	ctx := context.Background()
	panel := r.plan(t, "vector")
	tl, _, err := r.orch.instanceOf(ctx, r.pg.ID)
	must(t, err)
	must(t, r.orch.pgExtApplyFromFile(ctx, tl, []string{"vector"}, io.Discard))
	rows := r.rows(t)
	eb, _ := mflow.ParseExtBuild(rows.ExtBuild)
	if !eb.Built() || rows.Image != eb.Ref || !strings.Contains(strings.Join(rows.Extensions, " "), "vector") {
		t.Fatalf("rows = %+v", rows)
	}
	if cur, _ := r.orch.LastPGExtPlan(ctx, r.inst.ID); cur == nil || cur.ID != panel.ID {
		t.Errorf("the panel plan was replaced: %+v", cur)
	}
	var names []string
	for _, c := range r.fake.Calls() {
		if c.Method == "Create" {
			names = append(names, c.String())
		}
	}
	seen := map[string]bool{}
	for _, n := range names {
		if strings.Contains(n, "extresolve") {
			if seen[n] {
				t.Errorf("two resolve containers named alike: %s", n)
			}
			seen[n] = true
		}
	}
}

// An instance that does not run: the file's apply builds and boot-checks the
// image first, stores it and starts nothing; a failed check stores nothing.
func TestPGExtApplyFromFileStoppedBuildsAndChecks(t *testing.T) {
	t.Parallel()
	r := newRecipeRig(t)
	ctx := context.Background()
	r.stop()
	tl, _, err := r.orch.instanceOf(ctx, r.pg.ID)
	must(t, err)
	must(t, r.orch.pgExtApplyFromFile(ctx, tl, []string{"vector"}, io.Discard))
	rows := r.rows(t)
	eb, _ := mflow.ParseExtBuild(rows.ExtBuild)
	if !eb.Built() || rows.Image != eb.Ref {
		t.Fatalf("rows = %+v", rows)
	}
	if _, ok := r.checked.Load(eb.Ref); !ok {
		t.Error("the built image was stored without its boot check")
	}
	if up, _ := r.orch.reachedByRedeploy(ctx, tl); up {
		t.Error("a stopped instance was started")
	}
	if st, ok := r.cursor(t); !ok || st.Phase != pgExtDue {
		t.Errorf("cursor = %+v, %v (the next start reconciles)", st, ok)
	}

	// a check that fails stores nothing and leaves no cursor
	r2 := newRecipeRig(t)
	r2.stop()
	r2.check = func(string) (string, int) { return "FAILED vector\n", 3 }
	tl2, _, err := r2.orch.instanceOf(ctx, r2.pg.ID)
	must(t, err)
	before := r2.rows(t)
	err = r2.orch.pgExtApplyFromFile(ctx, tl2, []string{"vector"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "extension vector was not created") {
		t.Fatalf("err = %v", err)
	}
	if got := r2.rows(t); !got.equal(before) {
		t.Errorf("rows moved on a failed check: %+v", got)
	}
	if _, ok := r2.cursor(t); ok {
		t.Error("cursor left")
	}
}

// The panel's apply of a build on a stopped instance runs as a job that
// builds and boot-checks the image, stores it and starts nothing; a failed
// check stores nothing.
func TestApplyPGExtPlanStoppedBuildsInAJob(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newRecipeRig(t)
	p := r.plan(t, "vector")
	r.stop()
	_, j, err := r.orch.ApplyPGExtPlan(ctx, r.by, r.pg.ID, p.ID, ApproveOpts{Confirm: true})
	if err != nil || j == nil {
		t.Fatalf("job %v, err %v", j, err)
	}
	if got := r.wait(t, j.ID); got.State != "done" {
		t.Fatalf("job = %s: %s", got.State, got.Error)
	}
	rows := r.rows(t)
	eb, _ := mflow.ParseExtBuild(rows.ExtBuild)
	if !eb.Built() || rows.Image != eb.Ref {
		t.Fatalf("rows = %+v", rows)
	}
	if _, ok := r.checked.Load(eb.Ref); !ok {
		t.Error("the built image was stored without its boot check")
	}
	tl, _, err := r.orch.instanceOf(ctx, r.pg.ID)
	must(t, err)
	if up, _ := r.orch.reachedByRedeploy(ctx, tl); up {
		t.Error("a stopped instance was started")
	}

	r2 := newRecipeRig(t)
	p2 := r2.plan(t, "vector")
	r2.stop()
	r2.check = func(string) (string, int) { return "FAILED vector\n", 3 }
	before := r2.rows(t)
	_, j2, err := r2.orch.ApplyPGExtPlan(ctx, r2.by, r2.pg.ID, p2.ID, ApproveOpts{Confirm: true})
	must(t, err)
	if got := r2.wait(t, j2.ID); got.State != "failed" {
		t.Fatalf("job = %s", got.State)
	}
	if got := r2.rows(t); !got.equal(before) {
		t.Errorf("rows moved on a failed check: %+v", got)
	}
	if _, ok := r2.cursor(t); ok {
		t.Error("cursor left")
	}
}

// The job builds from the reviewed recipes its cursor carries, not from the
// definitions as they are when it runs.
func TestPGBuildImageUsesReviewedRecipes(t *testing.T) {
	t.Parallel()
	r := newRecipeRig(t)
	ctx := context.Background()
	var seen [][]string
	orig := r.orch.pgFns.Dockerfile
	r.orch.pgFns.Dockerfile = func(base string, major int, arch string, rs []mflow.Recipe, pins map[string]string) (string, error) {
		seen = append(seen, rs[0].Packages)
		return orig(base, major, arch, rs, pins)
	}
	// an admin edited the recipe after the plan was reviewed
	spec, _ := json.Marshal(mflow.Recipe{Packages: []string{"edited-pkg"}, Extensions: []string{"vector"}})
	must(t, r.st.PGRecipes.Upsert(ctx, store.PGRecipe{ID: "pgvector", Spec: string(spec), Enabled: true, Share: store.NameList{"all"}}))
	tl, m, err := r.orch.instanceOf(ctx, r.pg.ID)
	must(t, err)
	st := &pgExtState{Old: pgExtValsOf(tl, m), New: pgExtVals{Image: tl.ImageRef, Extensions: []string{"vector"}}, Build: &PGExtPlanIn{
		Names: []string{"pgvector"}, Base: pgStock, Arch: "amd64", Pins: map[string]string{"reviewed-pkg": "1-1"},
		Recipes: []mflow.Recipe{{Name: "pgvector", Packages: []string{"reviewed-pkg"}, Extensions: []string{"vector"}}},
	}}
	must(t, r.orch.pgBuildImage(ctx, tl, m, st, io.Discard))
	if len(seen) != 1 || len(seen[0]) != 1 || seen[0][0] != "reviewed-pkg" {
		t.Fatalf("built from %v, want the reviewed recipe", seen)
	}
	eb, _ := mflow.ParseExtBuild(st.New.ExtBuild)
	if !eb.Built() || eb.Packages["reviewed-pkg"] != "1-1" {
		t.Errorf("record = %+v: the pin of a package dpkg did not list is lost", eb)
	}
}

// A major change's target image takes the normal walk down: the newest build
// fails its boot check, the next older one passes.
func TestPGUpgradeTargetImageWalksDown(t *testing.T) {
	t.Parallel()
	r := recipePGUpRig(t)
	var first atomic.Value
	var imgs sync.Map
	r.orch.pgFns.Resolve = func(names []string, major int, _ string, _ []store.PGRecipe) ([]mflow.Recipe, error) {
		return []mflow.Recipe{{Name: names[0], Packages: []string{"postgresql-{major}-pgvector"}, Extensions: []string{"vector"}}}, nil
	}
	next := r.fake.Helper
	r.fake.Helper = func(s docker.ContainerSpec) (string, int, bool) {
		if len(s.Cmd) == 3 && s.Cmd[0] == "sh" {
			switch s.Cmd[2] {
			case pgRecipeCheckScript:
				imgs.Store(s.Image, true)
				if first.CompareAndSwap(nil, s.Image); first.Load() == s.Image { // the newest build fails
					return "FAILED vector\n", 3, true
				}
				return "P\tpostgresql-18\t18.1-1\nP\tpostgresql-18-pgvector\t0.8.5-1\nX\tvector\t0.8.5\n", 0, true
			case pgWalkScript:
				return "W\tpostgresql-18-pgvector\t0.8.5-1\n", 0, true
			}
		}
		return next(s)
	}
	u, ctx := r.bare(t)
	eb, _ := mflow.ParseExtBuild(u.m.ExtBuild)
	_, tb, err := u.toImage(ctx, eb, 18)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	imgs.Range(func(_, _ any) bool { n++; return true })
	if n != 2 || tb.Packages["postgresql-18-pgvector"] != "0.8.5-1" || tb.Ref == "" || tb.Ref == first.Load() {
		t.Errorf("%d images checked, build %+v", n, tb)
	}
}

// A URL recipe's build record keeps the URL and sha256 it was built from, so a
// later major change does not take them from a recipe edited since.
func TestPGBuildImageRecordsURLPackages(t *testing.T) {
	t.Parallel()
	r := newRecipeRig(t)
	ctx := context.Background()
	var vc mflow.Recipe
	for _, b := range mflow.BuiltinRecipes {
		if b.Name == "vchord" {
			vc = b
		}
	}
	// the rig's fake check offers vector only
	vc.Extensions, vc.Preload, vc.Requires = []string{"vector"}, nil, nil
	tl, m, err := r.orch.instanceOf(ctx, r.pg.ID)
	must(t, err)
	st := &pgExtState{Old: pgExtValsOf(tl, m), New: pgExtVals{Image: tl.ImageRef, Extensions: []string{"vector"}}, Build: &PGExtPlanIn{
		Names: []string{"vchord"}, Base: pgStock, Arch: "amd64", Recipes: []mflow.Recipe{vc},
	}}
	must(t, r.orch.pgBuildImage(ctx, tl, m, st, io.Discard))
	eb, _ := mflow.ParseExtBuild(st.New.ExtBuild)
	want := vc.SHA256[strconv.Itoa(m.Major)+"-amd64"]
	if len(eb.URLs) != 1 || want == "" {
		t.Fatalf("urls = %+v, want one for %d-amd64", eb.URLs, m.Major)
	}
	for _, u := range eb.URLs {
		if u.SHA256 != want || u.URL == "" {
			t.Errorf("url package = %+v, want sha %s", u, want)
		}
	}
}

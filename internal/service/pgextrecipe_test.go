package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/authz"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	mflow "github.com/FyrmForge/stackr/internal/service/internal/flow/operated"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/image"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/worker"
)

// The recipe engine is lane A2's; these tests give the Orchestrator a small
// fake of it (pgFnsFor) and script the one-shot containers through the fake
// daemon's Helper, so the plan, the apply and the walk down are tested here
// and not through A2's bodies.

const pgOld = "postgres:18.1" // older than the newest 18 minor stackr offers

type recipeRig struct {
	*pgExtRig
	by       *Principal
	resolves atomic.Int32
	// scripted answers of the one-shot containers
	resolve func(image string) string // "OK pkg=ver ..." or "NEED 18.5"
	check   func(image string) (out string, exit int)
	walk    func(pkgs string) string // PKGS of the walk: "name=version ..."
	checked sync.Map                 // the images the boot check ran in
	// failImage: Create of this image fails (the server does not start)
	failImage atomic.Value
}

func newRecipeRig(t *testing.T) *recipeRig {
	t.Helper()
	r := &recipeRig{pgExtRig: newPGExtRig(t), by: nil}
	r.by = roleP(r.org, "member")
	r.orch.cfg.FakeHost = t.TempDir()
	must(t, r.st.Tiles.SetImageRef(r.ctx, r.pg.ID, pgStock))
	r.resolve = func(string) string { return "OK postgresql-18-pgvector=0.8.6-1" }
	r.check = func(string) (string, int) {
		return "P\tpostgresql-18\t18.9-1\nP\tpostgresql-18-pgvector\t0.8.6-1\nS\tpostgres (PostgreSQL) 18.9 (Debian)\nX\tvector\t0.8.6\n", 0
	}
	r.walk = func(string) string { return "" }
	fns := pgRecipeFns{
		Check: func(rc mflow.Recipe) error {
			if rc.Name == "bad" {
				return errs.Invalidf("name", "bad recipe")
			}
			return nil
		},
		Resolve: func(names []string, major int, orgID string, rows []store.PGRecipe) ([]mflow.Recipe, error) {
			r.resolves.Add(1)
			defs := pgRecipeDefs(rows)
			var out []mflow.Recipe
			for _, n := range names {
				i := slices.IndexFunc(defs, func(d mflow.Recipe) bool { return d.Name == n })
				if i < 0 {
					return nil, errs.Conflictf("%s is not in the image and no recipe has that name.", n)
				}
				out = append(out, defs[i])
			}
			return out, nil
		},
		Preload: func(rs []mflow.Recipe) []string {
			var out []string
			for _, x := range rs {
				out = append(out, x.Preload...)
			}
			return out
		},
		Script: func(major int, pkgs []string) string { return "RESOLVE " + strings.Join(pkgs, " ") },
		Parse: func(out string) (mflow.Resolved, error) {
			f := strings.Fields(out)
			if len(f) == 2 && f[0] == "NEED" {
				return mflow.Resolved{NeedMinor: f[1]}, nil
			}
			res := mflow.Resolved{Versions: map[string]string{}}
			for _, kv := range f[1:] {
				k, v, _ := strings.Cut(kv, "=")
				res.Versions[k] = v
			}
			return res, nil
		},
		Dockerfile: func(base string, major int, arch string, rs []mflow.Recipe, pins map[string]string) (string, error) {
			b, _ := json.Marshal(pins)
			return fmt.Sprintf("FROM %s\n# %s %v\n", base, arch, string(b)), nil
		},
		Tag: func(base, arch, df string) string {
			return fmt.Sprintf("%s-ext-%s-%x", pgTagOf(base), arch, sha256.Sum256([]byte(df)))[:30]
		},
	}
	pgFnsFor.Store(r.orch, fns)
	t.Cleanup(func() { pgFnsFor.Delete(r.orch) })
	r.fake.Helper = func(s docker.ContainerSpec) (string, int, bool) {
		if len(s.Cmd) == 3 && s.Cmd[0] == "sh" {
			switch {
			case strings.HasPrefix(s.Cmd[2], "RESOLVE"):
				return r.resolve(s.Image), 0, true
			case s.Cmd[2] == pgWalkScript:
				for _, e := range s.Env {
					if v, ok := strings.CutPrefix(e, "PKGS="); ok {
						return r.walk(v), 0, true
					}
				}
			case s.Cmd[2] == pgRecipeCheckScript:
				r.checked.Store(s.Image, true)
				out, code := r.check(s.Image)
				return out, code, true
			}
		}
		if v, _ := r.failImage.Load().(string); v != "" {
			if s.Image == v {
				r.fake.Err = map[string]error{"Create": io.ErrUnexpectedEOF}
			} else {
				r.fake.Err = nil
			}
		}
		return "", 0, false
	}
	return r
}

// plan runs the plan job through the queue, for the extensions list.
func (r *recipeRig) plan(t *testing.T, exts ...string) *PGExtPlan {
	t.Helper()
	j, err := r.orch.PlanPGExtensions(context.Background(), r.by, r.pg.ID, PGExtIn{Extensions: &exts})
	must(t, err)
	if got := r.wait(t, j.ID); got.State != "done" {
		t.Fatalf("plan job = %s: %s", got.State, got.Error)
	}
	p, err := r.orch.LastPGExtPlan(context.Background(), r.inst.ID)
	must(t, err)
	if p == nil {
		t.Fatal("no plan stored")
	}
	return p
}

func has(lines []string, sub string) bool {
	return slices.ContainsFunc(lines, func(l string) bool { return strings.Contains(l, sub) })
}

// A plan resolves the recipes on the base, prices the build and the restart
// (message 14), and stores itself with an id and a hash that recomputes.
func TestPGRecipePlan(t *testing.T) {
	t.Parallel()
	r := newRecipeRig(t)
	p := r.plan(t, "vector")
	if len(p.Blockers) > 0 || !p.Build || !p.Restart || p.ID == "" || p.Hash == "" {
		t.Fatalf("plan = %+v", p)
	}
	if !strings.HasPrefix(p.In.Image, image.ClusterRegistry) || p.In.Base != pgStock || !slices.Equal(p.In.Names, []string{"pgvector"}) {
		t.Errorf("in = %+v", p.In)
	}
	if len(p.Versions) != 1 || p.Versions[0] != (PGPkgChange{Name: "postgresql-18-pgvector", To: "0.8.6-1"}) {
		t.Errorf("versions = %+v", p.Versions)
	}
	if !has(p.Lines, "Build an image") || !has(p.Lines, "restarts. A startup module runs in every database") || !has(p.Lines, "postgresql-18-pgvector 0.8.6-1") {
		t.Errorf("lines = %q", p.Lines)
	}
	tl, m, err := r.orch.instanceOf(r.ctx, r.pg.ID)
	must(t, err)
	if h, err := r.orch.pgPlanHash(r.ctx, tl, m, p.In); err != nil || h != p.Hash {
		t.Errorf("hash = %q, %v; stored %q", h, err, p.Hash)
	}
}

// A package that installs on no version of the base but does on the newest
// minor stackr offers: the plan says the instance is updated too (18a), the
// base moves, the resolve ran twice. Where even that fails it is 18b.
func TestPGRecipePlanSecondResolve(t *testing.T) {
	t.Parallel()
	r := newRecipeRig(t)
	must(t, r.st.Tiles.SetImageRef(r.ctx, r.pg.ID, pgOld))
	r.resolve = func(img string) string {
		if img == pgOld {
			return "NEED 18.5"
		}
		return "OK postgresql-18-pgvector=0.8.6-1"
	}
	p := r.plan(t, "vector")
	newest := fmt.Sprintf("postgres:%s", func() string { rl, _ := mflow.PGReleaseOf(18); return rl.Tag() }())
	if len(p.Blockers) > 0 || p.In.Base != newest {
		t.Fatalf("plan = %+v", p)
	}
	if !strings.Contains(p.PGUpdate, "pgvector needs postgres 18.5.") || !strings.Contains(p.PGUpdate, "runs 18.1 and is updated too.") || !has(p.Lines, p.PGUpdate) {
		t.Errorf("update line = %q", p.PGUpdate)
	}
	// 18b: the newest minor does not do either
	r.resolve = func(string) string { return "NEED 18.99" }
	p = r.plan(t, "vector")
	if len(p.Blockers) != 1 || !strings.Contains(p.Blockers[0], "needs postgres 18.99, which this stackr does not offer yet. Update stackr.") {
		t.Errorf("blockers = %q", p.Blockers)
	}
}

// Apply is bound to the reviewed plan: an unknown id and a plan whose inputs
// moved are message 28 and queue a new plan; a missing confirm on a plan that
// restarts is refused for confirmation and queues nothing (V6).
func TestPGRecipeApplyStaleAndConfirm(t *testing.T) {
	t.Parallel()
	r := newRecipeRig(t)
	ctx := context.Background()
	p := r.plan(t, "vector")
	jobs := func() int {
		js, err := r.st.Jobs.ListByState(ctx, "queued", "waiting", "running", "done")
		must(t, err)
		return len(js)
	}
	// V6: no confirm is not a stale plan
	before := jobs()
	_, j, err := r.orch.ApplyPGExtPlan(ctx, r.by, r.pg.ID, p.ID, ApproveOpts{})
	if !isConflict(err) || j != nil || !strings.Contains(err.Error(), "confirm") || strings.Contains(err.Error(), "out of date") {
		t.Fatalf("no confirm: %v, job %v", err, j)
	}
	if got := jobs(); got != before {
		t.Errorf("a refused confirm queued %d jobs", got-before)
	}
	if cur, _ := r.orch.LastPGExtPlan(ctx, r.inst.ID); cur == nil || cur.ID != p.ID {
		t.Error("the plan was replaced")
	}
	// an unknown id is stale: a new plan is queued
	_, _, err = r.orch.ApplyPGExtPlan(ctx, r.by, r.pg.ID, "no-such-plan", ApproveOpts{Confirm: true})
	if !isConflict(err) || !strings.Contains(err.Error(), "this plan is out of date; a new one is being made.") {
		t.Fatalf("unknown id: %v", err)
	}
	// the instance moved since the plan: its image changed. The replan the
	// unknown id queued may have landed first; plan again for a fresh id.
	p = r.plan(t, "vector")
	must(t, r.st.Tiles.SetImageRef(r.ctx, r.pg.ID, "postgres:18.8"))
	_, _, err = r.orch.ApplyPGExtPlan(ctx, r.by, r.pg.ID, p.ID, ApproveOpts{Confirm: true})
	if !isConflict(err) || !strings.Contains(err.Error(), "out of date") {
		t.Errorf("moved instance: %v", err)
	}
	if got := r.rows(t); got.ExtBuild != "{}" && got.ExtBuild != "" {
		t.Errorf("a stale apply wrote %q", got.ExtBuild)
	}
}

// A recipe edit between plan and apply makes the plan stale (the hash covers
// each recipe's full definition).
func TestPGRecipeApplyRecipeEdited(t *testing.T) {
	t.Parallel()
	r := newRecipeRig(t)
	ctx := context.Background()
	p := r.plan(t, "vector")
	spec, _ := json.Marshal(mflow.Recipe{Packages: []string{"postgresql-{major}-pgvector", "extra-pkg"}, Extensions: []string{"vector"}})
	must(t, r.st.PGRecipes.Upsert(ctx, store.PGRecipe{ID: "pgvector", Spec: string(spec), Enabled: true, Share: store.NameList{"all"}}))
	_, _, err := r.orch.ApplyPGExtPlan(ctx, r.by, r.pg.ID, p.ID, ApproveOpts{Confirm: true})
	if !isConflict(err) || !strings.Contains(err.Error(), "out of date") {
		t.Errorf("edited recipe: %v", err)
	}
}

// A good apply: the image is built and boot-checked, and the rows move in one
// step: image, startup modules, list and the build record with the installed
// versions. The plan is spent.
func TestPGRecipeApply(t *testing.T) {
	t.Parallel()
	r := newRecipeRig(t)
	ctx := context.Background()
	p := r.plan(t, "vector")
	_, j, err := r.orch.ApplyPGExtPlan(ctx, r.by, r.pg.ID, p.ID, ApproveOpts{Confirm: true})
	if err != nil || j == nil {
		t.Fatalf("apply: job %v, err %v", j, err)
	}
	if got := r.wait(t, j.ID); got.State != "done" {
		t.Fatalf("job = %s: %s", got.State, got.Error)
	}
	got := r.rows(t)
	eb, err := mflow.ParseExtBuild(got.ExtBuild)
	must(t, err)
	if !eb.Built() || got.Image != eb.Ref || got.Image != p.In.Image || !slices.Equal(got.Extensions, []string{"vector"}) {
		t.Fatalf("rows = %+v, build %+v", got, eb)
	}
	if eb.Base != pgStock || eb.Arch != p.In.Arch || !slices.Equal(eb.Recipes, []string{"pgvector"}) || eb.Packages["postgresql-18"] != "18.9-1" || eb.Packages["postgresql-18-pgvector"] != "0.8.6-1" {
		t.Errorf("build record = %+v", eb)
	}
	if cur, _ := r.orch.LastPGExtPlan(ctx, r.inst.ID); cur != nil {
		t.Error("the applied plan is still stored")
	}
	var built bool
	for _, c := range r.fake.Calls() {
		built = built || c.Method == "Build"
	}
	if !built {
		t.Error("no image was built")
	}
	if len(r.created(got.Image)) == 0 {
		t.Error("the built image never started")
	}
}

// A failed boot check at an extension walks the failing recipe down one build
// and tries again (Stage 0 spike 6); the second image passes.
func TestPGRecipeApplyWalksDown(t *testing.T) {
	t.Parallel()
	r := newRecipeRig(t)
	ctx := context.Background()
	var first string
	r.check = func(img string) (string, int) {
		if img == first {
			return "FAILED vector\n", 3
		}
		return "P\tpostgresql-18\t18.9-1\nP\tpostgresql-18-pgvector\t0.8.5-1\nS\tpostgres (PostgreSQL) 18.9 (Debian)\nX\tvector\t0.8.5\n", 0
	}
	r.walk = func(string) string { return "W\tpostgresql-18-pgvector\t0.8.5-1\n" }
	p := r.plan(t, "vector")
	first = p.In.Image
	_, j, err := r.orch.ApplyPGExtPlan(ctx, r.by, r.pg.ID, p.ID, ApproveOpts{Confirm: true})
	must(t, err)
	if got := r.wait(t, j.ID); got.State != "done" {
		t.Fatalf("job = %s: %s", got.State, got.Error)
	}
	eb, _ := mflow.ParseExtBuild(r.rows(t).ExtBuild)
	if eb.Packages["postgresql-18-pgvector"] != "0.8.5-1" || eb.Ref == p.In.Image {
		t.Errorf("build %+v", eb)
	}
	builds := 0
	for _, c := range r.fake.Calls() {
		if c.Method == "Build" {
			builds++
		}
	}
	if builds != 2 {
		t.Errorf("%d builds, want 2", builds)
	}
}

// A boot check that keeps failing stops at the cap, or at once when there is
// no older build, with the extension named; nothing is changed (message 19
// family) and the cursor is gone.
func TestPGRecipeApplyCheckFails(t *testing.T) {
	t.Parallel()
	for name, walk := range map[string]func(string) string{
		"no older build": func(string) string { return "" },
		"always older": func(pkgs string) string { // one patch older than what it is asked about
			_, cur, _ := strings.Cut(pkgs, "=")
			var a, b, c int
			_, _ = fmt.Sscanf(cur, "%d.%d.%d-1", &a, &b, &c)
			return fmt.Sprintf("W\tpostgresql-18-pgvector\t%d.%d.%d-1\n", a, b, c-1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newRecipeRig(t)
			r.check = func(string) (string, int) { return "undefined symbol: foo\nFAILED vector\n", 3 }
			r.walk = walk
			p := r.plan(t, "vector")
			_, j, err := r.orch.ApplyPGExtPlan(context.Background(), r.by, r.pg.ID, p.ID, ApproveOpts{Confirm: true})
			must(t, err)
			got := r.wait(t, j.ID)
			if got.State != "failed" || !strings.Contains(got.Error, "extension vector was not created; nothing was changed") {
				t.Fatalf("job = %s: %s", got.State, got.Error)
			}
			n := 0
			r.checked.Range(func(_, _ any) bool { n++; return true })
			if n > pgBuildWalkMax+1 || (name == "always older" && n != pgBuildWalkMax+1) {
				t.Errorf("%d images checked", n)
			}
			if rows := r.rows(t); rows.Image != pgStock || (rows.ExtBuild != "{}" && rows.ExtBuild != "") {
				t.Errorf("rows = %+v", rows)
			}
			if _, ok := r.cursor(t); ok {
				t.Error("cursor left")
			}
		})
	}
}

// The build moved the server minor: nothing is changed (message 19).
func TestPGRecipeApplyMinorMoved(t *testing.T) {
	t.Parallel()
	r := newRecipeRig(t)
	must(t, r.st.Tiles.SetImageRef(r.ctx, r.pg.ID, "postgres:18.6"))
	r.check = func(string) (string, int) {
		return "P\tpostgresql-18\t18.7-1\nS\tpostgres (PostgreSQL) 18.7 (Debian)\nX\tvector\t0.8.6\n", 0
	}
	p := r.plan(t, "vector")
	_, j, err := r.orch.ApplyPGExtPlan(context.Background(), r.by, r.pg.ID, p.ID, ApproveOpts{Confirm: true})
	must(t, err)
	got := r.wait(t, j.ID)
	if got.State != "failed" || !strings.Contains(got.Error, "the build moved postgres from 18.6 to 18.7; nothing was changed") {
		t.Fatalf("job = %s: %s", got.State, got.Error)
	}
	if rows := r.rows(t); rows.Image != "postgres:18.6" {
		t.Errorf("rows = %+v", rows)
	}
}

// The new server does not start: the put-back restores the image, both lists
// AND the build record (R1).
func TestPGRecipePutBackRestoresBuild(t *testing.T) {
	t.Parallel()
	r := newRecipeRig(t)
	ctx := context.Background()
	old := mflow.ExtBuild{Base: pgStock, Recipes: []string{"pgvector"}, Ref: image.ClusterRegistry + "acme/shop/pg:18.1-ext-amd64-old", Arch: "amd64"}
	must(t, r.st.OperatedInstances.SetBuild(ctx, r.inst.ID, nil, nil, old.String()))
	must(t, r.st.Tiles.SetImageRef(r.ctx, r.pg.ID, old.Ref))
	before := r.rows(t)
	p := r.plan(t, "vector")
	// the planned image fails to start; the old one runs
	r.failImage.Store(p.In.Image)
	// the built tag is the plan's unless the build walks: the check is clean
	_, j, err := r.orch.ApplyPGExtPlan(ctx, r.by, r.pg.ID, p.ID, ApproveOpts{Confirm: true})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	got := r.wait(t, j.ID)
	if got.State != "failed" || !strings.Contains(got.Error, "The previous image and startup modules are back and it runs again") {
		t.Fatalf("job = %s: %s", got.State, got.Error)
	}
	if rows := r.rows(t); !rows.equal(before) || rows.ExtBuild != old.String() {
		t.Errorf("rows = %+v, want %+v", rows, before)
	}
}

// ext_build '{}' and malformed text are not recipe-built, and an existing
// instance on them is refused nothing it was allowed before: a custom image
// takes its list as ever; an image edit of a recipe build is message 34.
func TestPGRecipeOwnedRefusals(t *testing.T) {
	t.Parallel()
	r := newRecipeRig(t)
	ctx := context.Background()
	for _, s := range []string{"", "{}", "{", `{"base":"postgres:18.9"}`} {
		if mflow.RecipeBuilt(s) {
			t.Errorf("%q is recipe-built", s)
		}
	}
	tl, m, err := r.orch.instanceOf(r.ctx, r.pg.ID)
	must(t, err)
	vec, trgm := []string{"vector"}, []string{"pg_trgm"}
	// official image, a name no recipe has: Stage 1's path
	if err := r.orch.pgExtOwnedRefusal(ctx, tl, m, PGExtIn{Extensions: &trgm}); err != nil {
		t.Errorf("contrib name: %v", err)
	}
	// official image, a recipe's extension: a plan (28)
	if err := r.orch.pgExtOwnedRefusal(ctx, tl, m, PGExtIn{Extensions: &vec}); !errors.Is(err, errPGPlanNeeded) {
		t.Errorf("recipe name: %v", err)
	}
	// custom image: Stage 1 as ever
	cust := tl
	cust.ImageRef = pgCustom
	if err := r.orch.pgExtOwnedRefusal(ctx, cust, m, PGExtIn{Extensions: &vec}); err != nil {
		t.Errorf("custom image: %v", err)
	}
	// a recipe build: no image by hand (34), no startup modules by hand (21)
	built := m
	built.ExtBuild = mflow.ExtBuild{Base: pgStock, Ref: "localhost:5000/o/s/pg:x"}.String()
	other, pre := "postgres:18.1", []string{"vchord"}
	if err := r.orch.pgExtOwnedRefusal(ctx, tl, built, PGExtIn{Image: &other}); !isConflict(err) || !strings.Contains(err.Error(), "is built from its extensions; its image is not set by hand.") {
		t.Errorf("image edit: %v", err)
	}
	if err := r.orch.pgExtOwnedRefusal(ctx, tl, built, PGExtIn{Preload: &pre}); !isConflict(err) || !strings.Contains(err.Error(), "come from its recipes") {
		t.Errorf("preload: %v", err)
	}
	// the plain path through setPGExt asks for a plan, and queues one
	_, _, err = r.orch.SetPGExtensions(ctx, r.by, r.pg.ID, PGExtIn{Extensions: &vec})
	if !isConflict(err) || !strings.Contains(err.Error(), "this plan is out of date; a new one is being made.") {
		t.Errorf("SetPGExtensions: %v", err)
	}
	if got := r.rows(t); len(got.Extensions) != 0 {
		t.Errorf("the plain path stored %v", got.Extensions)
	}
}

// An image edit through UpdateTile on a recipe build is message 34.
func TestPGRecipeUpdateTileRefused(t *testing.T) {
	t.Parallel()
	r := newRecipeRig(t)
	ctx := context.Background()
	ref := image.ClusterRegistry + "o/s/pg:built"
	must(t, r.st.OperatedInstances.SetBuild(ctx, r.inst.ID, nil, nil, mflow.ExtBuild{Base: pgStock, Ref: ref, Arch: "amd64"}.String()))
	must(t, r.st.Tiles.SetImageRef(r.ctx, r.pg.ID, ref))
	_, _, err := r.orch.UpdateTile(ctx, r.pg.ID, func(x *Tile) error { x.ImageRef = pgStock; return nil })
	if !isConflict(err) || !strings.Contains(err.Error(), "is built from its extensions; its image is not set by hand.") {
		t.Errorf("UpdateTile = %v", err)
	}
	if got := r.rows(t); got.Image != ref {
		t.Errorf("image = %q", got.Image)
	}
}

// The recipe-built instance's update is computed from its base, and is a plan
// on the new base, not an image edit.
func TestPGVersionRecipeBuilt(t *testing.T) {
	t.Parallel()
	r := newRecipeRig(t)
	ctx := context.Background()
	ref := image.ClusterRegistry + "o/s/pg:built"
	must(t, r.st.OperatedInstances.SetBuild(ctx, r.inst.ID, nil, []string{"vector"}, mflow.ExtBuild{Base: pgOld, Recipes: []string{"pgvector"}, Ref: ref, Arch: "amd64"}.String()))
	must(t, r.st.Tiles.SetImageRef(r.ctx, r.pg.ID, ref))
	v, err := r.orch.PGVersion(ctx, r.pg.ID)
	must(t, err)
	rl, _ := mflow.PGReleaseOf(18)
	if v.UpdateTo != rl.Image() {
		t.Errorf("UpdateTo = %q, want %q", v.UpdateTo, rl.Image())
	}
	_, j, err := r.orch.UpdatePGVersion(ctx, r.pg.ID)
	if err != nil || j == nil || j.Kind != string(kindPGExt) {
		t.Fatalf("job %v, err %v", j, err)
	}
	if got := r.wait(t, j.ID); got.State != "done" {
		t.Fatalf("plan job = %s: %s", got.State, got.Error)
	}
	p, err := r.orch.LastPGExtPlan(ctx, r.inst.ID)
	if err != nil || p == nil || !p.In.Update || p.In.Base != rl.Image() || !p.Build {
		t.Errorf("plan = %+v, %v", p, err)
	}
	if got := r.rows(t); got.Image != ref {
		t.Errorf("the update changed the image before it was applied: %q", got.Image)
	}
}

// A recipe build's major change is message 25 until Stage 3, before message 2.
func TestPGUpgradeRecipeBuiltRefused(t *testing.T) {
	t.Parallel()
	tl := Tile{Slug: "pg", ImageRef: "localhost:5000/o/s/pg:x"}
	m := OperatedInstance{ExtBuild: mflow.ExtBuild{Base: pgStock, Ref: "localhost:5000/o/s/pg:x"}.String()}
	err := noCustomImageForChange(tl, m, 17)
	if err != nil { // message 25 is gone (R28): a recipe build moves by rebuilding
		t.Errorf("err = %v", err)
	}
	m.ExtBuild = "{}"
	err = noCustomImageForChange(tl, m, 17)
	if !isConflict(err) || !strings.Contains(err.Error(), "runs a custom image") {
		t.Errorf("{} err = %v", err)
	}
}

// The registry sweep keeps the built image of a tile by its own ref, the build
// a change replaced for pgupKeep, and the images of a change in flight.
func TestRegistryCleanKeepsInstanceBuilds(t *testing.T) {
	t.Parallel()
	r := newRecipeRig(t)
	ctx := context.Background()
	prev := image.ClusterRegistry + "o/s/pg:prev"
	cur := image.ClusterRegistry + "o/s/pg:cur"
	eb := mflow.ExtBuild{Base: pgStock, Ref: cur, PrevRef: prev, PrevAt: time.Now().Add(-time.Hour)}
	must(t, r.st.OperatedInstances.SetBuild(ctx, r.inst.ID, nil, nil, eb.String()))
	tl, _, err := r.orch.instanceOf(ctx, r.pg.ID)
	must(t, err)
	refs := map[string]bool{}
	r.orch.keepOperatedRefs(ctx, tl, refs)
	if !refs["o/s/pg:prev"] {
		t.Errorf("refs = %v: the previous build is swept", refs)
	}
	// an old previous build goes
	eb.PrevAt = time.Now().Add(-2 * pgupKeep)
	must(t, r.st.OperatedInstances.SetBuild(ctx, r.inst.ID, nil, nil, eb.String()))
	refs = map[string]bool{}
	r.orch.keepOperatedRefs(ctx, tl, refs)
	if refs["o/s/pg:prev"] {
		t.Error("an old previous build is kept")
	}
	// a change in flight keeps both ends
	must(t, r.orch.setPGExtState(ctx, r.inst.ID, pgExtState{Phase: pgExtStart, Old: pgExtVals{Image: prev}, New: pgExtVals{Image: cur}}))
	refs = map[string]bool{}
	r.orch.keepOperatedRefs(ctx, tl, refs)
	if !refs["o/s/pg:prev"] || !refs["o/s/pg:cur"] {
		t.Errorf("refs = %v", refs)
	}
	// a major change keeps its images
	must(t, r.orch.setPGUp(ctx, r.inst.ID, pgUp{FromImage: image.ClusterRegistry + "o/s/pg:from", ToImage: image.ClusterRegistry + "o/s/pg:to"}))
	refs = map[string]bool{}
	r.orch.keepOperatedRefs(ctx, tl, refs)
	if !refs["o/s/pg:from"] || !refs["o/s/pg:to"] {
		t.Errorf("refs = %v", refs)
	}
}

// The deploy hook: a recipe-built tile whose image is gone is built again from
// its recorded pins, to the same ref; pins that no longer give that ref are
// message 33; a tile that is not recipe-built has nothing to rebuild.
func TestPGRecipeRebuildOperated(t *testing.T) {
	t.Parallel()
	r := newRecipeRig(t)
	ctx := context.Background()
	tl, _, err := r.orch.instanceOf(ctx, r.pg.ID)
	must(t, err)
	if err := r.orch.rebuildOperated(r.ctx, tl, io.Discard); !errors.Is(err, docker.ErrNoClusterImage) {
		t.Fatalf("not built: %v", err)
	}
	// the build a plan made, recorded
	p := r.plan(t, "vector")
	rs := []mflow.Recipe{mflow.BuiltinRecipes[0]}
	eb := r.orch.pgPlanBuild(p.In, rs, mflow.ExtBuild{})
	must(t, r.st.OperatedInstances.SetBuild(ctx, r.inst.ID, nil, []string{"vector"}, eb.String()))
	must(t, r.st.Tiles.SetImageRef(r.ctx, r.pg.ID, eb.Ref))
	tl, _, err = r.orch.instanceOf(ctx, r.pg.ID)
	must(t, err)
	before := len(r.fake.Calls())
	if err := r.orch.rebuildOperated(r.ctx, tl, io.Discard); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	built := false
	for _, c := range r.fake.Calls()[before:] {
		built = built || (c.Method == "Build" && strings.Contains(c.String(), eb.Ref))
	}
	if !built {
		t.Error("the image was not built to its ref")
	}
	// a pin that gives another ref (the recipe changed since)
	eb.Packages = map[string]string{"postgresql-18-pgvector": "0.0.1-1"}
	must(t, r.st.OperatedInstances.SetBuild(ctx, r.inst.ID, nil, []string{"vector"}, eb.String()))
	err = r.orch.rebuildOperated(r.ctx, tl, io.Discard)
	if !isConflict(err) || !strings.Contains(err.Error(), "postgresql-18-pgvector 0.0.1-1 can no longer be installed. Run Check again to pick current versions.") {
		t.Errorf("changed pins: %v", err)
	}
}

// Recipes: a built-in is switched on and off, never edited or deleted (22); an
// admin recipe goes through CheckRecipe, and cannot be deleted while an
// instance holds it.
func TestPGRecipeAdmin(t *testing.T) {
	t.Parallel()
	r := newRecipeRig(t)
	ctx := context.Background()
	admin := &Principal{User: User{ID: "root"}, Access: authz.User{ID: "root", Active: true, Admin: true}}
	vs, err := r.orch.PGRecipes(ctx)
	must(t, err)
	if len(vs) != len(mflow.BuiltinRecipes) || !vs[0].Builtin || !vs[0].Enabled {
		t.Fatalf("recipes = %+v", vs)
	}
	if _, err := r.orch.SetPGRecipe(ctx, r.by, PGRecipeView{Name: "pgvector"}); err == nil {
		t.Error("a member switched a recipe off")
	}
	v, err := r.orch.SetPGRecipe(ctx, admin, PGRecipeView{Name: "pgvector", Enabled: false, Packages: []string{"edited"}})
	if err != nil || v.Enabled || v.Packages[0] != "postgresql-{major}-pgvector" {
		t.Fatalf("switch off: %+v, %v", v, err)
	}
	if err := r.orch.DeletePGRecipe(ctx, admin, "pgvector"); !isConflict(err) || !strings.Contains(err.Error(), "pgvector is built in; switch it off instead.") {
		t.Errorf("delete built-in: %v", err)
	}
	if _, err := r.orch.SetPGRecipe(ctx, admin, PGRecipeView{Name: "bad", Enabled: true, Packages: []string{"x"}}); err == nil {
		t.Error("CheckRecipe was skipped")
	}
	own := PGRecipeView{Name: "mine", Enabled: true, Packages: []string{"postgresql-{major}-mine"}, Extensions: []string{"mine"}, Share: []string{"all"}}
	if v, err = r.orch.SetPGRecipe(ctx, admin, own); err != nil || v.Builtin || !slices.Equal(v.Extensions, []string{"mine"}) {
		t.Fatalf("add: %+v, %v", v, err)
	}
	eb := mflow.ExtBuild{Base: pgStock, Ref: "localhost:5000/o/s/pg:x", Recipes: []string{"mine"}}
	must(t, r.st.OperatedInstances.SetBuild(ctx, r.inst.ID, nil, nil, eb.String()))
	if err := r.orch.DeletePGRecipe(ctx, admin, "mine"); !isConflict(err) || !strings.Contains(err.Error(), "mine is enabled on 1 instances; it stays until they drop it.") {
		t.Errorf("delete in use: %v", err)
	}
	must(t, r.st.OperatedInstances.SetBuild(ctx, r.inst.ID, nil, nil, "{}"))
	if err := r.orch.DeletePGRecipe(ctx, admin, "mine"); err != nil {
		t.Errorf("delete: %v", err)
	}
	w := &world{orch: r.orch, st: r.st}
	if len(events(t, w, "pg.recipe-set")) != 2 || len(events(t, w, "pg.recipe-delete")) != 1 {
		t.Error("recipe changes are not audited")
	}
}

// A placement move names each recipe-built instance whose build is for another
// CPU type than the target's (message 32). The plain "cpu arch" check still
// decides the move.
func TestPGRecipeMovePlanLine(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := newPMFix(t)
	pg, err := n.orch.CreateOperatedTile(ctx, Tile{EnvironmentID: n.dev.ID, Name: "pg"}, "postgres", 0)
	must(t, err)
	m, err := n.orch.operated.GetByTile(ctx, pg.ID)
	must(t, err)
	eb := mflow.ExtBuild{Base: pgStock, Ref: "localhost:5000/o/s/pg:x", Arch: "amd64"}
	must(t, n.st.OperatedInstances.SetBuild(ctx, m.ID, nil, nil, eb.String()))
	line := func() string {
		plan, err := n.orch.PlanPlacementMove(ctx, "eu", "w1")
		must(t, err)
		for _, c := range plan.Checks {
			if c.Name == "extension build" {
				return c.Detail
			}
		}
		return ""
	}
	if got := line(); got != "" {
		t.Errorf("same CPU type: %q", got)
	}
	n.m.facts["w1"] = worker.Facts{Arch: "arm64", DiskFree: 500 << 30, DiskTotal: 1000 << 30, MemFree: 8 << 30}
	if got, want := line(), "pg was built for amd64; it is built again for arm64 on w1."; got != want {
		t.Errorf("line = %q, want %q", got, want)
	}
}

// A stack file on a not-yet-built instance: names no recipe brings make no
// plan (the file's plain path stores them); a recipe name does.
func TestPGExtPlanFromFileFirstBuild(t *testing.T) {
	t.Parallel()
	r := newRecipeRig(t)
	tl, m, err := r.orch.instanceOf(r.ctx, r.pg.ID)
	must(t, err)
	if _, id, err := r.orch.pgExtPlanFromFile(r.ctx, tl, m, []string{"pgcrypto"}, io.Discard); err != nil || id != "" {
		t.Fatalf("plain name: id %q, %v", id, err)
	}
	if _, id, err := r.orch.pgExtPlanFromFile(r.ctx, tl, m, []string{"vector"}, io.Discard); err != nil || id == "" {
		t.Fatalf("recipe name: id %q, %v", id, err)
	}
}

// The check's output as a real image prints it: packages dpkg only knows of
// carry no version and are not installed.
func TestParsePGCheckRealOutput(t *testing.T) {
	out := "P\tpostgresql-17\t17.11-1.pgdg13+2\nP\tpostgresql-17-jit-llvm\t\nP\tpostgresql-17-vchord\t1.1.1-1\n" +
		"S\tpostgres (PostgreSQL) 17.11 (Debian 17.11-1.pgdg13+2)\nX\tvchord\t1.1.1\nX\tvector\t0.8.7\n"
	b := parsePGCheck(out)
	if len(b.Pkgs) != 2 || b.Pkgs["postgresql-17-vchord"] != "1.1.1-1" || b.Server != "17.11" || b.Avail["vector"] != "0.8.7" {
		t.Errorf("parsed = %+v", b)
	}
}

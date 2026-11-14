package service

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	mflow "github.com/FyrmForge/stackr/internal/service/internal/flow/operated"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// builtRecord is a pgvector build record of an instance on postgres 18.
func builtRecord(m OperatedInstance, ref, pgvector string) mflow.ExtBuild {
	return mflow.ExtBuild{
		Base: pgStock, Recipes: []string{"pgvector"}, Arch: "amd64", Ref: ref,
		Packages: map[string]string{"postgresql-18": "18.9-1", fmt.Sprintf("postgresql-%d-pgvector", m.Major): pgvector},
	}
}

// markBuilt makes the instance of tile recipe-built.
func (r *recipeRig) markBuilt(t *testing.T, tileID, ref, pgvector string) OperatedInstance {
	t.Helper()
	m, err := r.orch.operated.GetByTile(r.ctx, tileID)
	must(t, err)
	must(t, r.st.OperatedInstances.SetBuild(r.ctx, m.ID, nil, []string{"vector"}, builtRecord(m, ref, pgvector).String()))
	m, err = r.orch.operated.GetByTile(r.ctx, tileID)
	must(t, err)
	return m
}

// countResolves scripts the newest build and returns how many resolve
// containers have been created so far.
func (r *recipeRig) countResolves(newest string) func() int {
	r.resolve = func(string) string { return "OK postgresql-18-pgvector=" + newest }
	return func() int {
		n := 0
		for _, c := range r.fake.Calls() {
			if c.Method == "Create" && len(c.Args) > 0 && strings.HasPrefix(c.Args[0], "stackr-extresolve-") {
				n++
			}
		}
		return n
	}
}

// The daily check records a newer build of a built instance.
func TestPGExtUpdateCheckRecordsChange(t *testing.T) {
	t.Parallel()
	r := newRecipeRig(t)
	n := r.countResolves("0.8.7-1")
	m := r.markBuilt(t, r.pg.ID, "ref1", "0.8.6-1")
	r.orch.checkPGExtUpdates(r.ctx)
	u := r.orch.PGExtUpdatesOf(r.ctx, m)
	if u == nil || len(u.Changes) != 1 || u.Changes[0] != (PGExtChange{Package: "postgresql-18-pgvector", From: "0.8.6-1", To: "0.8.7-1"}) {
		t.Fatalf("updates = %+v", u)
	}
	if n() != 1 {
		t.Errorf("resolves = %d", n())
	}
	v, err := r.orch.PGExtensions(r.ctx, r.pg.ID)
	if err != nil && v.Plan == "" {
		t.Fatal(err)
	}
	if v.Updates == nil || len(v.Updates.Changes) != 1 {
		t.Errorf("view updates = %+v", v.Updates)
	}
	if c := r.orch.pgExtUpdateCount(r.ctx, m); c != 1 {
		t.Errorf("count = %d", c)
	}
	// up to date: the row says so, nothing is shown
	r.countResolves("0.8.6-1")
	r.orch.checkPGExtUpdates(r.ctx)
	if u := r.orch.PGExtUpdatesOf(r.ctx, m); u != nil {
		t.Errorf("up to date shows %+v", u)
	}
	if _, ok, _ := r.orch.store.Settings.Get(r.ctx, pgextupdKey+m.ID); !ok {
		t.Error("the up-to-date check left no row")
	}
}

// An instance that is not recipe-built is never touched: no container, no key.
func TestPGExtUpdateCheckSkipsUnbuilt(t *testing.T) {
	t.Parallel()
	r := newRecipeRig(t)
	n := r.countResolves("0.8.7-1")
	r.orch.checkPGExtUpdates(r.ctx)
	if n() != 0 {
		t.Errorf("resolves = %d", n())
	}
	rows, err := r.orch.store.Settings.ListPrefix(r.ctx, pgextupdKey)
	must(t, err)
	if len(rows) != 0 {
		t.Errorf("rows = %v", rows)
	}
}

// Two instances on one node, base and package set share one resolve.
func TestPGExtUpdateCheckSharesResolve(t *testing.T) {
	t.Parallel()
	r := newRecipeRig(t)
	n := r.countResolves("0.8.7-1")
	pg2, err := r.orch.CreateOperatedTile(r.ctx, Tile{EnvironmentID: r.env, Name: "pg2"}, "postgres", 0)
	must(t, err)
	m1 := r.markBuilt(t, r.pg.ID, "ref1", "0.8.6-1")
	m2 := r.markBuilt(t, pg2.ID, "ref2", "0.8.5-1")
	r.orch.checkPGExtUpdates(r.ctx)
	if n() != 1 {
		t.Errorf("resolves = %d, want 1", n())
	}
	for _, m := range []OperatedInstance{m1, m2} {
		if u := r.orch.PGExtUpdatesOf(r.ctx, m); u == nil || len(u.Changes) != 1 {
			t.Errorf("%s updates = %+v", m.ID, u)
		}
	}
}

// A URL package follows the recipe's version against the release the build holds.
func TestPGExtUpdateCheckURLVersion(t *testing.T) {
	t.Parallel()
	r := newRecipeRig(t)
	n := r.countResolves("0.8.6-1")
	fns := r.orch.pgFns
	was := fns.Resolve
	fns.Resolve = func(names []string, major int, orgID string, rows []store.PGRecipe) ([]mflow.Recipe, error) {
		rs, err := was(names, major, orgID, rows)
		for i := range rs {
			if rs[i].Name == "vchord" {
				rs[i].Version = "1.2.0"
			}
		}
		return rs, err
	}
	m, err := r.orch.operated.GetByTile(r.ctx, r.pg.ID)
	must(t, err)
	eb := mflow.ExtBuild{
		Base: pgStock, Recipes: []string{"vchord"}, Arch: "amd64", Ref: "ref1",
		URLs: map[string]mflow.URLPkg{"postgresql-18-vchord": {Version: "1.1.1"}},
	}
	must(t, r.st.OperatedInstances.SetBuild(r.ctx, m.ID, nil, nil, eb.String()))
	r.orch.checkPGExtUpdates(r.ctx)
	m, _ = r.orch.operated.GetByTile(r.ctx, r.pg.ID)
	u := r.orch.PGExtUpdatesOf(r.ctx, m)
	if u == nil || len(u.Changes) != 1 || u.Changes[0] != (PGExtChange{Package: "postgresql-18-vchord", From: "1.1.1", To: "1.2.0"}) {
		t.Fatalf("updates = %+v", u)
	}
	if n() != 0 {
		t.Errorf("a URL recipe started %d containers", n())
	}
}

// A failed resolve keeps the last result.
func TestPGExtUpdateCheckFailureKeeps(t *testing.T) {
	t.Parallel()
	r := newRecipeRig(t)
	r.countResolves("0.8.7-1")
	m := r.markBuilt(t, r.pg.ID, "ref1", "0.8.6-1")
	r.orch.checkPGExtUpdates(r.ctx)
	r.orch.pgFns.Parse = func(string) (mflow.Resolved, error) { return mflow.Resolved{}, errors.New("apt is down") }
	r.orch.checkPGExtUpdates(r.ctx)
	if u := r.orch.PGExtUpdatesOf(r.ctx, m); u == nil || len(u.Changes) != 1 {
		t.Errorf("after a failed resolve: %+v", u)
	}
}

// The key goes when the build record moves, and with its instance.
func TestPGExtUpdateCleared(t *testing.T) {
	t.Parallel()
	r := newRecipeRig(t)
	r.countResolves("0.8.7-1")
	m := r.markBuilt(t, r.pg.ID, "ref1", "0.8.6-1")
	r.orch.checkPGExtUpdates(r.ctx)
	key := pgextupdKey + m.ID
	has := func() bool { _, ok, _ := r.orch.store.Settings.Get(r.ctx, key); return ok }
	if !has() {
		t.Fatal("no row after the check")
	}
	// a write of the same record keeps it
	must(t, r.orch.pgExtWrite(r.ctx, r.pg.ID, r.rows(t)))
	if !has() {
		t.Error("an unchanged build cleared the row")
	}
	// an apply moves the build record
	v := r.rows(t)
	v.ExtBuild = builtRecord(m, "ref2", "0.8.7-1").String()
	must(t, r.orch.pgExtWrite(r.ctx, r.pg.ID, v))
	if has() {
		t.Error("the row outlived the build it checked")
	}
	// a row of a build that moved without the hook is never shown
	b, _ := mflow.ParseExtBuild(v.ExtBuild)
	must(t, r.orch.store.Settings.Set(r.ctx, key, fmt.Sprintf(`{"checked_at":%q,"ref":"ref1","changes":[{"package":"p","from":"1","to":"2"}]}`, time.Now().Format(time.RFC3339))))
	m, _ = r.orch.operated.GetByTile(r.ctx, r.pg.ID)
	if u := r.orch.PGExtUpdatesOf(r.ctx, m); u != nil {
		t.Errorf("a check of %s shown on %s: %+v", "ref1", b.Ref, u)
	}
	// rows of an instance that is gone are swept
	must(t, r.orch.store.Settings.Set(r.ctx, pgextupdKey+"gone", `{"changes":[]}`))
	r.orch.checkPGExtUpdates(r.ctx)
	if _, ok, _ := r.orch.store.Settings.Get(r.ctx, pgextupdKey+"gone"); ok {
		t.Error("the row of a gone instance stayed")
	}
}

// The canvas card of the instance carries the count, for its chip.
func TestPGExtUpdateOnCanvas(t *testing.T) {
	t.Parallel()
	r := newRecipeRig(t)
	r.countResolves("0.8.7-1")
	r.markBuilt(t, r.pg.ID, "ref1", "0.8.6-1")
	r.orch.checkPGExtUpdates(r.ctx)
	v, err := r.orch.Canvas(r.ctx, CanvasScope{Kind: CanvasEnv, ID: r.env}, ShowAll)
	must(t, err)
	for _, n := range v.Nodes {
		if n.ID == r.pg.ID {
			if n.ExtUpdates != 1 {
				t.Errorf("ExtUpdates = %d", n.ExtUpdates)
			}
			return
		}
	}
	t.Error("the instance is not on the canvas")
}

package service

import (
	"context"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

const (
	pgCustom = "ghcr.io/acme/pg:18.1"
	pgStock  = "postgres:18.9"
)

// pgExtRig is branchRig with a deploy that works on the fake daemon and the
// instance on a custom image.
type pgExtRig struct {
	*branchRig
	ctx context.Context // on the instance's node, as a job's is
}

func newPGExtRig(t *testing.T) *pgExtRig {
	t.Helper()
	r := &pgExtRig{branchRig: newBranchRig(t)}
	r.fake.Details["c-pool"] = docker.Detail{Running: true, Health: "healthy", Networks: map[string]string{"n": "10.0.0.2"}}
	ctx, err := r.orch.withTileNode(context.Background(), r.pg.ID)
	must(t, err)
	r.ctx = ctx
	must(t, r.st.Tiles.SetImageRef(ctx, r.pg.ID, pgCustom))
	return r
}

func (r *pgExtRig) rows(t *testing.T) pgExtVals {
	t.Helper()
	tl, m, err := r.orch.instanceOf(r.ctx, r.pg.ID)
	must(t, err)
	return pgExtValsOf(tl, m)
}

func (r *pgExtRig) cursor(t *testing.T) (pgExtState, bool) {
	t.Helper()
	return r.orch.pgExtOf(r.ctx, r.inst.ID)
}

// queue writes the cursor of a change as setPGExt would.
func (r *pgExtRig) queue(t *testing.T, nw pgExtVals) pgExtState {
	t.Helper()
	st := pgExtState{Phase: pgExtQueued, Old: r.rows(t), New: nw}
	must(t, r.orch.setPGExtState(r.ctx, r.inst.ID, st))
	return st
}

func (r *pgExtRig) apply(t *testing.T, st pgExtState) error {
	t.Helper()
	tl, m, err := r.orch.instanceOf(r.ctx, r.pg.ID)
	must(t, err)
	return r.orch.applyPGExt(r.ctx, tl, m, st, io.Discard)
}

func (r *pgExtRig) created(image string) []string {
	var out []string
	for _, c := range r.fake.Calls() {
		if c.Method == "Create" && len(c.Args) == 2 && c.Args[1] == image {
			out = append(out, c.String())
		}
	}
	return out
}

// A change that works: stored, restarted on the new image, cursor gone.
func TestPGExtApply(t *testing.T) {
	t.Parallel()
	r := newPGExtRig(t)
	want := pgExtVals{Image: pgStock, Extensions: []string{"vector"}}
	must(t, r.apply(t, r.queue(t, want)))
	if got := r.rows(t); !got.equal(want) {
		t.Errorf("rows = %+v, want %+v", got, want)
	}
	if st, ok := r.cursor(t); ok {
		t.Errorf("cursor left: %+v", st)
	}
	if len(r.created(pgStock)) == 0 {
		t.Error("the new image was never started")
	}
}

// The server does not start on the new image (R1): the old image and lists
// are put back, the old one runs again (7a), and the job fails.
func TestPGExtPutBack(t *testing.T) {
	t.Parallel()
	r := newPGExtRig(t)
	old := r.rows(t)
	want := pgExtVals{Image: pgStock, Extensions: []string{"vector"}}
	// the new image's container fails to be created; the old one's works. The
	// fake asks Helper for every spec with its lock held, before it looks at Err.
	r.fake.Helper = func(s docker.ContainerSpec) (string, int, bool) {
		switch s.Image {
		case pgStock:
			r.fake.Err = map[string]error{"Create": io.ErrUnexpectedEOF}
		case pgCustom:
			r.fake.Err = nil
		}
		return "", 0, false
	}
	err := r.apply(t, r.queue(t, want))
	if err == nil || !strings.Contains(err.Error(), "The previous image and startup modules are back and it runs again") {
		t.Fatalf("err = %v, want 7a", err)
	}
	if got := r.rows(t); !got.equal(old) {
		t.Errorf("rows = %+v, want the old %+v", got, old)
	}
	if st, ok := r.cursor(t); ok {
		t.Errorf("cursor left: %+v", st)
	}
}

// The old image does not start either (7b): the values are still put back,
// the cursor goes, and the error says so.
func TestPGExtPutBackFails(t *testing.T) {
	t.Parallel()
	r := newPGExtRig(t)
	old := r.rows(t)
	r.fake.Details["c-pool"] = docker.Detail{Running: true, Health: "healthy"} // no address: every rollout fails
	err := r.apply(t, r.queue(t, pgExtVals{Image: pgStock}))
	if err == nil || !strings.Contains(err.Error(), "did not start either") {
		t.Fatalf("err = %v, want 7b", err)
	}
	if got := r.rows(t); !got.equal(old) {
		t.Errorf("rows = %+v, want the old %+v", got, old)
	}
	if _, ok := r.cursor(t); ok {
		t.Error("cursor left")
	}
}

// V3: an error before any container changed (the pull) puts only the columns
// back, redeploys nothing and says nothing was changed.
func TestPGExtPhaseless(t *testing.T) {
	t.Parallel()
	r := newPGExtRig(t)
	old := r.rows(t)
	r.fake.Edit(func() { r.fake.Err = map[string]error{"Pull(" + pgStock + ")": io.ErrUnexpectedEOF} })
	err := r.apply(t, r.queue(t, pgExtVals{Image: pgStock}))
	if err == nil || !strings.HasPrefix(err.Error(), "nothing was changed: ") {
		t.Fatalf("err = %v", err)
	}
	if got := r.rows(t); !got.equal(old) {
		t.Errorf("rows = %+v, want the old %+v", got, old)
	}
	if len(r.created(pgStock)) != 0 || len(r.created(pgCustom)) != 0 {
		t.Error("a container was created")
	}
	if _, ok := r.cursor(t); ok {
		t.Error("cursor left")
	}
}

// R4: a payload whose old values are no longer the stored ones is refused,
// nothing changed.
func TestPGExtStalePayload(t *testing.T) {
	t.Parallel()
	r := newPGExtRig(t)
	old := r.rows(t)
	st := pgExtState{Phase: pgExtQueued, Old: pgExtVals{Image: "ghcr.io/acme/pg:18.0"}, New: pgExtVals{Image: pgStock}}
	must(t, r.orch.setPGExtState(r.ctx, r.inst.ID, st))
	err := r.apply(t, st)
	if err == nil || !strings.Contains(err.Error(), "changed since it was queued; nothing was changed") {
		t.Fatalf("err = %v", err)
	}
	if got := r.rows(t); !got.equal(old) {
		t.Errorf("rows moved: %+v", got)
	}
	if _, ok := r.cursor(t); ok {
		t.Error("cursor left")
	}
}

// A settings save between the store and the put-back keeps its value: the job
// writes only its own columns (R3).
func TestPGExtWriteKeepsSettings(t *testing.T) {
	t.Parallel()
	r := newPGExtRig(t)
	_, m, err := r.orch.instanceOf(r.ctx, r.pg.ID)
	must(t, err)
	_, err = r.orch.operated.SetSettings(r.ctx, m, map[string]string{"work_mem": "8MB"})
	must(t, err)
	must(t, r.orch.pgExtWrite(r.ctx, r.pg.ID, pgExtVals{Image: pgStock, Preload: []string{"vchord"}, Extensions: []string{"vector"}}))
	tl, m, err := r.orch.instanceOf(r.ctx, r.pg.ID)
	must(t, err)
	if m.Settings["work_mem"] != "8MB" || tl.ImageRef != pgStock || !slices.Equal(m.Preload, []string{"vchord"}) {
		t.Errorf("row = %+v image %q", m, tl.ImageRef)
	}
}

// The deploy hook reconciles when the cursor owes one and clears it; without
// a cursor it does nothing.
func TestPGExtDueHook(t *testing.T) {
	t.Parallel()
	r := newPGExtRig(t)
	tl, _, err := r.orch.instanceOf(r.ctx, r.pg.ID)
	must(t, err)
	must(t, r.orch.pgExtDue(r.ctx, tl, io.Discard))
	must(t, r.orch.setPGExtState(r.ctx, r.inst.ID, pgExtState{Phase: pgExtDue, Due: true}))
	must(t, r.orch.pgExtDue(r.ctx, tl, io.Discard))
	if st, ok := r.cursor(t); ok {
		t.Errorf("a due cursor is left after the reconcile: %+v", st)
	}
	// inside a job's own deploy: only the flag goes, the cursor stays
	must(t, r.orch.setPGExtState(r.ctx, r.inst.ID, pgExtState{Phase: pgExtStart, Due: true}))
	must(t, r.orch.pgExtDue(r.ctx, tl, io.Discard))
	if st, ok := r.cursor(t); !ok || st.Due || st.Phase != pgExtStart {
		t.Errorf("cursor = %+v, %v", st, ok)
	}
}

// resumePGExt after a restart: store, start and putback queue the job again
// and it finishes the change; queued and check stored nothing, so the cursor
// goes; due stays; a change resumed too often is left.
func TestPGExtResume(t *testing.T) {
	t.Parallel()
	r := newPGExtRig(t)
	old := r.rows(t)
	want := pgExtVals{Image: pgStock, Extensions: []string{"vector"}}
	for _, ph := range []string{pgExtQueued, pgExtCheck} {
		must(t, r.orch.setPGExtState(r.ctx, r.inst.ID, pgExtState{Phase: ph, Old: old, New: want}))
		r.orch.resumePGExt(r.ctx)
		if _, ok := r.cursor(t); ok {
			t.Errorf("%s: cursor left", ph)
		}
	}
	must(t, r.orch.setPGExtState(r.ctx, r.inst.ID, pgExtState{Phase: pgExtDue, Due: true}))
	r.orch.resumePGExt(r.ctx)
	if st, ok := r.cursor(t); !ok || st.Phase != pgExtDue {
		t.Errorf("due cursor = %+v, %v", st, ok)
	}
	must(t, r.orch.setPGExtState(r.ctx, r.inst.ID, pgExtState{Phase: pgExtStore, Due: true, Old: old, New: want, Resumes: pgExtMaxResumes}))
	r.orch.resumePGExt(r.ctx)
	if js, err := r.orch.TileJobs(r.ctx, []string{r.pg.ID}, 10); err != nil || len(js) != 0 {
		t.Fatalf("a change resumed too often queued %v, %v", js, err)
	}

	must(t, r.orch.setPGExtState(r.ctx, r.inst.ID, pgExtState{Phase: pgExtStore, Due: true, Old: old, New: want}))
	r.orch.resumePGExt(r.ctx)
	js, err := r.orch.TileJobs(r.ctx, []string{r.pg.ID}, 10)
	if err != nil || len(js) != 1 || js[0].Kind != string(kindPGExt) {
		t.Fatalf("jobs = %+v, %v", js, err)
	}
	if got := r.wait(t, js[0].ID); got.State != "done" {
		t.Fatalf("job = %s: %s", got.State, got.Error)
	}
	if got := r.rows(t); !got.equal(want) {
		t.Errorf("rows = %+v, want %+v", got, want)
	}
	if _, ok := r.cursor(t); ok {
		t.Error("cursor left")
	}

	// a put-back in flight is finished first
	must(t, r.orch.pgExtWrite(r.ctx, r.pg.ID, want))
	must(t, r.orch.setPGExtState(r.ctx, r.inst.ID, pgExtState{Phase: pgExtPutBack, Old: old, New: want}))
	r.orch.resumePGExt(r.ctx)
	js, err = r.orch.TileJobs(r.ctx, []string{r.pg.ID}, 10)
	must(t, err)
	if len(js) == 0 {
		t.Fatal("no job for the put-back")
	}
	r.wait(t, js[0].ID)
	if got := r.rows(t); !got.equal(old) {
		t.Errorf("rows = %+v, want the old after the put-back", got)
	}
}

// The request: a running instance gets a job and the cursor; the job does it.
func TestPGExtSetRunning(t *testing.T) {
	t.Parallel()
	r := newPGExtRig(t)
	ctx := context.Background()
	by := roleP(r.org, "member")
	img := pgStock
	_, j, err := r.orch.SetPGExtensions(ctx, by, r.pg.ID, PGExtIn{Image: &img})
	if err != nil || j == nil || j.Kind != string(kindPGExt) {
		t.Fatalf("job = %+v, err %v", j, err)
	}
	if !slices.Contains(j.LockSet, r.pg.ID) {
		t.Errorf("lock set = %v", j.LockSet)
	}
	if got := r.wait(t, j.ID); got.State != "done" {
		t.Fatalf("job = %s: %s", got.State, got.Error)
	}
	if got := r.rows(t); got.Image != pgStock {
		t.Errorf("image = %q", got.Image)
	}
}

// A stopped instance stores the values, writes the due cursor and starts
// nothing (R8).
func TestPGExtSetStopped(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	pg, err := w.orch.CreateOperatedTile(ctx, Tile{EnvironmentID: w.env, Name: "pg"}, "postgres", 0)
	must(t, err)
	m, err := w.orch.operated.GetByTile(ctx, pg.ID)
	must(t, err)
	before := len(w.fake.Calls())
	by := roleP(w.org, "member")
	img, pre := "ghcr.io/acme/pg:18.1", []string{"vchord"}
	v, j, err := w.orch.SetPGExtensions(ctx, by, pg.ID, PGExtIn{Image: &img, Preload: &pre})
	if err != nil || j != nil {
		t.Fatalf("job %v, err %v", j, err)
	}
	if v.Image != img || !slices.Equal(v.Preload, pre) || v.Running || v.Official {
		t.Errorf("view = %+v", v)
	}
	for _, c := range w.fake.Calls()[before:] {
		if c.Method != "List" && c.Method != "HostInfo" {
			t.Errorf("a stopped instance made the call %s", c)
		}
	}
	st, ok := w.orch.pgExtOf(ctx, m.ID)
	if !ok || st.Phase != pgExtDue || !st.Due {
		t.Errorf("cursor = %+v, %v", st, ok)
	}
	// a due cursor does not block the next request
	more := []string{"vector"}
	if _, _, err := w.orch.SetPGExtensions(ctx, by, pg.ID, PGExtIn{Extensions: &more}); err != nil {
		t.Errorf("next request: %v", err)
	}
}

// Every refusal of a request leaves the rows as they were.
func TestPGExtSetRefusals(t *testing.T) {
	t.Parallel()
	r := newPGExtRig(t)
	ctx := context.Background()
	o := r.orch
	by := roleP(r.org, "member")
	old := r.rows(t)
	set := func(in PGExtIn) error {
		_, _, err := o.SetPGExtensions(ctx, by, r.pg.ID, in)
		return err
	}
	str := func(s ...string) *[]string { return &s }
	img := pgStock

	if err := set(PGExtIn{Extensions: str("Bad Name")}); err == nil || !strings.Contains(err.Error(), "is not an extension name") {
		t.Errorf("message 9: %v", err)
	}
	if err := set(PGExtIn{Preload: str("pg_stat_statements")}); err == nil || !strings.Contains(err.Error(), "always loaded") {
		t.Errorf("message 10: %v", err)
	}
	s3, err := o.CreateOperatedTile(ctx, Tile{EnvironmentID: r.env, Name: "files"}, "s3", 0)
	must(t, err)
	if _, _, err := o.SetPGExtensions(ctx, by, s3.ID, PGExtIn{Extensions: str("vector")}); err == nil || !strings.Contains(err.Error(), "only postgres does") {
		t.Errorf("message 11: %v", err)
	}

	// 29: a change in flight; due does not count
	must(t, o.setPGExtState(ctx, r.inst.ID, pgExtState{Phase: pgExtStart}))
	if err := set(PGExtIn{Image: &img}); err == nil || !strings.Contains(err.Error(), "is in progress") {
		t.Errorf("message 29: %v", err)
	}
	o.delPGExtState(ctx, r.inst.ID)

	// 26: a major change that can still be rolled back
	must(t, o.setPGUp(ctx, r.inst.ID, pgUp{From: 17, To: 18, Done: true, At: time.Now()}))
	if err := set(PGExtIn{Image: &img}); err == nil || !strings.Contains(err.Error(), "can still be rolled back to postgres 17") {
		t.Errorf("message 26: %v", err)
	}
	must(t, r.st.Settings.Delete(ctx, pgupKey+r.inst.ID))

	// 5: a name a slice lists
	_, m, err := o.instanceOf(ctx, r.pg.ID)
	must(t, err)
	_, err = o.operated.SetExtensions(ctx, m, nil, []string{"vector"})
	must(t, err)
	sl := r.db
	sl.Extensions = []string{"vector"}
	_, _, err = o.tiles.Update(ctx, r.db, sl)
	must(t, err)
	if err := set(PGExtIn{Extensions: str()}); err == nil || !strings.Contains(err.Error(), "vector is listed by slice db") {
		t.Errorf("message 5: %v", err)
	}

	// 1: a standby, custom image
	must(t, r.st.OperatedMembers.Create(ctx, store.OperatedMember{ID: "mem-1", InstanceID: r.inst.ID, Role: "replica", NodeID: "n", Volume: "v", State: "running", CreatedAt: time.Now()}))
	if err := set(PGExtIn{Preload: str("vchord")}); err == nil || !strings.Contains(err.Error(), "has a standby") {
		t.Errorf("message 1: %v", err)
	}
	if got := r.rows(t); got.Image != old.Image || len(got.Preload) != 0 || !slices.Equal(got.Extensions, []string{"vector"}) {
		t.Errorf("rows moved: %+v", got)
	}
	if _, ok := r.cursor(t); ok {
		t.Error("a refusal left a cursor")
	}
	// the list alone is no restart: the standby does not stop it (4b, the
	// server's offer, is the engine lane's read)
	more := []string{"vector"}
	if err := set(PGExtIn{Extensions: &more}); err != nil {
		t.Errorf("a list change with a standby: %v", err)
	}
}

// An official image to an official image makes no extensions job and no
// cursor, with or without a standby: today's minor update (R6).
func TestPGExtOfficialMinorStays(t *testing.T) {
	t.Parallel()
	r := newBranchRig(t)
	ctx := context.Background()
	by := roleP(r.org, "member")
	must(t, r.st.OperatedMembers.Create(ctx, store.OperatedMember{ID: "mem-1", InstanceID: r.inst.ID, Role: "replica", NodeID: "n", Volume: "v", State: "running", CreatedAt: time.Now()}))
	img := "postgres:18.99"
	_, j, err := r.orch.SetPGExtensions(ctx, by, r.pg.ID, PGExtIn{Image: &img})
	if err != nil {
		t.Fatal(err)
	}
	if j != nil && j.Kind == string(kindPGExt) {
		t.Errorf("job = %+v", j)
	}
	if _, ok := r.orch.pgExtOf(ctx, r.inst.ID); ok {
		t.Error("a minor update wrote a cursor")
	}
	if tl, _ := r.orch.tiles.Get(ctx, r.pg.ID); tl.ImageRef != img {
		t.Errorf("image = %q", tl.ImageRef)
	}
}

// UpdateTile: an image change that is not official to official is the image
// alone (message 27), and goes to the extensions path; while a change is in
// flight any edit of the tile is refused (29).
func TestPGExtUpdateTile(t *testing.T) {
	t.Parallel()
	r := newPGExtRig(t)
	ctx := context.Background()
	o := r.orch
	_, _, err := o.UpdateTile(ctx, r.pg.ID, func(x *Tile) error { x.ImageRef = pgStock; x.MemLimitMB = 512; return nil })
	if err == nil || !strings.Contains(err.Error(), "Change the image on its own.") {
		t.Fatalf("mixed edit: %v", err)
	}
	if got := r.rows(t); got.Image != pgCustom {
		t.Errorf("image = %q", got.Image)
	}
	_, j, err := o.UpdateTile(ctx, r.pg.ID, func(x *Tile) error { x.ImageRef = pgStock; return nil })
	if err != nil || j == nil || j.Kind != string(kindPGExt) {
		t.Fatalf("image-only edit: job %+v, err %v", j, err)
	}
	if got := r.wait(t, j.ID); got.State != "done" {
		t.Fatalf("job = %s: %s", got.State, got.Error)
	}
	must(t, o.setPGExtState(ctx, r.inst.ID, pgExtState{Phase: pgExtStore}))
	_, _, err = o.UpdateTile(ctx, r.pg.ID, func(x *Tile) error { x.MemLimitMB = 256; return nil })
	if err == nil || !strings.Contains(err.Error(), "is in progress") {
		t.Errorf("edit during a change: %v", err)
	}
	must(t, o.setPGExtState(ctx, r.inst.ID, pgExtState{Phase: pgExtDue, Due: true}))
	if _, _, err = o.UpdateTile(ctx, r.pg.ID, func(x *Tile) error { x.MemLimitMB = 256; return nil }); err != nil {
		t.Errorf("edit with only a due cursor: %v", err)
	}
}

// A slice's list: names the instance does not list are refused (3); a
// provisioned slice queues its own deploy (R18); one never deployed just
// stores; a branch copies the list and is refused on an instance that does
// not list it.
func TestPGExtSlices(t *testing.T) {
	t.Parallel()
	r := newPGExtRig(t)
	ctx := context.Background()
	o := r.orch
	if _, _, err := o.SetSliceExtensions(ctx, r.db.ID, []string{"vector"}); err == nil || !strings.Contains(err.Error(), "slice db: vector is not enabled on pg") {
		t.Fatalf("message 3: %v", err)
	}
	if _, _, err := o.SetSliceExtensions(ctx, r.db.ID, []string{"No"}); err == nil {
		t.Error("a bad name was taken")
	}
	_, m, err := o.instanceOf(ctx, r.pg.ID)
	must(t, err)
	_, err = o.operated.SetExtensions(ctx, m, nil, []string{"vector"})
	must(t, err)
	got, j, err := o.SetSliceExtensions(ctx, r.db.ID, []string{"vector"})
	if err != nil || j == nil || j.Kind != string(kindDeploy) || !slices.Contains(j.LockSet, r.db.ID) {
		t.Fatalf("provisioned: job %+v, err %v", j, err)
	}
	if !slices.Equal(got.Extensions, []string{"vector"}) {
		t.Errorf("row = %v", got.Extensions)
	}
	v, err := o.SliceExtensions(ctx, r.db.ID)
	must(t, err)
	if v.Instance != "pg" || !v.Provisioned || !slices.Equal(v.Allowed, []string{"vector"}) {
		t.Errorf("view = %+v", v)
	}
	// never deployed: stored, no job
	fresh, err := o.CreateSliceTile(ctx, r.env, "fresh", "shop:dev:pg", "", nil)
	must(t, err)
	if _, fj, err := o.SetSliceExtensions(ctx, fresh.ID, []string{"anything"}); err != nil || fj != nil {
		t.Errorf("not provisioned: job %v, err %v", fj, err)
	}
	// a branch copies the list
	r.wait(t, j.ID)
	by := roleP(r.org, "member")
	br, _, err := o.BranchSlice(ctx, by, r.db.ID, BranchIn{})
	must(t, err)
	if !slices.Equal(br.Extensions, []string{"vector"}) {
		t.Errorf("branch list = %v", br.Extensions)
	}
	// ... and is refused on an instance that does not list it
	other, err := o.CreateOperatedTile(ctx, Tile{EnvironmentID: r.env, Name: "other"}, "postgres", 0)
	must(t, err)
	_, _, err = o.BranchSlice(ctx, by, r.db.ID, BranchIn{Instance: "shop:dev:" + other.Slug})
	if _, ok := errs.IsConflict(err); !ok || !strings.Contains(err.Error(), "vector is not enabled on other") {
		t.Errorf("branch to another instance: %v", err)
	}
}

// A candidate (restore, major change) inherits the instance's startup modules
// and extensions, and a major change of a custom image is refused (message 2).
func TestPGExtCandidateAndMajor(t *testing.T) {
	t.Parallel()
	r := newPGExtRig(t)
	ctx := context.Background()
	o := r.orch
	tl, m, err := o.instanceOf(ctx, r.pg.ID)
	must(t, err)
	_, err = o.operated.SetExtensions(ctx, m, []string{"vchord"}, []string{"vector"})
	must(t, err)
	_, m, err = o.instanceOf(ctx, r.pg.ID)
	must(t, err)
	cand, err := o.newCandidateAs(ctx, pitrSource{tile: tl, inst: m}, "pg-cand", pgCustom, 18, nil)
	must(t, err)
	cm, err := o.operated.GetByTile(ctx, cand.ID)
	must(t, err)
	if !slices.Equal(cm.Preload, []string{"vchord"}) || !slices.Equal(cm.Extensions, []string{"vector"}) {
		t.Errorf("candidate = %v %v", cm.Preload, cm.Extensions)
	}

	if err := noCustomImageForChange(tl, m, 17); err == nil || !strings.Contains(err.Error(), "runs a custom image. Make a new instance on postgres 17") {
		t.Errorf("custom image: %v", err)
	}
	stock := tl
	stock.ImageRef = pgStock
	m.Preload = []string{"vchord"}
	if err := noCustomImageForChange(stock, m, 17); err == nil {
		t.Error("a startup module did not refuse the change")
	}
	m.Preload = nil
	if err := noCustomImageForChange(stock, m, 17); err != nil {
		t.Errorf("stock instance: %v", err)
	}
}

// ChangePGMajor refuses a custom image and an instance with startup modules
// (message 2).
func TestPGExtMajorRefused(t *testing.T) {
	t.Parallel()
	r := newPGUpRig(t)
	ctx := context.Background()
	o := r.orch
	by := roleP(r.org, "member")
	tl, m, err := o.instanceOf(ctx, r.pg.ID)
	must(t, err)
	must(t, r.st.Tiles.SetImageRef(ctx, r.pg.ID, "ghcr.io/acme/pg:"+strings.TrimPrefix(tl.ImageRef, "postgres:")))
	if _, err := o.ChangePGMajor(ctx, by, r.pg.ID, 18); err == nil || !strings.Contains(err.Error(), "runs a custom image. Make a new instance on postgres 18") {
		t.Errorf("custom image: %v", err)
	}
	must(t, r.st.Tiles.SetImageRef(ctx, r.pg.ID, tl.ImageRef))
	_, err = o.operated.SetExtensions(ctx, m, []string{"vchord"}, nil)
	must(t, err)
	if _, err := o.ChangePGMajor(ctx, by, r.pg.ID, 18); err == nil || !strings.Contains(err.Error(), "runs a custom image") {
		t.Errorf("startup module: %v", err)
	}
	if _, err := o.CheckPGUpgrade(ctx, by, r.pg.ID, 18); err == nil || !strings.Contains(err.Error(), "runs a custom image") {
		t.Errorf("check: %v", err)
	}
}

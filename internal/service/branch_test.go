package service

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/authz"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/securitylog"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

func roleP(org, role string) *Principal {
	id := "u-" + role
	return &Principal{User: User{ID: id}, Access: authz.User{ID: id, Active: true, Roles: map[string]string{org: role}}}
}

// branchRig is a postgres tile "pg" (running) with slice "db" provisioned on
// it, and an api tile holding a cred on it.
type branchRig struct {
	*world
	pg, db, api Tile
	inst        store.OperatedInstance
}

func newBranchRig(t *testing.T) *branchRig {
	t.Helper()
	w := newWorld(t)
	ctx := context.Background()
	pg, err := w.orch.CreateOperatedTile(ctx, Tile{EnvironmentID: w.env, Name: "pg"}, "postgres")
	must(t, err)
	db, err := w.orch.CreateSliceTile(ctx, w.env, "db", "shop:dev:pg", "")
	must(t, err)
	api := w.tile(t, "api", false)
	m, err := w.orch.operated.GetByTile(ctx, pg.ID)
	must(t, err)
	p := store.Provision{
		ID: uuid.NewString(), TileID: db.ID, InstanceID: m.ID,
		DBName: "shop_dev_db", DBUser: "shop_dev_db", DBPassword: "owner-pw", CreatedAt: time.Now(),
	}
	must(t, w.st.Provisions.Create(ctx, p))
	must(t, w.st.Bindings.Create(ctx, store.Binding{
		ID: uuid.NewString(), ProvisionID: p.ID, ConsumerTileID: api.ID, Access: "write",
		DBUser: "shop_dev_db_api", DBPassword: "api-pw", Outputs: "{}", CreatedAt: time.Now(),
	}))
	w.fake.Containers = append(w.fake.Containers, docker.Container{
		ID: "c-pg", Name: "c-pg", State: "running",
		Labels: map[string]string{tile.LabelTile: pg.ID, tile.LabelRole: "replica"},
	})
	return &branchRig{world: w, pg: pg, db: db, api: api, inst: m}
}

func TestParseExpiry(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]time.Duration{"3d": 72 * time.Hour, "36h": 36 * time.Hour, "90m": 90 * time.Minute} {
		if got, err := ParseExpiry(in); err != nil || got != want {
			t.Errorf("ParseExpiry(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "d", "0d", "-1d", "1.5d", "soon", "0s"} {
		if _, err := ParseExpiry(in); err == nil {
			t.Errorf("ParseExpiry(%q) took it", in)
		}
	}
}

// A branch is a new slice tile on the source's instance: random suffix, own
// row, drop on remove, no bindings, a seed job that dumps the source into
// it, and a security-log line.
func TestBranchSlice(t *testing.T) {
	t.Parallel()
	r := newBranchRig(t)
	ctx := context.Background()
	o := r.orch
	writer := roleP(r.org, "member")

	r.fake.ExecOut = "PGDMP-source-bytes"
	br, j, err := o.BranchSlice(ctx, writer, r.db.ID, BranchIn{Expires: "3d"})
	must(t, err)
	suffix, ok := strings.CutPrefix(br.Slug, "db-br-")
	if !ok || len(suffix) != 4 || br.Kind != tile.Slice || br.EnvironmentID != r.env {
		t.Fatalf("branch = %q kind %s", br.Slug, br.Kind)
	}
	if br.ProvisionFrom == nil || *br.ProvisionFrom != "shop:dev:pg" || br.OnRemove == nil || *br.OnRemove != tile.Drop {
		t.Errorf("branch row = %+v", br)
	}
	if br.ExpiresAt == nil || time.Until(*br.ExpiresAt) < 71*time.Hour || time.Until(*br.ExpiresAt) > 73*time.Hour {
		t.Errorf("expires_at = %v, want about 3d out", br.ExpiresAt)
	}
	if j.Kind != "seed" || !slices.Contains(j.LockSet, r.pg.ID) || !slices.Contains(j.LockSet, br.ID) {
		t.Errorf("job = %s %v", j.Kind, j.LockSet)
	}
	if bs, err := o.Bindings(ctx, br.ID); err != nil || len(bs) != 0 {
		t.Errorf("bindings on the branch = %v, %v; none are copied", bs, err)
	}

	// the job provisions the branch, then pipes the source's dump into its load
	if got := r.wait(t, j.ID); got.State != "done" {
		t.Fatalf("seed job = %s: %s", got.State, got.Error)
	}
	if got := string(r.fake.ExecIn); got != "PGDMP-source-bytes" {
		t.Errorf("load stdin = %q, want the source's dump", got)
	}
	var dumped, loaded bool
	for _, c := range r.fake.Calls() {
		args := strings.Join(c.Args, " ")
		switch {
		case c.Method == "ExecStream" && strings.Contains(args, "pg_dump"):
			dumped = strings.Contains(args, "-d shop_dev_db") && !strings.Contains(args, "PGPASSWORD")
		case c.Method == "ExecStream" && strings.Contains(args, "pg_restore"):
			loaded = strings.Contains(args, "-d shop_dev_db_br_"+suffix)
		}
	}
	if !dumped || !loaded {
		t.Errorf("pg_dump of the source = %v, pg_restore into the branch = %v", dumped, loaded)
	}

	es, err := o.SecurityLog(ctx, SecurityFilter{Action: securitylog.SliceBranch})
	must(t, err)
	if len(es) != 1 || es[0].ActorID != "u-member" || es[0].TargetID != br.ID ||
		!strings.Contains(es[0].Detail, "shop:dev:db") || !strings.Contains(es[0].Detail, "at=now") {
		t.Errorf("security log = %+v", es)
	}
	if strings.Contains(es[0].Detail, "owner-pw") {
		t.Error("the log line carries a password")
	}

	// a branch of a branch drops the old suffix
	if _, ok, err := o.operated.ProvisionOf(ctx, br.ID); err != nil || !ok {
		t.Fatalf("branch provisioned = %v, %v", ok, err)
	}
	br2, _, err := o.BranchSlice(ctx, writer, br.ID, BranchIn{})
	must(t, err)
	if strings.Count(br2.Slug, "-br-") != 1 || br2.ExpiresAt != nil {
		t.Errorf("branch of a branch = %q, expires %v", br2.Slug, br2.ExpiresAt)
	}
}

func TestBranchSliceRefusals(t *testing.T) {
	t.Parallel()
	r := newBranchRig(t)
	ctx := context.Background()
	o := r.orch
	writer := roleP(r.org, "member")

	if _, _, err := o.BranchSlice(ctx, roleP(r.org, "viewer"), r.db.ID, BranchIn{}); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("viewer = %v, want refused", err)
	}
	if _, _, err := o.BranchSlice(ctx, nil, r.db.ID, BranchIn{}); err == nil {
		t.Error("no principal branched")
	}
	for name, in := range map[string]BranchIn{
		"bad expiry":   {Expires: "soon"},
		"unknown env":  {Env: "nope"},
		"unknown stk":  {Env: "nope:dev"},
		"odd instance": {Instance: "a:b"},
	} {
		if _, _, err := o.BranchSlice(ctx, writer, r.db.ID, in); err == nil {
			t.Errorf("%s: branched", name)
		} else if _, ok := errs.IsInvalid(err); !ok {
			t.Errorf("%s: err = %v, want invalid", name, err)
		}
	}
	// the instance named must be postgres
	s3, err := o.CreateOperatedTile(ctx, Tile{EnvironmentID: r.env, Name: "files"}, "s3")
	must(t, err)
	if _, _, err := o.BranchSlice(ctx, writer, r.db.ID, BranchIn{Instance: "shop:dev:" + s3.Slug}); err == nil {
		t.Error("branched onto an s3 instance")
	}
	// not a slice, not provisioned
	if _, _, err := o.BranchSlice(ctx, writer, r.api.ID, BranchIn{}); err == nil {
		t.Error("branched an image tile")
	}
	fresh, err := o.CreateSliceTile(ctx, r.env, "fresh", "shop:dev:pg", "")
	must(t, err)
	if _, _, err := o.BranchSlice(ctx, writer, fresh.ID, BranchIn{}); err == nil {
		t.Error("branched a slice that was never deployed")
	} else if _, ok := errs.IsConflict(err); !ok {
		t.Errorf("undeployed = %v, want conflict", err)
	}
	// an s3 slice
	bucket, err := o.CreateSliceTile(ctx, r.env, "bucket", "shop:dev:"+s3.Slug, "")
	must(t, err)
	m3, err := o.operated.GetByTile(ctx, s3.ID)
	must(t, err)
	must(t, r.st.Provisions.Create(ctx, store.Provision{
		ID: uuid.NewString(), TileID: bucket.ID, InstanceID: m3.ID, DBName: "b", DBUser: "b", CreatedAt: time.Now(),
	}))
	if _, _, err := o.BranchSlice(ctx, writer, bucket.ID, BranchIn{}); err == nil || !strings.Contains(err.Error(), "only postgres") {
		t.Errorf("s3 slice = %v, want the only-postgres refusal", err)
	}
}

// --env moves the branch to another env of the stack; the source instance
// must admit it, and the caller needs write there too.
func TestBranchSliceToAnotherEnv(t *testing.T) {
	t.Parallel()
	r := newBranchRig(t)
	ctx := context.Background()
	o := r.orch
	writer := roleP(r.org, "member")
	prod, err := o.CreateEnv(ctx, r.stack, "prod", EnvSpec{Type: "static", FromKind: "branch", FromBranch: "main"})
	must(t, err)

	if _, _, err := o.BranchSlice(ctx, writer, r.db.ID, BranchIn{Env: "prod"}); err == nil {
		t.Fatal("prod took a branch of an instance that does not admit it")
	} else if _, ok := errs.IsConflict(err); !ok {
		t.Errorf("not admitted = %v, want conflict", err)
	}
	if _, err := o.SetOperatedAllow(ctx, r.pg.ID, []string{"acme:shop:*"}); err != nil {
		t.Fatal(err)
	}
	br, _, err := o.BranchSlice(ctx, writer, r.db.ID, BranchIn{Env: "prod"})
	must(t, err)
	if br.EnvironmentID != prod.ID || *br.ProvisionFrom != "shop:dev:pg" {
		t.Errorf("branch = env %s from %q", br.EnvironmentID, *br.ProvisionFrom)
	}
}

func TestSweepExpired(t *testing.T) {
	t.Parallel()
	r := newBranchRig(t)
	ctx := context.Background()
	o := r.orch
	mk := func(name string, exp *time.Time) Tile {
		t.Helper()
		s, err := o.CreateSliceTile(ctx, r.env, name, "shop:dev:pg", "")
		must(t, err)
		s.ExpiresAt = exp
		must(t, r.st.Tiles.Update(ctx, s))
		return s
	}
	past, future := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	old, fresh, never := mk("old", &past), mk("fresh", &future), mk("never", nil)

	o.sweepExpired(ctx)

	eventually := func(what string, f func() bool) {
		t.Helper()
		for range 500 {
			if f() {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", what)
	}
	eventually("the expired slice to go", func() bool {
		_, err := r.st.Tiles.Get(ctx, old.ID)
		return err != nil
	})
	for _, s := range []Tile{fresh, never, r.db} {
		if _, err := r.st.Tiles.Get(ctx, s.ID); err != nil {
			t.Errorf("%s was swept: %v", s.Slug, err)
		}
	}
}

// An expired branch that was loaded goes through the normal delete: its own
// database is dropped, the source's is left alone.
func TestSweepDropsOnlyTheBranchDatabase(t *testing.T) {
	t.Parallel()
	r := newBranchRig(t)
	ctx := context.Background()
	o := r.orch
	r.fake.ExecOut = "PGDMP-source-bytes"
	br, j, err := o.BranchSlice(ctx, roleP(r.org, "member"), r.db.ID, BranchIn{Expires: "1h"})
	must(t, err)
	if got := r.wait(t, j.ID); got.State != "done" {
		t.Fatalf("seed job = %s: %s", got.State, got.Error)
	}
	p, ok, err := o.operated.ProvisionOf(ctx, br.ID)
	if err != nil || !ok || p.DBName == "shop_dev_db" {
		t.Fatalf("branch provision = %+v, %v, %v", p, ok, err)
	}
	// the instance's volume is where the backup before a drop looks
	must(t, r.st.Volumes.Create(ctx, store.Volume{
		ID: uuid.NewString(), ScopeKind: "env", ScopeID: r.env, Slug: "pg-data", Name: "pg-data", InstanceID: &r.inst.ID, CreatedAt: time.Now(),
	}))
	past := time.Now().Add(-time.Minute)
	br.ExpiresAt = &past
	must(t, r.st.Tiles.Update(ctx, br))

	o.sweepExpired(ctx)
	for range 500 {
		if _, err := r.st.Tiles.Get(ctx, br.ID); err != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := r.st.Tiles.Get(ctx, br.ID); err == nil {
		js, _ := o.TileJobs(ctx, []string{br.ID}, 3)
		t.Fatalf("the expired branch is still there; jobs: %+v", js)
	}
	var dropped, source bool
	for _, c := range r.fake.Calls() {
		args := strings.Join(c.Args, " ")
		if c.Method != "Exec" || !strings.Contains(args, "DROP DATABASE") {
			continue
		}
		dropped = dropped || strings.Contains(args, `DROP DATABASE IF EXISTS "`+p.DBName+`"`)
		source = source || strings.Contains(args, `DROP DATABASE IF EXISTS "shop_dev_db"`)
	}
	if !dropped || source {
		t.Errorf("branch database dropped = %v, source database dropped = %v", dropped, source)
	}
	if _, err := r.st.Tiles.Get(ctx, r.db.ID); err != nil {
		t.Errorf("the source slice is gone: %v", err)
	}

	// a branch is thrown away without the instance backup a normal drop takes
	dumps := func() int {
		n := 0
		for _, c := range r.fake.Calls() {
			if strings.Contains(strings.Join(c.Args, " "), "pg_dumpall") {
				n++
			}
		}
		return n
	}
	if n := dumps(); n != 0 {
		t.Errorf("deleting a branch ran %d instance backups", n)
	}
	plain, err := o.CreateSliceTile(ctx, r.env, "plain", "shop:dev:pg", "")
	must(t, err)
	must(t, r.st.Provisions.Create(ctx, store.Provision{
		ID: uuid.NewString(), TileID: plain.ID, InstanceID: r.inst.ID, DBName: "shop_dev_plain", DBUser: "shop_dev_plain", CreatedAt: time.Now(),
	}))
	_, err = o.SetSliceOnRemove(ctx, plain.ID, tile.Drop)
	must(t, err)
	dj, err := o.DeleteTile(ctx, plain.ID)
	must(t, err)
	if got := r.wait(t, dj.ID); got.State != "done" {
		t.Fatalf("delete job = %s: %s", got.State, got.Error)
	}
	if dumps() == 0 {
		t.Error("deleting a normal drop slice took no backup")
	}
}

func TestSliceCreds(t *testing.T) {
	t.Parallel()
	r := newBranchRig(t)
	ctx := context.Background()
	o := r.orch

	c, err := o.SliceCreds(ctx, roleP(r.org, "member"), r.db.ID)
	must(t, err)
	if c.Database != "shop_dev_db" || c.User != "shop_dev_db" || c.Password != "owner-pw" || c.Port != 5432 {
		t.Errorf("creds = %+v, want the slice's own owner", c)
	}
	if c.Password == "api-pw" {
		t.Error("a bound consumer's cred came back")
	}
	es, err := o.SecurityLog(ctx, SecurityFilter{Action: securitylog.SecretReveal})
	must(t, err)
	if len(es) != 1 || es[0].ActorID != "u-member" || es[0].TargetID != r.db.ID || strings.Contains(es[0].Detail, "owner-pw") {
		t.Errorf("reveal log = %+v", es)
	}
	if _, err := o.SliceCreds(ctx, roleP(r.org, "viewer"), r.db.ID); err == nil {
		t.Error("a viewer read the creds")
	}
	if _, err := o.SliceCreds(ctx, roleP(r.org, "member"), r.api.ID); err == nil {
		t.Error("creds of an image tile")
	}
	fresh, err := o.CreateSliceTile(ctx, r.env, "fresh", "shop:dev:pg", "")
	must(t, err)
	if _, _, err := o.DialSlice(ctx, fresh.ID); err == nil {
		t.Error("dialed a slice that was never deployed")
	}
}

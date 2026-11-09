package operated_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	mflow "github.com/FyrmForge/stackr/internal/service/internal/flow/operated"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/operated"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/opmember"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/stack"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
	"github.com/FyrmForge/stackr/internal/service/internal/s3"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

var ctx = context.Background()

type vipStub struct{}

func (vipStub) Set(context.Context, string, []string, []string) error {
	return nil
}

func (vipStub) Remove(context.Context, string) error {
	return nil
}

type s3Fake struct {
	made, dropped, users, removed []string
	grants                        map[string]string // user -> bucket:read|write
	protect                       []s3.Protect      // each Protect call, in order
	restored                      string            // "<prefix>|<exact>|<at>" of the last restore
}

func (s *s3Fake) Protect(_ context.Context, _ string, p s3.Protect) error {
	s.protect = append(s.protect, p)
	return nil
}

func (s *s3Fake) Restore(_ context.Context, _, prefix string, exact bool, at time.Time, _ func(s3.RestoreResult)) (s3.RestoreResult, error) {
	s.restored = fmt.Sprintf("%s|%t|%d", prefix, exact, at.Unix())
	return s3.RestoreResult{Restored: 1}, nil
}

func (s *s3Fake) Sizes(context.Context, string) (int64, int64, error) { return 10, 4, nil }

func (s *s3Fake) Ping(context.Context) error { return nil }

func (s *s3Fake) AddUser(_ context.Context, key, _ string) error {
	s.users = append(s.users, key)
	return nil
}

func (s *s3Fake) GrantUser(_ context.Context, user, bucket string, write bool) error {
	if s.grants == nil {
		s.grants = map[string]string{}
	}
	access := "read"
	if write {
		access = "write"
	}
	s.grants[user] = bucket + ":" + access
	return nil
}

func (s *s3Fake) RemoveUser(_ context.Context, user string) error {
	s.removed = append(s.removed, user)
	delete(s.grants, user)
	return nil
}

func (s *s3Fake) CreateBucket(_ context.Context, n string) error {
	s.made = append(s.made, n)
	return nil
}

func (s *s3Fake) DropBucket(_ context.Context, n string) error {
	s.dropped = append(s.dropped, n)
	return nil
}

func (s *s3Fake) SetPublic(context.Context, string, bool) error { return nil }

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

type world struct {
	f        *mflow.Flow
	fake     *dockerfake.Fake
	s3       *s3Fake
	db       store.Tile // the instance tile
	orders   store.Tile // a slice tile on it
	api, rep store.Tile // consumers
	instance store.OperatedInstance
}

// setup: an env with a running operated tile "db" of engine, a slice tile
// "orders" and consumers "api" and "reporter".
func setup(t *testing.T, engine string) *world {
	st := servicetest.Store(t)
	fake := dockerfake.New()
	s3 := &s3Fake{}
	tiles := tile.New(st.Tiles, fake, vipStub{})
	f := &mflow.Flow{
		Tiles:     tiles,
		Instances: operated.New(st.OperatedInstances, st.Provisions, st.Bindings),
		Volumes:   volume.New(st.Volumes, fake),
		Envs:      environment.New(st.Environments, fake),
		Stacks:    stack.New(st.Stacks),
		Members:   opmember.New(st.OperatedMembers),
		S3: func(string, string, string) mflow.S3Admin {
			return s3
		},
		ReadyWait: 20 * time.Millisecond,
		ReadyPoll: time.Millisecond,
	}
	orgID := uuid.NewString()
	stackID := uuid.NewString()
	envID := uuid.NewString()
	now := time.Now()
	must(t, st.Orgs.Create(ctx, store.Org{
		ID:        orgID,
		Name:      "o",
		Slug:      "o",
		EnvColors: "{}",
		Settings:  "{}",
		CreatedAt: now,
	}))
	must(t, st.Stacks.Create(ctx, store.Stack{
		ID:        stackID,
		OrgID:     orgID,
		Name:      "s",
		Slug:      "s",
		Settings:  "{}",
		CreatedAt: now,
	}))
	must(t, st.Environments.Create(ctx, store.Environment{
		ID:         envID,
		StackID:    stackID,
		Name:       "dev",
		Slug:       "dev",
		Type:       "static",
		Settings:   "{}",
		Network:    "n",
		FromKind:   "branch",
		FromBranch: "main",
		CreatedAt:  now,
	}))
	mk := func(name, kind string) store.Tile {
		row := store.Tile{
			StackID:       stackID,
			EnvironmentID: envID,
			Name:          name,
			Kind:          kind,
		}
		switch kind {
		case tile.Image:
			row.ImageRef = name + ":1"
		case tile.Slice:
			from := "s:dev:db"
			row.ProvisionFrom = &from
		}
		out, err := tiles.Create(ctx, row)
		must(t, err)
		return out
	}
	w := &world{
		f:      f,
		fake:   fake,
		s3:     s3,
		db:     mk("db", tile.Operated),
		orders: mk("orders", tile.Slice),
		api:    mk("api", tile.Image),
		rep:    mk("reporter", tile.Image),
	}
	m, err := f.Instances.Create(ctx, w.db.ID, engine, "stackr", "")
	must(t, err)
	if engine == "postgres" {
		m, err = f.Instances.SetMajor(ctx, m, 17)
		must(t, err)
	}
	w.instance = m
	// a slice makes the instance's pooler: it can be pulled and started, and
	// its auth lookup proves itself; every other exec answers ExecOut
	fake.RunID = "pool1"
	fake.Details = map[string]docker.Detail{"pool1": {Running: true, State: "running"}}
	tiles.Gate = tile.Gate{Poll: time.Millisecond, Grace: time.Millisecond, Deadline: time.Second}
	f.Pull = func(context.Context, string) error { return nil }
	fake.ExecFn = func(cmd []string) string {
		if strings.Contains(strings.Join(cmd, " "), "get_auth(") {
			return "1,0,0"
		}
		return fake.ExecOut
	}
	fake.Containers = []docker.Container{
		{
			ID:    "c1",
			State: "running",
			Labels: map[string]string{
				tile.LabelTile: w.db.ID,
				tile.LabelRole: "replica",
			},
		},
	}
	return w
}

// execs is every Exec from call n on, one per line.
func execs(f *dockerfake.Fake, n int) string {
	var b strings.Builder
	for _, c := range f.Calls()[n:] {
		// the pooler's own work (its role, its lookup proof, the live
		// max_connections, its reload) is tested in pooler_flow_test.go
		if p := strings.Join(c.Args, " ") + c.Stdin; strings.Contains(p, "stackr_pooler") || strings.Contains(p, "kill -HUP") ||
			strings.Contains(p, "'max_connections'") || strings.Contains(p, "bool_or(pending_restart)") {
			continue
		}
		if c.Method == "Exec" || c.Method == "ExecStream" {
			b.WriteString(strings.Join(c.Args, " ") + "\n" + c.Stdin)
		}
	}
	return b.String()
}

func contains(t *testing.T, what, got string, want ...string) {
	t.Helper()
	for _, s := range want {
		if !strings.Contains(got, s) {
			t.Errorf("%s: no %q in\n%s", what, s, got)
		}
	}
}

// url: pooled is for postgres only: a bucket slice refuses it at provision.
func TestPooledRefusedOnBucket(t *testing.T) {
	w := setup(t, "s3")
	cur, err := w.f.Tiles.Get(ctx, w.orders.ID)
	must(t, err)
	next, pooled := cur, "pooled"
	next.SliceURL = &pooled
	next, _, err = w.f.Tiles.Update(ctx, cur, next)
	must(t, err)
	if _, err := w.f.Provision(ctx, w.db, next); err == nil || !strings.Contains(err.Error(), "applies to postgres slices only") {
		t.Errorf("provision = %v, want the postgres-only refusal", err)
	}
}

// A slice's url setting decides what url, host and port point at: Republish
// moves the stored outputs, and a same-access Bind reads the setting too.
func TestSliceURLSetting(t *testing.T) {
	w := setup(t, "postgres")
	w.fake.ExecOut = "0,0"
	p, err := w.f.Provision(ctx, w.db, w.orders)
	must(t, err)
	w.fake.ExecOut = "0"
	b, err := w.f.Bind(ctx, p, w.api, "write")
	must(t, err)
	host := "db-" + w.instance.ID[:8]
	read := func(b store.Binding) map[string]string {
		out, err := operated.Outputs(b)
		must(t, err)
		return out
	}
	if out := read(b); out["host"] != host || out["url"] != out["url_direct"] || out["url_read"] != out["url_direct"] {
		t.Fatalf("default outputs %v, want direct", out)
	}
	set := func(u string) {
		cur, err := w.f.Tiles.Get(ctx, w.orders.ID)
		must(t, err)
		next := cur
		next.SliceURL = &u
		_, _, err = w.f.Tiles.Update(ctx, cur, next)
		must(t, err)
	}
	set("pooled")
	moved, err := w.f.Republish(ctx, w.db)
	must(t, err)
	if len(moved) != 1 || moved[0] != w.api.ID {
		t.Fatalf("moved = %v, want the api consumer", moved)
	}
	bs, err := w.f.Instances.Bindings(ctx, p.ID)
	must(t, err)
	out := read(bs[0])
	if out["host"] != host+"-pooler" || out["url"] != out["url_pooled"] || out["url_read"] != out["url_pooled"] || out["host_direct"] != host {
		t.Errorf("pooled outputs %v", out)
	}
	set("direct")
	b, err = w.f.Bind(ctx, p, w.api, "write")
	must(t, err)
	if out := read(b); out["host"] != host || out["url"] != out["url_direct"] {
		t.Errorf("after switching back %v, want direct", out)
	}
}

func TestPostgresSliceAndBindings(t *testing.T) {
	w := setup(t, "postgres")
	w.fake.ExecOut = "0,0"
	p, err := w.f.Provision(ctx, w.db, w.orders)
	must(t, err)
	contains(t, "provision", execs(w.fake, 0),
		`CREATE ROLE "s_dev_orders"`,
		`CREATE DATABASE "s_dev_orders" OWNER "s_dev_orders"`,
		"SET log_min_error_statement = PANIC",
		"psql -h /var/run/postgresql -q ",
	)
	if p.DBName != "s_dev_orders" || p.DBUser != "s_dev_orders" {
		t.Errorf("provision = %+v", p)
	}

	// A second Provision only verifies: ALTER the owner, no CREATE.
	w.fake.ExecOut = "1,1"
	n := len(w.fake.Calls())
	again, err := w.f.Provision(ctx, w.db, w.orders)
	must(t, err)
	if ex := execs(w.fake, n); again.ID != p.ID || !strings.Contains(ex, `ALTER ROLE "s_dev_orders"`) || strings.Contains(ex, "CREATE ROLE") || strings.Contains(ex, "CREATE DATABASE") {
		t.Errorf("re-provision %s:\n%s", again.ID, ex)
	}

	// api writes, reporter reads; each its own user, in the slice's db.
	w.fake.ExecOut = "0"
	n = len(w.fake.Calls())
	api, err := w.f.Bind(ctx, p, w.api, "write")
	must(t, err)
	contains(t, "bind api", execs(w.fake, n),
		"-d s_dev_orders",
		`CREATE ROLE "s_dev_orders_api" LOGIN PASSWORD`,
		`GRANT ALL ON DATABASE "s_dev_orders" TO "s_dev_orders_api"`,
	)
	out, err := operated.Outputs(api)
	must(t, err)
	host := "db-" + w.instance.ID[:8]
	if api.DBUser != "s_dev_orders_api" || out["host"] != host || out["user"] != "s_dev_orders_api" || out["database"] != "s_dev_orders" ||
		out["url"] != "postgres://s_dev_orders_api:"+api.DBPassword+"@"+host+":5432/s_dev_orders" {
		t.Errorf("api binding %+v, outputs %v", api, out)
	}
	n = len(w.fake.Calls())
	rep, err := w.f.Bind(ctx, p, w.rep, "read")
	must(t, err)
	contains(t, "bind reporter", execs(w.fake, n),
		`CREATE ROLE "s_dev_orders_reporter"`,
		`GRANT SELECT ON ALL TABLES IN SCHEMA public TO "s_dev_orders_reporter"`,
		`ALTER DEFAULT PRIVILEGES FOR ROLE "s_dev_orders_api" IN SCHEMA public GRANT SELECT ON TABLES TO "s_dev_orders_reporter"`,
	)

	// The same access again is nothing.
	n = len(w.fake.Calls())
	_, err = w.f.Bind(ctx, p, w.api, "write")
	must(t, err)
	if ex := execs(w.fake, n); ex != "" {
		t.Errorf("same access ran:\n%s", ex)
	}

	// Another access re-grants in place: user and password stay.
	n = len(w.fake.Calls())
	moved, err := w.f.Bind(ctx, p, w.rep, "write")
	must(t, err)
	ex := execs(w.fake, n)
	contains(t, "re-grant", ex,
		`REVOKE ALL ON DATABASE "s_dev_orders" FROM "s_dev_orders_reporter"`,
		`GRANT ALL ON DATABASE "s_dev_orders" TO "s_dev_orders_reporter"`,
	)
	if strings.Contains(ex, "ROLE \"s_dev_orders_reporter\" LOGIN") || moved.DBUser != rep.DBUser ||
		moved.DBPassword != rep.DBPassword || moved.Access != "write" {
		t.Errorf("re-grant %+v:\n%s", moved, ex)
	}

	// Unbind: the user's tables pass to the owner, then the user goes.
	n = len(w.fake.Calls())
	must(t, w.f.Unbind(ctx, moved))
	contains(t, "unbind", execs(w.fake, n),
		`REASSIGN OWNED BY "s_dev_orders_reporter" TO "s_dev_orders"`,
		`DROP OWNED BY "s_dev_orders_reporter"`,
		`DROP ROLE IF EXISTS "s_dev_orders_reporter"`,
	)
	if bs, err := w.f.Instances.Bindings(ctx, p.ID); err != nil || len(bs) != 1 {
		t.Errorf("bindings after unbind = %v, %v", bs, err)
	}

	// on_remove drop: every binding, then the database, then the row.
	n = len(w.fake.Calls())
	must(t, w.f.Drop(ctx, p, true, io.Discard))
	contains(t, "drop", execs(w.fake, n),
		`DROP ROLE IF EXISTS "s_dev_orders_api"`,
		`DROP DATABASE IF EXISTS "s_dev_orders" WITH (FORCE)`,
	)
	if _, ok, err := w.f.Instances.ProvisionOf(ctx, w.orders.ID); err != nil || ok {
		t.Errorf("provision after drop: %v %v", ok, err)
	}
}

// A slice's name is its address (DECIDE 197); one another row on the
// instance already holds, a binding's user included, is refused.
func TestNamesAreUniqueOnTheInstance(t *testing.T) {
	w := setup(t, "postgres")
	w.fake.ExecOut = "0,0"
	p, err := w.f.Provision(ctx, w.db, w.orders)
	must(t, err)
	_, err = w.f.Bind(ctx, p, w.api, "write")
	must(t, err)
	from := "s:dev:db"
	clash, err := w.f.Tiles.Create(ctx, store.Tile{
		StackID:       w.api.StackID,
		EnvironmentID: w.api.EnvironmentID,
		Name:          "orders-api",
		Kind:          tile.Slice,
		ProvisionFrom: &from,
	})
	must(t, err)
	if _, err := w.f.Provision(ctx, w.db, clash); !isConflict(err) {
		t.Errorf("clashing slice s_dev_orders_api = %v, want a conflict", err)
	}
}

func TestKeepAndTeardownNeedsForce(t *testing.T) {
	w := setup(t, "postgres")
	w.fake.ExecOut = "0,0"
	p, err := w.f.Provision(ctx, w.db, w.orders)
	must(t, err)
	_, err = w.f.Bind(ctx, p, w.api, "write")
	must(t, err)
	n := len(w.fake.Calls())
	must(t, w.f.Drop(ctx, p, false, io.Discard))
	if ex := execs(w.fake, n); strings.Contains(ex, "DROP DATABASE") || !strings.Contains(ex, `DROP ROLE IF EXISTS "s_dev_orders_api"`) {
		t.Errorf("keep:\n%s", ex)
	}
	if _, ok, _ := w.f.Instances.ProvisionOf(ctx, w.orders.ID); ok {
		t.Error("a kept slice kept its row")
	}

	// Added back, it finds its kept database by name: no CREATE, the owner
	// takes a fresh password.
	w.fake.ExecOut = "1,1"
	n = len(w.fake.Calls())
	back, err := w.f.Provision(ctx, w.db, w.orders)
	must(t, err)
	ex := execs(w.fake, n)
	if back.DBName != "s_dev_orders" || back.ID == p.ID || strings.Contains(ex, "CREATE ROLE") || strings.Contains(ex, "CREATE DATABASE") ||
		!strings.Contains(ex, `ALTER ROLE "s_dev_orders"`) {
		t.Errorf("re-added %+v:\n%s", back, ex)
	}
	if err := w.f.Teardown(ctx, w.db, false, io.Discard); !isConflict(err) {
		t.Errorf("teardown with a held slice = %v, want a conflict", err)
	}
	n = len(w.fake.Calls())
	must(t, w.f.Teardown(ctx, w.db, true, io.Discard))
	contains(t, "forced teardown", execs(w.fake, n), `DROP DATABASE IF EXISTS "s_dev_orders"`)
	if _, err := w.f.Instances.GetByTile(ctx, w.db.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("instance after teardown: %v", err)
	}
}

func isConflict(err error) bool {
	_, ok := errs.IsConflict(err)
	return ok
}

// s3: a bucket per slice whose own cred is the root one; every consumer gets
// a user with a bucket-scoped policy at its access, rewritten on re-grant
// and removed with the binding.
func TestS3BucketPerSlice(t *testing.T) {
	w := setup(t, "s3")
	p, err := w.f.Provision(ctx, w.db, w.orders)
	must(t, err)
	if !slices.Equal(w.s3.made, []string{"s-dev-orders"}) || p.DBUser != "stackr" {
		t.Errorf("made %v, user %q", w.s3.made, p.DBUser)
	}
	b, err := w.f.Bind(ctx, p, w.rep, "read")
	must(t, err)
	out, err := operated.Outputs(b)
	must(t, err)
	user := "s-dev-orders-reporter"
	if b.DBUser != user || b.Access != "read" || out["bucket"] != "s-dev-orders" ||
		out["access_key"] != user || out["secret_key"] != b.DBPassword ||
		out["endpoint"] != "http://db-"+w.instance.ID[:8]+":9000" {
		t.Errorf("binding %+v, outputs %v", b, out)
	}
	if !slices.Equal(w.s3.users, []string{user}) || w.s3.grants[user] != "s-dev-orders:read" {
		t.Errorf("users %v, grants %v", w.s3.users, w.s3.grants)
	}
	b, err = w.f.Bind(ctx, p, w.rep, "write")
	must(t, err)
	if b.DBUser != user || len(w.s3.users) != 1 || w.s3.grants[user] != "s-dev-orders:write" {
		t.Errorf("re-grant: binding %+v, users %v, grants %v", b, w.s3.users, w.s3.grants)
	}
	must(t, w.f.Drop(ctx, p, true, io.Discard))
	if !slices.Equal(w.s3.dropped, []string{"s-dev-orders"}) || !slices.Equal(w.s3.removed, []string{user}) {
		t.Errorf("dropped %v, removed %v", w.s3.dropped, w.s3.removed)
	}
}

// Every bucket is versioned for 7 days unless its slice says otherwise; an
// edit, a replay and a restore all go through the same settings.
func TestS3Protection(t *testing.T) {
	w := setup(t, "s3")
	p, err := w.f.Provision(ctx, w.db, w.orders)
	must(t, err)
	if got := w.s3.protect; len(got) != 1 || got[0].VersionDays != 7 || got[0].QuotaBytes != 0 || len(got[0].CORS) != 0 {
		t.Fatalf("default protection %+v", got)
	}

	cur := w.orders
	days, gb := 30, 2
	cur.VersionDays, cur.QuotaGB, cur.CORSOrigins = &days, &gb, []string{"https://app.example.com"}
	orders, _, err := w.f.Tiles.Update(ctx, w.orders, cur)
	must(t, err)
	must(t, w.f.Resync(ctx, orders))
	want := s3.Protect{VersionDays: 30, QuotaBytes: 2 << 30, CORS: []string{"https://app.example.com"}}
	if got := w.s3.protect[len(w.s3.protect)-1]; got.VersionDays != want.VersionDays || got.QuotaBytes != want.QuotaBytes || !slices.Equal(got.CORS, want.CORS) {
		t.Fatalf("resync wrote %+v, want %+v", got, want)
	}

	// A replay after a restored volume writes the slice's settings, not the defaults.
	must(t, w.f.Replay(ctx, w.db, io.Discard))
	if got := w.s3.protect[len(w.s3.protect)-1]; got.VersionDays != 30 {
		t.Fatalf("replay wrote %+v", got)
	}

	// Versioning off is 0 days, not "unset".
	zero := 0
	cur = orders
	cur.VersionDays = &zero
	orders, _, err = w.f.Tiles.Update(ctx, orders, cur)
	must(t, err)
	must(t, w.f.Resync(ctx, orders))
	if got := w.s3.protect[len(w.s3.protect)-1]; got.VersionDays != 0 {
		t.Fatalf("off wrote %+v", got)
	}

	// A caller holding an older copy of the slice tile still writes the latest settings.
	if _, err := w.f.Provision(ctx, w.db, w.orders); err != nil {
		t.Fatal(err)
	}
	if got := w.s3.protect[len(w.s3.protect)-1]; got.VersionDays != 0 {
		t.Fatalf("stale tile wrote %+v, want the latest row (versioning off)", got)
	}

	at := time.Unix(1700000000, 0)
	must(t, w.f.RestoreBucket(ctx, p, "photos/", false, at, io.Discard))
	if w.s3.restored != "photos/|false|1700000000" {
		t.Fatalf("restore asked %q", w.s3.restored)
	}
	now, old, err := w.f.Sizes(ctx, p)
	if err != nil || now != 10 || old != 4 {
		t.Fatalf("sizes %d %d %v", now, old, err)
	}
}

func TestContainer(t *testing.T) {
	w := setup(t, "postgres")
	c, err := w.f.Container(ctx, w.db)
	must(t, err)
	net := operated.Network(w.instance.ID)
	want := []docker.NetAttach{
		{
			Name:    net,
			Aliases: []string{"db-" + w.instance.ID[:8]},
		},
	}
	if c.Image != "postgres:17" || len(c.Binds) != 1 || !strings.HasSuffix(c.Binds[0], ":/var/lib/postgresql/data") ||
		!slices.Contains(c.Env, "POSTGRES_USER=stackr") || !slices.EqualFunc(c.Networks, want, sameNet) {
		t.Errorf("container = %+v", c)
	}
	if !slices.ContainsFunc(w.fake.Calls(), func(c dockerfake.Call) bool {
		return c.Method == "EnsureNetwork" && c.Args[0] == net
	}) {
		t.Errorf("no EnsureNetwork(%s)", net)
	}
	must(t, w.f.Ready(ctx, w.db))
	w.fake.Err = map[string]error{"Exec": errors.New("no response")}
	if err := w.f.Ready(ctx, w.db); err == nil {
		t.Error("ready passed while pg_isready fails")
	}
}

// A postgres container keeps the tile's restart policy until it has a
// standby, then never restarts itself: the panel starts it. A fenced primary
// is refused a container at all, and an instance with a standby cannot be
// torn down before the standby goes.
func TestContainerRestartFenceAndStandbys(t *testing.T) {
	w := setup(t, "postgres")
	c, err := w.f.Container(ctx, w.db)
	must(t, err)
	if c.Restart != "" {
		t.Errorf("a postgres without a standby has restart %q, want the tile's", c.Restart)
	}
	if s := setup(t, "s3"); func() string { c, _ := s.f.Container(ctx, s.db); return c.Restart }() != "" {
		t.Error("s3 keeps the tile's restart policy")
	}

	p, err := w.f.Members.CreatePrimary(ctx, w.instance.ID)
	must(t, err)
	if _, err := w.f.Members.SetFenced(ctx, p, true); err != nil {
		t.Fatal(err)
	}
	if _, err := w.f.Container(ctx, w.db); !isConflict(err) {
		t.Errorf("fenced primary: %v, want a conflict", err)
	}
	if _, err := w.f.Members.SetFenced(ctx, p, false); err != nil {
		t.Fatal(err)
	}
	must(t, func() error { _, err := w.f.Container(ctx, w.db); return err }())

	if _, err := w.f.Members.AddReplica(ctx, w.instance.ID, "a", "b"); err != nil {
		t.Fatal(err)
	}
	if c, err := w.f.Container(ctx, w.db); err != nil || c.Restart != "no" {
		t.Errorf("with a standby: restart %q, %v, want no", c.Restart, err)
	}
	if err := w.f.Teardown(ctx, w.db, true, io.Discard); !isConflict(err) {
		t.Errorf("teardown with a standby: %v, want a conflict", err)
	}
}

func sameNet(a, b docker.NetAttach) bool {
	return a.Name == b.Name && slices.Equal(a.Aliases, b.Aliases)
}

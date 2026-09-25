package managed_test

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	mflow "github.com/FyrmForge/stackr/internal/service/internal/flow/managed"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/managed"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

var ctx = context.Background()

type vipStub struct{}

func (vipStub) Set(context.Context, string, []string) error {
	return nil
}

func (vipStub) Remove(context.Context, string) error {
	return nil
}

type s3Fake struct{ made, dropped []string }

func (s *s3Fake) Ping(context.Context) error { return nil }

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
	instance store.ManagedInstance
}

// setup: an env with a running managed tile "db" of engine, a slice tile
// "orders" and consumers "api" and "reporter".
func setup(t *testing.T, engine string) *world {
	st := servicetest.Store(t)
	fake := dockerfake.New()
	s3 := &s3Fake{}
	tiles := tile.New(st.Tiles, fake, vipStub{})
	f := &mflow.Flow{
		Tiles:     tiles,
		Instances: managed.New(st.ManagedInstances, st.Provisions, st.Bindings),
		Volumes:   volume.New(st.Volumes, fake),
		Envs:      environment.New(st.Environments, fake),
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
		db:     mk("db", tile.Managed),
		orders: mk("orders", tile.Slice),
		api:    mk("api", tile.Image),
		rep:    mk("reporter", tile.Image),
	}
	m, err := f.Instances.Create(ctx, w.db.ID, engine, "stackr", "")
	must(t, err)
	w.instance = m
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
		if c.Method == "Exec" {
			b.WriteString(strings.Join(c.Args, " ") + "\n")
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

func TestPostgresSliceAndBindings(t *testing.T) {
	w := setup(t, "postgres")
	w.fake.ExecOut = "0,0"
	p, err := w.f.Provision(ctx, w.db, w.orders)
	must(t, err)
	contains(t, "provision", execs(w.fake, 0),
		`CREATE ROLE "orders"`,
		`CREATE DATABASE "orders" OWNER "orders"`,
		"SET log_min_error_statement = PANIC",
		"psql -q ",
	)
	if p.DBName != "orders" || p.DBUser != "orders" || p.OnRemove != managed.Keep {
		t.Errorf("provision = %+v", p)
	}

	// A second Provision only verifies: ALTER the owner, no CREATE.
	w.fake.ExecOut = "1,1"
	n := len(w.fake.Calls())
	again, err := w.f.Provision(ctx, w.db, w.orders)
	must(t, err)
	if ex := execs(w.fake, n); again.ID != p.ID || !strings.Contains(ex, `ALTER ROLE "orders"`) || strings.Contains(ex, "CREATE") {
		t.Errorf("re-provision %s:\n%s", again.ID, ex)
	}

	// api writes, reporter reads; each its own user, in the slice's db.
	w.fake.ExecOut = "0"
	n = len(w.fake.Calls())
	api, err := w.f.Bind(ctx, p, w.api, "write")
	must(t, err)
	contains(t, "bind api", execs(w.fake, n),
		"-d orders",
		`CREATE ROLE "orders_api" LOGIN PASSWORD`,
		`GRANT ALL ON DATABASE "orders" TO "orders_api"`,
	)
	out, err := managed.Outputs(api)
	must(t, err)
	host := "db-" + w.instance.ID[:8]
	if api.DBUser != "orders_api" || out["PGHOST"] != host || out["PGUSER"] != "orders_api" || out["PGDATABASE"] != "orders" ||
		out["DATABASE_URL"] != "postgres://orders_api:"+api.DBPassword+"@"+host+":5432/orders" {
		t.Errorf("api binding %+v, outputs %v", api, out)
	}
	n = len(w.fake.Calls())
	rep, err := w.f.Bind(ctx, p, w.rep, "read")
	must(t, err)
	contains(t, "bind reporter", execs(w.fake, n),
		`CREATE ROLE "orders_reporter"`,
		`GRANT SELECT ON ALL TABLES IN SCHEMA public TO "orders_reporter"`,
		`ALTER DEFAULT PRIVILEGES FOR ROLE "orders_api" IN SCHEMA public GRANT SELECT ON TABLES TO "orders_reporter"`,
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
		`REVOKE ALL ON DATABASE "orders" FROM "orders_reporter"`,
		`GRANT ALL ON DATABASE "orders" TO "orders_reporter"`,
	)
	if strings.Contains(ex, "ROLE \"orders_reporter\" LOGIN") || moved.DBUser != rep.DBUser ||
		moved.DBPassword != rep.DBPassword || moved.Access != "write" {
		t.Errorf("re-grant %+v:\n%s", moved, ex)
	}

	// Unbind: the user's tables pass to the owner, then the user goes.
	n = len(w.fake.Calls())
	must(t, w.f.Unbind(ctx, moved))
	contains(t, "unbind", execs(w.fake, n),
		`REASSIGN OWNED BY "orders_reporter" TO "orders"`,
		`DROP OWNED BY "orders_reporter"`,
		`DROP ROLE IF EXISTS "orders_reporter"`,
	)
	if bs, err := w.f.Instances.Bindings(ctx, p.ID); err != nil || len(bs) != 1 {
		t.Errorf("bindings after unbind = %v, %v", bs, err)
	}

	// on_remove drop: every binding, then the database, then the row.
	p, err = w.f.Instances.SetOnRemove(ctx, p, managed.Drop)
	must(t, err)
	n = len(w.fake.Calls())
	must(t, w.f.Drop(ctx, p, false, io.Discard))
	contains(t, "drop", execs(w.fake, n),
		`DROP ROLE IF EXISTS "orders_api"`,
		`DROP DATABASE IF EXISTS "orders" WITH (FORCE)`,
	)
	if _, ok, err := w.f.Instances.ProvisionOf(ctx, w.orders.ID); err != nil || ok {
		t.Errorf("provision after drop: %v %v", ok, err)
	}
}

// A new name never answers to one the instance already holds, users included.
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
	q, err := w.f.Provision(ctx, w.db, clash)
	must(t, err)
	if q.DBName != "orders_api_2" {
		t.Errorf("clashing slice = %q, want orders_api_2", q.DBName)
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
	if ex := execs(w.fake, n); strings.Contains(ex, "DROP DATABASE") || !strings.Contains(ex, `DROP ROLE IF EXISTS "orders_api"`) {
		t.Errorf("keep:\n%s", ex)
	}
	if _, ok, _ := w.f.Instances.ProvisionOf(ctx, w.orders.ID); ok {
		t.Error("a kept slice kept its row")
	}

	_, err = w.f.Provision(ctx, w.db, w.orders)
	must(t, err)
	if err := w.f.Teardown(ctx, w.db, false, io.Discard); !isConflict(err) {
		t.Errorf("teardown with a held slice = %v, want a conflict", err)
	}
	n = len(w.fake.Calls())
	must(t, w.f.Teardown(ctx, w.db, true, io.Discard))
	contains(t, "forced teardown", execs(w.fake, n), `DROP DATABASE IF EXISTS "orders"`)
	if _, err := w.f.Instances.GetByTile(ctx, w.db.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("instance after teardown: %v", err)
	}
}

func isConflict(err error) bool {
	_, ok := errs.IsConflict(err)
	return ok
}

// s3: a bucket per slice; every consumer holds the root creds (ponytail in
// s3.go), so Bind mints nothing and the access is recorded only.
func TestS3BucketPerSlice(t *testing.T) {
	w := setup(t, "s3")
	p, err := w.f.Provision(ctx, w.db, w.orders)
	must(t, err)
	if !slices.Equal(w.s3.made, []string{"orders"}) || p.DBUser != "stackr" {
		t.Errorf("made %v, user %q", w.s3.made, p.DBUser)
	}
	b, err := w.f.Bind(ctx, p, w.rep, "read")
	must(t, err)
	out, err := managed.Outputs(b)
	must(t, err)
	if b.DBUser != "stackr" || b.Access != "read" || out["S3_BUCKET"] != "orders" ||
		out["S3_ENDPOINT"] != "http://db-"+w.instance.ID[:8]+":9000" {
		t.Errorf("binding %+v, outputs %v", b, out)
	}
	p, err = w.f.Instances.SetOnRemove(ctx, p, managed.Drop)
	must(t, err)
	must(t, w.f.Drop(ctx, p, false, io.Discard))
	if !slices.Equal(w.s3.dropped, []string{"orders"}) {
		t.Errorf("dropped %v", w.s3.dropped)
	}
}

func TestContainer(t *testing.T) {
	w := setup(t, "postgres")
	c, err := w.f.Container(ctx, w.db)
	must(t, err)
	net := managed.Network(w.instance.ID)
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

func sameNet(a, b docker.NetAttach) bool {
	return a.Name == b.Name && slices.Equal(a.Aliases, b.Aliases)
}

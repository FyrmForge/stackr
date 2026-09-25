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
	db, api  store.Tile
	instance store.ManagedInstance
}

// setup: an env with a running managed tile "db" of engine and a consumer "api".
func setup(t *testing.T, engine string) *world {
	st := servicetest.Store(t)
	fake, s3 := dockerfake.New(), &s3Fake{}
	tiles := tile.New(st.Tiles, fake, vipStub{})
	f := &mflow.Flow{
		Tiles:     tiles,
		Instances: managed.New(st.ManagedInstances, st.Provisions),
		Volumes:   volume.New(st.Volumes, fake),
		Envs:      environment.New(st.Environments, fake),
		S3:        func(string, string, string) mflow.S3Admin { return s3 },
		ReadyWait: 20 * time.Millisecond,
		ReadyPoll: time.Millisecond,
	}
	h := struct{ OrgID, StackID, EnvID string }{uuid.NewString(), uuid.NewString(), uuid.NewString()}
	now := time.Now()
	must(t, st.Orgs.Create(ctx, store.Org{
		ID:        h.OrgID,
		Name:      "o",
		Slug:      "o",
		EnvColors: "{}",
		Settings:  "{}",
		CreatedAt: now,
	}))
	must(t, st.Stacks.Create(ctx, store.Stack{
		ID:        h.StackID,
		OrgID:     h.OrgID,
		Name:      "s",
		Slug:      "s",
		Settings:  "{}",
		CreatedAt: now,
	}))
	must(t, st.Environments.Create(ctx, store.Environment{
		ID:         h.EnvID,
		StackID:    h.StackID,
		Name:       "dev",
		Slug:       "dev",
		Type:       "static",
		Settings:   "{}",
		Network:    "n",
		FromKind:   "branch",
		FromBranch: "main",
		CreatedAt:  now,
	}))
	db, err := tiles.Create(ctx, store.Tile{
		StackID:       h.StackID,
		EnvironmentID: h.EnvID,
		Name:          "db",
		Kind:          tile.Managed,
	})
	must(t, err)
	api, err := tiles.Create(ctx, store.Tile{
		StackID:       h.StackID,
		EnvironmentID: h.EnvID,
		Name:          "api",
		Kind:          tile.Image,
		ImageRef:      "a:1",
	})
	must(t, err)
	m, err := f.Instances.Create(ctx, db.ID, engine, "stackr", "")
	must(t, err)
	fake.Containers = []docker.Container{{
		ID:     "c1",
		State:  "running",
		Labels: map[string]string{tile.LabelTile: db.ID, tile.LabelRole: "replica"},
	}}
	return &world{
		f:        f,
		fake:     fake,
		s3:       s3,
		db:       db,
		api:      api,
		instance: m,
	}
}

func execs(f *dockerfake.Fake) string {
	var b strings.Builder
	for _, c := range f.Calls() {
		if c.Method == "Exec" {
			b.WriteString(strings.Join(c.Args, " ") + "\n")
		}
	}
	return b.String()
}

func TestPostgresProvisionAndDrop(t *testing.T) {
	w := setup(t, "postgres")
	w.fake.ExecOut = "0,0"
	p, err := w.f.Attach(ctx, w.api, w.db, "", false, managed.Drop)
	must(t, err)
	ex := execs(w.fake)
	if !strings.Contains(ex, `CREATE ROLE "api"`) || !strings.Contains(ex, `CREATE DATABASE "api" OWNER "api"`) ||
		!strings.Contains(ex, "SET log_min_error_statement = PANIC") {
		t.Errorf("provision execs:\n%s", ex)
	}
	// step 7b task 5 replaces this: DATABASE_URL is asserted on the binding's
	// outputs once Bind stores them.

	// A second consumer asking for the same name gets its own, suffixed slice.
	web, err := w.f.Tiles.Create(ctx, store.Tile{
		StackID:       w.api.StackID,
		EnvironmentID: w.api.EnvironmentID,
		Name:          "web",
		Kind:          tile.Image,
		ImageRef:      "w:1",
	})
	must(t, err)
	w.fake.ExecOut = "0,0"
	q, err := w.f.Attach(ctx, web, w.db, "api", false, "")
	must(t, err)
	if q.DBName != "api_2" {
		t.Errorf("second slice = %q, want api_2", q.DBName)
	}

	// Re-provision (consumer deploy) is check-then-create: ALTER, no CREATE.
	w.fake.ExecOut = "1,1"
	before := len(w.fake.Calls())
	must(t, w.f.Reconcile(ctx, w.api, io.Discard))
	var again strings.Builder
	for _, c := range w.fake.Calls()[before:] {
		again.WriteString(strings.Join(c.Args, " "))
	}
	if s := again.String(); !strings.Contains(s, `ALTER ROLE "api"`) || strings.Contains(s, "CREATE") {
		t.Errorf("re-provision: %s", s)
	}
	// The fake answers the bare row; real psql only does that quiet (-q).
	if s := again.String(); !strings.Contains(s, "psql -q ") {
		t.Errorf("re-provision psql is not quiet: %s", s)
	}

	// on_remove drop: the engine drops it and the row goes.
	must(t, w.f.Detach(ctx, p, false))
	if ex := execs(w.fake); !strings.Contains(ex, `DROP DATABASE IF EXISTS "api" WITH (FORCE)`) {
		t.Errorf("drop execs:\n%s", ex)
	}
	if _, err := w.f.Instances.GetProvision(ctx, p.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("row after drop: %v", err)
	}
}

func TestKeepOrphansAndTeardownNeedsForce(t *testing.T) {
	w := setup(t, "postgres")
	w.fake.ExecOut = "0,0"
	p, err := w.f.Attach(ctx, w.api, w.db, "orders", false, "")
	must(t, err)
	if p.DBName != "orders" {
		t.Errorf("slice name = %q", p.DBName)
	}
	must(t, w.f.Detach(ctx, p, false))
	if strings.Contains(execs(w.fake), "DROP") {
		t.Error("a keep slice was dropped")
	}
	if err := w.f.Teardown(ctx, w.db, false, io.Discard); !isConflict(err) {
		t.Errorf("teardown with a held slice = %v, want a conflict", err)
	}
	must(t, w.f.Teardown(ctx, w.db, true, io.Discard))
	if !strings.Contains(execs(w.fake), `DROP DATABASE IF EXISTS "orders"`) {
		t.Error("forced teardown did not drop the slice")
	}
}

func isConflict(err error) bool {
	_, ok := errs.IsConflict(err)
	return ok
}

func TestS3BucketPerConsumer(t *testing.T) {
	w := setup(t, "s3")
	w.api.Slug = "my_api"
	p, err := w.f.Attach(ctx, w.api, w.db, "", false, managed.Drop)
	must(t, err)
	if !slices.Equal(w.s3.made, []string{"my-api"}) || p.DBUser != "stackr" {
		t.Errorf("made %v, user %q", w.s3.made, p.DBUser)
	}
	// step 7b task 5 replaces this: S3_ENDPOINT and S3_BUCKET are asserted on
	// the binding's outputs once Bind stores them.
	must(t, w.f.Detach(ctx, p, false))
	if !slices.Equal(w.s3.dropped, []string{"my-api"}) {
		t.Errorf("dropped %v", w.s3.dropped)
	}
	elsewhere := w.api
	elsewhere.EnvironmentID = "elsewhere"
	if _, err := w.f.Attach(ctx, elsewhere, w.db, "", false, ""); err == nil {
		t.Error("an env-scoped instance served another env")
	}
}

func TestContainer(t *testing.T) {
	w := setup(t, "postgres")
	c, err := w.f.Container(ctx, w.db)
	must(t, err)
	if c.Image != "postgres:17" || len(c.Binds) != 1 || !strings.HasSuffix(c.Binds[0], ":/var/lib/postgresql/data") ||
		!slices.Contains(c.Env, "POSTGRES_USER=stackr") || len(c.Networks) != 0 {
		t.Errorf("container = %+v", c)
	}
	must(t, w.f.Ready(ctx, w.db))
	w.fake.Err = map[string]error{"Exec": errors.New("no response")}
	if err := w.f.Ready(ctx, w.db); err == nil {
		t.Error("ready passed while pg_isready fails")
	}
}

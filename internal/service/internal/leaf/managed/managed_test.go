package managed_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/managed"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

var ctx = context.Background()

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// setup seeds one env with tiles db, api and web.
func setup(t *testing.T) (*managed.Leaf, *store.Store, managed.Home, map[string]string) {
	st := servicetest.Store(t)
	h := managed.Home{OrgID: uuid.NewString(), StackID: uuid.NewString(), EnvID: uuid.NewString()}
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
		Domains:   "[]",
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
	tiles := map[string]string{}
	for _, s := range []string{"db", "api", "web"} {
		tiles[s] = uuid.NewString()
		must(t, st.Tiles.Create(ctx, store.Tile{
			ID:            tiles[s],
			StackID:       h.StackID,
			EnvironmentID: h.EnvID,
			Name:          s,
			Slug:          s,
			Kind:          "image",
			UpdatePolicy:  "manual",
			Replicas:      1,
			CreatedAt:     now,
			UpdatedAt:     now,
		}))
	}
	return managed.New(st.ManagedInstances, st.Provisions), st, h, tiles
}

func TestScope(t *testing.T) {
	l, _, h, tiles := setup(t)
	if _, err := l.Create(ctx, tiles["db"], "postgres", "global", h, "root", "db:5432"); err == nil {
		t.Error("unknown scope coerced")
	}
	m, err := l.Create(ctx, tiles["db"], "postgres", "", h, "root", "db:5432")
	if err != nil || m.ScopeKind != managed.Env || m.ScopeID != h.EnvID || len(m.AdminPassword) != 48 {
		t.Fatalf("create = %+v %v", m, err)
	}
	other := managed.Home{EnvID: uuid.NewString(), StackID: h.StackID, OrgID: h.OrgID}
	if vs, _ := l.Visible(ctx, other); len(vs) != 0 {
		t.Errorf("env-scoped instance visible from another env: %v", vs)
	}
	if m, err = l.SetScope(ctx, m, managed.Org, h); err != nil || m.ScopeID != h.OrgID {
		t.Fatalf("org scope = %+v %v", m, err)
	}
	if vs, _ := l.Visible(ctx, other); len(vs) != 1 {
		t.Errorf("org-scoped instance not visible across envs: %v", vs)
	}
	if got, _ := l.Get(ctx, m.ID); got.AdminPassword != m.AdminPassword {
		t.Error("admin password did not round-trip")
	}
}

func TestSlices(t *testing.T) {
	l, _, h, tiles := setup(t)
	m, _ := l.Create(ctx, tiles["db"], "postgres", "", h, "root", "db:5432")
	slice := managed.Slice{
		Slug:       "api",
		DBName:     "api",
		DBUser:     "api",
		DBPassword: managed.Password(),
	}

	p, err := l.Provision(ctx, m, tiles["api"], slice)
	if err != nil || p.OnRemove != managed.Keep {
		t.Fatalf("provision = %+v %v", p, err)
	}
	if _, err := l.Provision(ctx, m, tiles["api"], slice); err == nil {
		t.Error("second slice for one consumer")
	}
	if _, err := l.Provision(
		ctx,
		m,
		tiles["web"],
		managed.Slice{DBName: "w", DBUser: "w", OnRemove: "detach"},
	); err == nil {
		t.Error("bad on_remove accepted")
	}
	if _, err := l.Share(ctx, p, tiles["web"], false); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("cross-env share = %v", err)
	}
	shared, err := l.Share(ctx, p, tiles["web"], true)
	must(t, err)
	if names, _ := l.SliceNames(ctx, m.ID); len(names) != 2 {
		t.Errorf("names = %v", names)
	}
	if _, err := l.SetPublic(ctx, p, true, ""); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("public without a domain = %v", err)
	}
	p, _ = l.SetOutputs(ctx, p, map[string]string{"DATABASE_URL": "postgres://x"})
	if managed.Outputs(p)["DATABASE_URL"] != "postgres://x" {
		t.Error("outputs")
	}

	// keep: orphaned, binding gone; ephemeral env: dropped whatever it says.
	if drop, err := l.Release(ctx, p, false); err != nil || drop {
		t.Fatalf("release keep = %v %v", drop, err)
	}
	got, _ := l.GetProvision(ctx, p.ID)
	if got.ConsumerTileID != nil || len(managed.Outputs(got)) != 0 {
		t.Errorf("orphan = %+v", got)
	}
	if drop, _ := l.Release(ctx, shared, true); !drop {
		t.Error("ephemeral env kept its slice")
	}

	// Teardown refuses while slices are held; forced, each slice once.
	if _, err := l.Teardown(ctx, m, false); err == nil {
		t.Error("teardown with held slices")
	}
	drop, err := l.Teardown(ctx, m, true)
	if err != nil || len(drop) != 1 {
		t.Fatalf("forced teardown = %v %v", drop, err)
	}
	must(t, l.Delete(ctx, m.ID))
	if ps, _ := l.ByInstance(ctx, m.ID); len(ps) != 0 {
		t.Errorf("provisions outlived the instance: %v", ps)
	}
}

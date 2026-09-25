package managed_test

import (
	"context"
	"errors"
	"slices"
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

type home struct {
	OrgID, StackID, EnvID string
}

// setup seeds one env with tiles db (the instance), orders (a slice), api
// and web (consumers).
func setup(t *testing.T) (*managed.Leaf, *store.Store, home, map[string]string) {
	st := servicetest.Store(t)
	h := home{OrgID: uuid.NewString(), StackID: uuid.NewString(), EnvID: uuid.NewString()}
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
	tiles := map[string]string{}
	for _, s := range []string{"db", "orders", "api", "web"} {
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
	return managed.New(st.ManagedInstances, st.Provisions, st.Bindings), st, h, tiles
}

func TestCreate(t *testing.T) {
	l, _, _, tiles := setup(t)
	if _, err := l.Create(ctx, tiles["db"], "postgres", "", "db:5432"); err == nil {
		t.Error("instance without an admin user")
	}
	m, err := l.Create(ctx, tiles["db"], "postgres", "root", "db:5432")
	if err != nil || len(m.AdminPassword) != 48 {
		t.Fatalf("create = %+v %v", m, err)
	}
	got, err := l.Get(ctx, m.ID)
	must(t, err)
	if got.AdminPassword != m.AdminPassword {
		t.Error("admin password did not round-trip")
	}
	if len(got.Allow) != 0 || len(got.EnvPairs) != 0 {
		t.Errorf("a new instance allows nothing: %+v", got)
	}
}

func TestSlices(t *testing.T) {
	l, _, _, tiles := setup(t)
	m, _ := l.Create(ctx, tiles["db"], "postgres", "root", "db:5432")
	slice := managed.Slice{
		DBName:     "orders",
		DBUser:     "orders",
		DBPassword: managed.Password(),
	}

	p, err := l.CreateProvision(ctx, m, tiles["orders"], slice)
	if err != nil || p.OnRemove != managed.Keep {
		t.Fatalf("provision = %+v %v", p, err)
	}
	if _, err := l.CreateProvision(ctx, m, tiles["orders"], slice); err == nil {
		t.Error("second provision for one slice tile")
	}
	if _, err := l.CreateProvision(
		ctx,
		m,
		tiles["web"],
		managed.Slice{
			DBName:   "w",
			DBUser:   "w",
			OnRemove: "detach",
		},
	); err == nil {
		t.Error("bad on_remove accepted")
	}
	if got, ok, err := l.ProvisionOf(ctx, tiles["orders"]); err != nil || !ok || got.ID != p.ID {
		t.Errorf("provision of = %+v %v %v", got, ok, err)
	}
	if _, ok, err := l.ProvisionOf(ctx, tiles["api"]); err != nil || ok {
		t.Errorf("a consumer has no provision: %v %v", ok, err)
	}
	if _, err := l.SetPublic(ctx, p, true, ""); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("public without a domain = %v", err)
	}

	// A binding per consumer: its own user, access and outputs.
	cred := managed.Cred{
		Access:   "write",
		User:     "orders_api",
		Password: managed.Password(),
		Outputs: map[string]string{
			"PGUSER": "orders_api",
		},
	}
	b, err := l.Bind(ctx, p, tiles["api"], cred)
	must(t, err)
	if _, err := l.Bind(ctx, p, tiles["api"], cred); err == nil {
		t.Error("second binding for one consumer on one slice")
	}
	bad := cred
	bad.Access = "admin"
	if _, err := l.Bind(ctx, p, tiles["web"], bad); err == nil {
		t.Error("bad access accepted")
	}
	bound, err := l.Bound(ctx, tiles["api"])
	must(t, err)
	out, err := managed.Outputs(bound[tiles["orders"]])
	must(t, err)
	if bound[tiles["orders"]].DBPassword != cred.Password || out["PGUSER"] != "orders_api" {
		t.Errorf("bound = %+v, outputs %v", bound, out)
	}
	if ps, _ := l.ForConsumer(ctx, tiles["api"]); len(ps) != 1 || ps[0].ID != p.ID {
		t.Errorf("for consumer = %v", ps)
	}
	if names, _ := l.Names(ctx, m.ID); !slices.Equal(names, []string{"orders", "orders", "orders_api"}) {
		t.Errorf("names = %v", names)
	}
	b, err = l.SetAccess(ctx, b, "read")
	must(t, err)
	if bs, _ := l.Bindings(ctx, p.ID); len(bs) != 1 || bs[0].Access != "read" || bs[0].DBUser != "orders_api" {
		t.Errorf("after set access = %+v", bs)
	}
	must(t, l.Unbind(ctx, b.ID))
	if bs, _ := l.Bindings(ctx, p.ID); len(bs) != 0 {
		t.Errorf("after unbind = %+v", bs)
	}

	// Teardown refuses while slices are held; forced, it hands them back.
	if _, err := l.Teardown(ctx, m, false); err == nil {
		t.Error("teardown with held slices")
	}
	held, err := l.Teardown(ctx, m, true)
	if err != nil || len(held) != 1 {
		t.Fatalf("forced teardown = %v %v", held, err)
	}
	must(t, l.Delete(ctx, m.ID))
	if ps, _ := l.ByInstance(ctx, m.ID); len(ps) != 0 {
		t.Errorf("provisions outlived the instance: %v", ps)
	}
}

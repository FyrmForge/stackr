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

type home struct {
	OrgID, StackID, EnvID string
}

// setup seeds one env with tiles db, api and web.
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
	// step 7b task 5 replaces this: outputs live on the binding, and a
	// release unbinds rather than orphaning the provision.

	// keep: the row stays; ephemeral env: dropped whatever it says.
	if drop, err := l.Release(ctx, p, false); err != nil || drop {
		t.Fatalf("release keep = %v %v", drop, err)
	}
	if got, err := l.GetProvision(ctx, p.ID); err != nil || got.TileID != tiles["api"] {
		t.Errorf("kept = %+v %v", got, err)
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

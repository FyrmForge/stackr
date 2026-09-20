package project

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/service"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// Each logical slice is its own node, so a consumer of three databases on one
// instance gets three edges, one per slice, not one to the instance. Two
// variables reading the SAME slice still collapse: identical stacked paths
// render darker and inflate the hit area.
func TestTileRefsEdgePerSliceDeduplicated(t *testing.T) {
	ctx := context.Background()
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)
	now := time.Now().UTC()

	instance := &repo.Tile{ID: "pg1", StackID: seed.Stack.ID, EnvironmentID: seed.Env.ID,
		Name: "sharedpg", Slug: "sharedpg", Kind: "service", Engine: "postgres",
		Status: "running", CreatedAt: now, UpdatedAt: now}
	require.NoError(t, s.CreateTile(ctx, instance), "create instance")

	for _, name := range []string{"orders", "carts", "stock"} {
		res := &repo.ManagedResource{ID: "res-" + name, EnvironmentID: seed.Env.ID,
			ProviderTileID: instance.ID, Name: name, Slug: "sharedpg-" + name,
			Kind: "postgres", Status: "active", CreatedAt: now, UpdatedAt: now}
		require.NoError(t, s.CreateResource(ctx, res), "create resource %s", name)
		require.NoError(t, s.UpsertOutput(ctx, &repo.ResourceOutput{ResourceID: res.ID,
			Name: "DATABASE_URL", Value: "postgres://" + name}), "output %s", name)
		require.NoError(t, s.CreateBinding(ctx, &repo.ResourceBinding{ResourceID: res.ID,
			ConsumerTileID: seed.Tile.ID, CreatedAt: now}), "bind %s", name)
		v := &repo.Variable{OwnerKind: repo.OwnerTile, OwnerID: seed.Tile.ID,
			Name: name + "_URL", Value: "${{ tile.sharedpg-" + name + ".DATABASE_URL }}",
			CreatedAt: now, UpdatedAt: now}
		require.NoError(t, s.UpsertVariable(ctx, v), "var %s", name)
	}
	// A second variable onto a slice already referenced, the duplicate case.
	require.NoError(t, s.UpsertVariable(ctx, &repo.Variable{OwnerKind: repo.OwnerTile, OwnerID: seed.Tile.ID,
		Name: "ORDERS_URL_ALIAS", Value: "${{ tile.sharedpg-orders.DATABASE_URL }}",
		CreatedAt: now, UpdatedAt: now}), "dup var")

	h := &handler{store: s, envs: service.NewEnvironmentService(s, nil, nil, nil, service.NewGateService(s)), vars: service.NewVariableService(s, nil, nil, nil), stacks: service.NewStackService(s, nil, nil, service.NewEnvironmentService(s, nil, nil, nil, service.NewGateService(s)), nil, service.NewGateService(s), nil), tiles: service.NewTileService(s, nil, nil, nil, nil, nil, service.NewGateService(s))}
	refs := h.tileRefs(ctx, []repo.Tile{*seed.Tile, *instance})
	got := append([]string(nil), refs[seed.Tile.ID]...)
	sort.Strings(got)
	want := []string{"res-carts", "res-orders", "res-stock"}
	require.Equal(t, want, got, "tileRefs: want one edge per slice")
}

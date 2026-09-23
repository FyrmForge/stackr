package managedtiles_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/managedtiles"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// Registering an engine must be enough to make it a first-class managed tile:
// provisionable, publishing outputs, named with its own nouns, offered in the
// picker, with no branch anywhere outside its registry entry. This test is
// the contract; if it needs a change outside this file to pass for a new
// engine, the registry has sprung a leak again.
func TestRegisteredEngineNeedsNoBranches(t *testing.T) {
	ctx := context.Background()
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)

	const name = "fakedb"
	managedtiles.Engines[name] = managedtiles.Engine{
		Label: "FakeDB", Order: 99, DefaultImage: "fake:1", Port: 1234,
		DataPath: "/data", PrimaryOutput: "DATABASE_URL", SliceNoun: "shard",
		UnitNoun: "shard", SecretSuffix: "_url",
		Env: func(d *repo.Tile) []string { return nil },
		Conn: func(d *repo.Tile) []managedtiles.ConnVar {
			return []managedtiles.ConnVar{{Name: "DATABASE_URL", Value: "fake://" + d.Slug, Secret: true}}
		},
		Provision: func(svc *managedtiles.Service, ctx context.Context, instance, consumer *repo.Tile, slice string, _ bool) (*repo.Provision, error) {
			if slice == "" {
				slice = consumer.Slug
			}
			p := &repo.Provision{ID: "fake-p1", InstanceTileID: instance.ID, ConsumerTileID: consumer.ID,
				EnvID: consumer.EnvironmentID, DBName: slice, DBUser: slice, DBPassword: "pw",
				Status: "active", CreatedAt: time.Now().UTC()}
			if err := s.CreateProvision(ctx, p); err != nil {
				return nil, err
			}
			return p, svc.SyncResource(ctx, instance, p)
		},
		Outputs: func(_ *managedtiles.Service, _ context.Context, instance *repo.Tile, p *repo.Provision) []repo.ResourceOutput {
			return []repo.ResourceOutput{{Name: "DATABASE_URL", Value: "fake://" + p.DBName, Secret: true}}
		},
	}
	defer delete(managedtiles.Engines, name)

	require.True(t, managedtiles.CanProvision(name), "an engine with a Provision hook must be provisionable")
	assert.Equal(t, "DATABASE_URL", managedtiles.DefaultOutput(name))
	var listed bool
	for _, o := range managedtiles.EngineOptions() {
		listed = listed || (o.Name == name && o.Label == "FakeDB")
	}
	assert.True(t, listed, "a registered engine must appear in the picker")

	inst := instance(t, s, seed, name)
	require.NoError(t, managedtiles.NewDB(inst), "NewDB")
	assert.Equal(t, "fake:1", inst.ImageRef, "image")
	managedtiles.PublishConnection(ctx, s, svcRows(s), inst)
	vars, err := s.ListVariables(ctx, repo.OwnerTile, inst.ID)
	require.NoError(t, err)
	require.Len(t, vars, 1, "published vars = %+v", vars)
	require.Equal(t, "DATABASE_URL", vars[0].Name, "published vars = %+v", vars)

	p, err := svc(s).Provision(ctx, inst, seed.Tile, "", false)
	require.NoError(t, err, "provision")
	outs := outputs(t, s, seed.Env.ID, managedtiles.ResourceSlug(inst, p))
	assert.NotEmpty(t, outs["DATABASE_URL"].Value, "the slice published no outputs")

	// Asking for a public slice on an engine that has no such concept is
	// refused by the registry, not by a caller that happens to know.
	_, err = svc(s).Provision(ctx, inst, seed.Tile, "other", true)
	assert.Error(t, err, "public slice must be refused for an engine without PublicSlices")
}

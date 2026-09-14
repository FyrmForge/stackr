package envcompare

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/config/stackconf"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

func tile(slug, image, env string) stackconf.TileState {
	return stackconf.TileState{Tile: repo.Tile{Slug: slug, Kind: "service", SourceType: "image", ImageRef: image, Env: env}}
}

func TestCompare(t *testing.T) {
	envs := []repo.Environment{{ID: "dev", Slug: "dev"}, {ID: "prod", Slug: "production"}}
	state := stackconf.State{Envs: map[string]stackconf.EnvState{
		"dev": {Tiles: map[string]stackconf.TileState{
			"api":    tile("api", "api:1", "PORT=8080\nTOKEN=${{ stack.secrets.TOKEN }}"),
			"worker": tile("worker", "worker:1", ""),
			"same":   tile("same", "x:1", "A=1"),
		}},
		"production": {Tiles: map[string]stackconf.TileState{
			"api":  tile("api", "api:2", "PORT=80\nTOKEN=${{ stack.secrets.PROD_TOKEN }}"),
			"same": tile("same", "x:1", "A=1"),
		}},
	}}
	intended := map[string][]repo.Intended{
		"prod": {{EnvironmentID: "prod", TileSlug: "api", Key: "env.PORT", Value: "80"}},
	}

	res := Compare("s", envs, state, intended)
	require.Len(t, res.Rows, 3)
	byslug := map[string]Row{}
	for _, r := range res.Rows {
		byslug[r.Slug] = r
	}
	api := byslug["api"].Cells[1]
	assert.Equal(t, "differs", api.State)
	assert.Equal(t, []Key{{Name: "env.PORT", Ref: "8080", Val: "80", Intended: true}, {Name: "image", Ref: "api:1", Val: "api:2"}}, api.Keys,
		"secrets compare by presence, so TOKEN is not a difference; PORT is intended at its marked value")
	assert.Equal(t, "same", byslug["same"].Cells[1].State)
	assert.Equal(t, "missing", byslug["worker"].Cells[1].State)
	assert.Equal(t, 1, res.Differs)
	assert.Equal(t, 1, res.Missing)
	assert.Equal(t, map[string]bool{"env": true, "image": true}, api.Fields())

	// the marked value moved on: the key shows up again
	state.Envs["production"].Tiles["api"] = tile("api", "api:2", "PORT=81\nTOKEN=${{ stack.secrets.PROD_TOKEN }}")
	res = Compare("s", envs, state, intended)
	for _, r := range res.Rows {
		if r.Slug == "api" {
			assert.False(t, r.Cells[1].Keys[0].Intended)
		}
	}

	// declared per env by the file ("" value) on the reference: intended everywhere
	res = Compare("s", envs, state, map[string][]repo.Intended{"dev": {{TileSlug: "api", Key: "image"}}, "prod": {{TileSlug: "api", Key: "env.PORT"}}})
	for _, r := range res.Rows {
		if r.Slug == "api" {
			assert.Equal(t, "intended", r.Cells[1].State)
		}
	}
}

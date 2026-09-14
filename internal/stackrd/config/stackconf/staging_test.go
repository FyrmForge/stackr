package stackconf

import (
	"testing"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/stretchr/testify/require"
)

// TestApplyStagedPatch covers the three staged ops against a live-derived env.
func TestApplyStagedPatch(t *testing.T) {
	base := func() ResolvedEnv {
		return ResolvedEnv{Tiles: map[string]TileConf{
			"web": {Type: "service", Port: 80, Env: EnvMap{"A": "1", "B": "2"}},
		}}
	}

	// update: a sparse patch replaces only the named fields; env is a full
	// replace, so a removed var actually disappears (not merged).
	re := base()
	applyStagedPatch(re, repo.StagedChange{
		TileSlug: "web",
		Payload:  `{"op":"update","patch":{"published_ports":"8080:80","env":{"A":"9"}}}`,
	})
	web := re.Tiles["web"]
	require.Equal(t, "8080:80", web.PublishedPorts, "published_ports not patched: %q", web.PublishedPorts)
	require.Equal(t, 80, web.Port, "untouched field clobbered: port=%d", web.Port)
	require.Len(t, web.Env, 1, "env should be a full replace {A:9}, got %v", web.Env)
	require.Equal(t, "9", web.Env["A"], "env should be a full replace {A:9}, got %v", web.Env)

	// create: a full TileConf lands as a new tile.
	re = base()
	applyStagedPatch(re, repo.StagedChange{
		TileSlug: "cache",
		Payload:  `{"op":"create","patch":{"type":"service","image":"redis:7","port":6379}}`,
	})
	_, ok := re.Tiles["cache"]
	require.True(t, ok, "create did not add the tile")

	// delete: the tile is dropped so Diff emits a strict-mode delete.
	re = base()
	applyStagedPatch(re, repo.StagedChange{TileSlug: "web", Payload: `{"op":"delete"}`})
	_, ok = re.Tiles["web"]
	require.False(t, ok, "delete did not drop the tile")

	// delete wins over update regardless of arrival order.
	re = base()
	applyStagedPatch(re, repo.StagedChange{TileSlug: "web", Payload: `{"op":"delete"}`})
	applyStagedPatch(re, repo.StagedChange{TileSlug: "web", Payload: `{"op":"update","patch":{"port":90}}`})
	_, ok = re.Tiles["web"]
	require.False(t, ok, "update resurrected a deleted tile")
}

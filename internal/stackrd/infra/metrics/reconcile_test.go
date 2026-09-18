package metrics

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

func TestReconcileStatus(t *testing.T) {
	cases := []struct {
		name   string
		tile   repo.Tile
		state  string
		health string
		has    bool
		want   string
		wantOK bool
	}{
		{"running tile, container gone", repo.Tile{Status: "running"}, "", "", false, "stopped", true},
		{"running tile, container exited", repo.Tile{Status: "running"}, "exited", "", true, "stopped", true},
		{"running tile, container running", repo.Tile{Status: "running"}, "running", "", true, "", false},
		{"stopped tile, container running", repo.Tile{Status: "stopped"}, "running", "", true, "running", true},
		{"stopped tile, container gone", repo.Tile{Status: "stopped"}, "", "", false, "", false},
		{"building is not arbitrated", repo.Tile{Status: "building"}, "", "", false, "", false},
		{"paused is not arbitrated", repo.Tile{Status: "paused"}, "running", "", true, "", false},
		{"done is not arbitrated", repo.Tile{Status: "done"}, "", "", false, "", false},
		{"idle is not arbitrated", repo.Tile{Status: "idle"}, "", "", false, "", false},
		{"volume tile", repo.Tile{Status: "running", Kind: "volume"}, "", "", false, "", false},
		{"cron tile", repo.Tile{Status: "running", Kind: "cron"}, "", "", false, "", false},
		{"function tile", repo.Tile{Status: "running", Kind: "function"}, "", "", false, "", false},

		// docker-native health verdicts
		{"running turns unhealthy", repo.Tile{Status: "running"}, "running", "unhealthy", true, "unhealthy", true},
		{"running stays healthy", repo.Tile{Status: "running"}, "running", "healthy", true, "", false},
		{"unhealthy recovers", repo.Tile{Status: "unhealthy"}, "running", "healthy", true, "running", true},
		{"unhealthy no-checks recovers", repo.Tile{Status: "unhealthy"}, "running", "", true, "running", true},
		{"unhealthy stays", repo.Tile{Status: "unhealthy"}, "running", "unhealthy", true, "", false},
		{"unhealthy container gone", repo.Tile{Status: "unhealthy"}, "", "", false, "stopped", true},

		// A failed deploy writes "error" and nothing else clears it: a bad
		// image tag leaves swarm serving the old task while the canvas reads
		// Crashed for ever.
		{"error tile, container running", repo.Tile{Status: "error"}, "running", "", true, "running", true},
		{"error tile, container unhealthy", repo.Tile{Status: "error"}, "running", "unhealthy", true, "unhealthy", true},
		{"error tile, container gone", repo.Tile{Status: "error"}, "", "", false, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := reconcileStatus(&c.tile, c.state, c.health, c.has)
			require.Equal(t, c.want, got)
			require.Equal(t, c.wantOK, ok)
		})
	}
}

// A task rescheduled from the manager to a worker leaves a `created`
// container behind on the manager. That corpse used to win over the swarm task
// list, so a tile serving traffic on the worker read "stopped" forever.
func TestMergeRemoteBeatsLocalCorpse(t *testing.T) {
	state := map[string]string{"dead": "created", "exited": "exited", "live": "running"}
	health := map[string]string{"live": "unhealthy", "dead": "unhealthy"}
	remote := map[string]runtime.NodeTask{"dead": {}, "exited": {}, "live": {}, "offnode": {}}

	mergeRemote(state, health, remote)

	require.Equal(t, "running", state["dead"], "created corpse must not beat a running task")
	require.Equal(t, "running", state["exited"])
	require.Equal(t, "running", state["offnode"], "tile with no local container at all")
	require.Empty(t, health["dead"], "the corpse's health goes with it")

	// A local container that is actually running keeps its finer-grained health.
	require.Equal(t, "running", state["live"])
	require.Equal(t, "unhealthy", health["live"])
}

// Nothing in the task list means nothing is touched: a tile really stopped
// everywhere must still reconcile to stopped.
func TestMergeRemoteEmptyLeavesLocalAlone(t *testing.T) {
	state := map[string]string{"a": "exited"}
	mergeRemote(state, map[string]string{}, nil)
	require.Equal(t, "exited", state["a"])
}

package container

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
)

func ctr(name, state string) ctRow {
	return ctRow{ManagedContainer: runtime.ManagedContainer{ID: name, Name: name, State: state}}
}

// Swarm keeps several generations of every task, so the page grew to
// 22 rows of which 15 were corpses. Hidden by default, counted always: the
// remove button on an exited agent or proxy row is the only way to clear
// it, so the count must never silently become zero rows.
func TestApplyFilterHidesStoppedButCountsThem(t *testing.T) {
	rows := []ctRow{
		ctr("live", "running"),
		ctr("corpse", "exited"),
		ctr("never-started", "created"),
		ctr("other", "running"),
	}

	out, stopped := applyFilter(rows, false)
	require.Equal(t, 2, stopped, "both non-running rows are counted")
	require.Len(t, out, 2)
	for _, r := range out {
		require.Equal(t, "running", r.State)
		require.False(t, r.ShowStopped, "links stay on the default view")
	}

	out, stopped = applyFilter(rows, true)
	require.Equal(t, 2, stopped, "the count is the same either way")
	require.Len(t, out, 4)
	for _, r := range out {
		require.True(t, r.ShowStopped, "an action must return to the view it was taken from")
	}
}

// The filter has to survive the poll that replaces the table, and an action
// on a row has to come back to the same view.
func TestFilterSurvivesTheRoundTrip(t *testing.T) {
	require.Equal(t, "/containers", listURL(false))
	require.Equal(t, "/containers?stopped=1", listURL(true))

	r := ctRow{ManagedContainer: runtime.ManagedContainer{ID: "abc"}}
	require.Equal(t, "/containers/abc/remove", r.path("/remove"))

	r.ShowStopped = true
	require.Equal(t, "/containers/abc/remove?stopped=1", r.path("/remove"))

	r.Node = "n1"
	require.Equal(t, "/containers/abc/remove?node=n1&stopped=1", r.path("/remove"))

	r.ShowStopped = false
	require.Equal(t, "/containers/abc/remove?node=n1", r.path("/remove"))
}

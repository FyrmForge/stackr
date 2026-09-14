package app

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

func tileWithHome(node string) *repo.Tile { return &repo.Tile{HomeNode: node} }

// PlacementView carried a Down bool, and a server is pending, ready,
// draining or down. Collapsing three of those into one flag made the overview
// tell an operator their node was "not answering" thirty seconds after they
// drained it themselves, sending them to the network when the fix was the
// Activate button. The cause a message names is the fix it implies.
func TestPlacementReasonNamesTheRealCause(t *testing.T) {
	cases := []struct {
		status string
		down   bool
		says   string
	}{
		{"ready", false, ""},
		{"draining", true, "is draining"},
		{"pending", true, "has not finished joining"},
		{"down", true, "is not answering"},
		{"gone", true, "no longer in the swarm"},
	}
	for _, c := range cases {
		t.Run(c.status, func(t *testing.T) {
			v := placementView{Name: "worker-2.example.test", Status: c.status}
			require.Equal(t, c.down, v.Down())
			if !c.down {
				return
			}
			msg := placementReason(v)
			require.Contains(t, msg, c.says)
			require.Contains(t, msg, "worker-2.example.test", "always name the machine")
		})
	}

	// The one that caused the bug: a drained node must not be described as
	// unreachable, and must point at the action that fixes it.
	drained := placementReason(placementView{Name: "worker-2.example.test", Status: "draining"})
	require.NotContains(t, drained, "not answering")
	require.True(t, strings.Contains(drained, "Activate"), "say what to do: %q", drained)
}

// An unpinned tile has no node to report on and must not claim anything is
// wrong with one.
func TestPlacementViewUnpinnedIsNotDown(t *testing.T) {
	require.False(t, placementView{}.Down())
}

// Every other screen says the hostname; Placement printed the id.
func TestHomeNodeNamePrefersTheHostname(t *testing.T) {
	tile := tileWithHome("vnixldiwsrqsb6908e4n4vwad")
	require.Equal(t, "worker-2.example.test",
		homeNodeName(tile, placementView{Name: "worker-2.example.test", Status: "ready"}))
	require.Equal(t, "vnixldiwsrqsb6908e4n4vwad", homeNodeName(tile, placementView{}),
		"a node with no row still says something rather than going blank")
}

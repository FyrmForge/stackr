package graph

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The card has no status footer, so its subtitle carries both counts. A missing
// member count is omitted rather than rendered as "0 members".
func TestOrgCardSubtitle(t *testing.T) {
	for _, tc := range []struct {
		stacks, members int
		pending         bool
		want            string
	}{
		{3, 2, false, "3 stacks · 2 members"},
		{1, 1, false, "1 stack · 1 member"},
		{0, 4, false, "0 stacks · 4 members"},
		{2, 0, false, "2 stacks"},
		// Counts are noise on an org you cannot open yet: clicking the card
		// redirects the owner to the wizard and walls everyone else off, and
		// "0 stacks" gives no hint of either.
		{0, 1, true, "Setup unfinished"},
	} {
		assert.Equal(t, tc.want, orgDetail(tc.stacks, tc.members, tc.pending),
			"orgDetail(%d, %d, %v)", tc.stacks, tc.members, tc.pending)
	}
}

func TestBuildOrgsIsEdgelessAndNavigable(t *testing.T) {
	g := BuildOrgs([]OrgSummary{
		{ID: "o1", Name: "Acme", Slug: "acme", Href: "/orgs/o1", Stacks: 3, Members: 2},
		{ID: "o2", Name: "Solo", Slug: "solo", Href: "/orgs/o2", Stacks: 1, Members: 1},
	}, nil)

	require.Empty(t, g.Edges, "orgs share nothing, so the canvas must be edgeless")
	byID := map[string]Node{}
	for _, n := range g.Nodes {
		byID[n.ID] = n
	}
	acme := byID[OrgNodeID("o1")]
	require.Equal(t, KindOrg, acme.Kind, "org card wrong: %+v", acme)
	require.Equal(t, "3 stacks · 2 members", acme.Detail, "org card wrong: %+v", acme)
	require.True(t, acme.Nav, "org card wrong: %+v", acme)
	require.Equal(t, "/orgs/o1", acme.Href, "org card wrong: %+v", acme)
	solo := byID[OrgNodeID("o2")]
	require.Equal(t, "1 stack · 1 member", solo.Detail, "singular subtitle / deck wrong: %+v", solo)
	require.Equal(t, 1, solo.Deck, "singular subtitle / deck wrong: %+v", solo)
	// Nothing sets Status on an org card, it has no footer to show one in.
	assert.Empty(t, acme.Status, "org card carries a status: %q", acme.Status)
	// Deck is capped so a busy org doesn't grow an unbounded pile of layers.
	require.Equal(t, 2, acme.Deck, "deck want capped at 2")
	// Two cards must never share a spot, or one is invisible until dragged.
	require.False(t, acme.X == byID[OrgNodeID("o2")].X && acme.Y == byID[OrgNodeID("o2")].Y,
		"cards overlap at %v,%v", acme.X, acme.Y)
}

// A saved drag wins over the auto grid, and unsaved cards still get placed.
func TestBuildOrgsKeepsDraggedPositions(t *testing.T) {
	pos := map[string][2]float64{OrgNodeID("o1"): {900, 40}}
	g := BuildOrgs([]OrgSummary{
		{ID: "o1", Name: "Acme"},
		{ID: "o2", Name: "Solo"},
	}, pos)

	for _, n := range g.Nodes {
		switch n.ID {
		case OrgNodeID("o1"):
			assert.Equal(t, 900.0, n.X, "dragged card moved: %+v", n)
			assert.Equal(t, 40.0, n.Y, "dragged card moved: %+v", n)
			assert.True(t, n.Saved, "dragged card moved: %+v", n)
		case OrgNodeID("o2"):
			assert.False(t, n.Saved, "never-dragged card marked saved: %+v", n)
			assert.False(t, n.X == 900 && n.Y == 40, "auto-placed card landed under the dragged one")
		}
	}
}

// The grid wraps rather than growing one endless column.
func TestBuildOrgsWrapsIntoColumns(t *testing.T) {
	var orgs []OrgSummary
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		orgs = append(orgs, OrgSummary{ID: id, Name: id})
	}
	g := BuildOrgs(orgs, nil)
	cols := map[float64]int{}
	for _, n := range g.Nodes {
		cols[n.X]++
	}
	require.Len(t, cols, 2, "5 orgs at %d per column want 2 columns: %v", orgsPerColumn, cols)
}

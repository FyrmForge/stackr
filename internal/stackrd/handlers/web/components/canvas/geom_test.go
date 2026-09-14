package canvas

import (
	"regexp"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/handlers/web/graph"
)

// A deck is one card plus a sub-tile per thing attached to it, every box the
// same size, each dealt 28px further down so only its bottom strip shows. That
// only reads as a deck if the card paints over the first sub-tile, the first
// over the second, and so on: a later sub-tile's body lands exactly on the
// strip that is all you see of the one before it.
//
// The class alone puts every sub-tile at z-index 0, so document order decided,
// and document order is the wrong way round: the tile with two volumes drew
// one blank card and one label. geomCSS owns the
// deck's z, and this is the ordering it has to keep.
//
// Replica rows are dealt past the volumes into that same deck, so they take
// the same ordering. Both kinds go in one node here rather than leaving the
// replicaSubIDs branch to run only in production.
func TestGeomCSSPaintsEachDeckFrontToBack(t *testing.T) {
	n := graph.Node{
		ID: "tile1", X: 100, Y: 200,
		Subtiles: []graph.Subtile{{Kind: "volume", Name: "vola"}, {Kind: "volume", Name: "volb"}},
	}
	n.Replicas.Rows = []graph.ReplicaRow{{Slot: 2}, {Slot: 3}}
	n.Replicas.Hidden = 4

	css := geomCSS(graph.Graph{Nodes: []graph.Node{n}})

	// The card first, then the deck in the order geomCSS deals it: volumes,
	// then replica rows, then the "+n more" row.
	deck := []string{`\[data-node-id="tile1"\]`}
	for i := range n.Subtiles {
		deck = append(deck, `\[data-sub-id="tile1/`+strconv.Itoa(i)+`"\]`)
	}
	for _, id := range replicaSubIDs(n) {
		deck = append(deck, `\[data-sub-id="`+regexp.QuoteMeta(id)+`"\]`)
	}
	require.Len(t, deck, 6, "2 volumes + 2 replica rows + the +n more row, under the card")

	for i := 1; i < len(deck); i++ {
		above, below := zOf(t, css, deck[i-1]), zOf(t, css, deck[i])
		assert.Greater(t, above, below,
			"deck entry %d must paint over %d: its body covers that one's label strip", i-1, i)
	}
}

// A card with nothing attached is left to its class. .graph-node-volume sits a
// volume card under the tile it belongs to at z-index 0, and an attribute
// selector here would tie on specificity and beat it from the body.
func TestGeomCSSLeavesAnUndeckedCardsZIndexAlone(t *testing.T) {
	g := graph.Graph{Nodes: []graph.Node{{ID: "solo", X: 10, Y: 20}}}

	rule := ruleFor(t, geomCSS(g), `\[data-node-id="solo"\]`)
	assert.NotContains(t, rule, "z-index",
		"an undecked card must not carry a z-index: it would override .graph-node-volume")
}

func ruleFor(t *testing.T, css, sel string) string {
	t.Helper()
	m := regexp.MustCompile(sel + `\{([^}]*)\}`).FindStringSubmatch(css)
	require.Len(t, m, 2, "no rule for %s in:\n%s", sel, css)
	return m[1]
}

func zOf(t *testing.T, css, sel string) int {
	t.Helper()
	m := regexp.MustCompile(`z-index:(-?\d+)`).FindStringSubmatch(ruleFor(t, css, sel))
	require.Len(t, m, 2, "no z-index for %s", sel)
	n, err := strconv.Atoi(m[1])
	require.NoError(t, err)
	return n
}

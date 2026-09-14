package canvas

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/handlers/web/graph"
)

// The client builds a port-forward card from what this returns and nothing
// else, so the payload has to carry everything the card needs. When it was
// building the markup itself, the copy drifted unnoticed, that path only runs
// with a live tunnel open.
func TestStatusNodesShipEphemeralCardsWhole(t *testing.T) {
	ctx := context.Background()
	fwd := graph.Node{
		ID: "forward:abcd1234:5432", Kind: graph.KindForward, Name: "sharedpg", Ephemeral: true,
		Forwards: []graph.ForwardUser{{Name: "Ada Lovelace", Role: "owner"}},
	}
	ordinary := graph.Node{ID: graph.AppNodeID("a1"), Kind: graph.KindApp, Name: "web"}

	out := StatusNodes(ctx, []graph.Node{fwd, ordinary})
	require.Len(t, out, 2)

	got := out[0]
	require.False(t, got.HTML == "" || got.Class == "" || got.Height == 0,
		"ephemeral node missing what the client needs to draw it: %+v", got)
	assert.False(t, got.Class != nodeClass(fwd) || got.Height != nodeHFor(fwd),
		"ephemeral card's dress must be the same one the page renders")
	// It is the real card, not a placeholder: the person holding the tunnel is
	// named in it.
	assert.Contains(t, got.HTML, "Ada Lovelace", "rendered face missing its forward user")
	// Escaping is templ's, not ours, check it actually happened.
	danger := graph.Node{ID: "forward:x:1", Kind: graph.KindForward, Ephemeral: true,
		Forwards: []graph.ForwardUser{{Name: `<script>alert(1)</script>`}}}
	html := StatusNodes(ctx, []graph.Node{danger})[0].HTML
	assert.NotContains(t, html, "<script>", "forward user name not escaped")

	// An ordinary card already exists in the page; re-sending its whole markup
	// would be waste. Only its footer (the part that changes) comes back.
	assert.False(t, out[1].HTML != "" || out[1].Class != "",
		"non-ephemeral node should carry no card markup: %+v", out[1])
	assert.NotEqual(t, "", out[1].Footer, "an ordinary card must carry its rendered footer")
}

// The footer is the one line on a card that changes while you watch it, and
// the client no longer decides what it says. Each kind's wording comes from
// the same component the page rendered.
func TestStatusNodesRenderFootersPerKind(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		node graph.Node
		want string
	}{
		{"a running service reads Online", graph.Node{Kind: graph.KindApp, Status: "running"}, "Online"},
		{"a finished job reads Online too", graph.Node{Kind: graph.KindApp, Status: "done"}, "Online"},
		{"a crash says so", graph.Node{Kind: graph.KindApp, Status: "error"}, "Crashed"},
		{"nothing run yet is idle", graph.Node{Kind: graph.KindApp}, "idle"},
		{"a cron shows its last run", graph.Node{Kind: graph.KindCron, LastRun: "ok · Aug 2 03:00"}, "ok · Aug 2 03:00"},
		{"a paused cron says paused", graph.Node{Kind: graph.KindCron, LastRun: "never run"}, "paused"},
	}
	for _, c := range cases {
		got := StatusNodes(ctx, []graph.Node{c.node})[0].Footer
		assert.Contains(t, got, c.want, "%s", c.name)
	}

	// An org card has no footer at all, an organization is not running or
	// idle, and an empty strip reads as a missing value.
	assert.Equal(t, "", StatusNodes(ctx, []graph.Node{{Kind: graph.KindOrg, Name: "acme"}})[0].Footer,
		"org card should have no footer")
}

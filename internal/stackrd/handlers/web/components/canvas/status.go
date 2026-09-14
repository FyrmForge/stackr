package canvas

import (
	"bytes"
	"context"

	"github.com/a-h/templ"

	"github.com/FyrmForge/stackr/internal/stackrd/handlers/web/graph"
)

// The canvas is server-rendered, except for the cards that come and go without
// a page load: a port-forward relay exists only while its container does. The
// client used to rebuild those in JavaScript, which meant one card's markup
// written twice, and the copy that only runs when a tunnel is open is the one
// no test, screenshot or click-through ever exercises. So the server renders
// them here too, and the client only places what it is handed.

// StatusNode is a node as the status endpoint reports it: the graph node plus,
// for the cards the client has to create itself, everything needed to build
// one, its markup, its wrapper class and its height.
type StatusNode struct {
	graph.Node
	HTML   string `json:"html,omitempty"`
	Class  string `json:"class,omitempty"`
	Height int    `json:"height,omitempty"`
	// Footer is the card's bottom strip re-rendered: the status light, a
	// cron's last result, the live forward count. The client swaps it in
	// whole rather than deciding what those should say.
	Footer string `json:"footer,omitempty"`
}

// StatusNodes prepares a status payload's nodes. Only ephemeral ones carry
// markup: every other card already exists in the page, and the client just
// updates its status and position.
func StatusNodes(ctx context.Context, nodes []graph.Node) []StatusNode {
	out := make([]StatusNode, 0, len(nodes))
	for _, n := range nodes {
		sn := StatusNode{Node: n}
		if n.Ephemeral {
			sn.HTML = render(ctx, ForwardFace(n))
			sn.Class = nodeClass(n)
			sn.Height = nodeHFor(n)
		} else if hasFooter(n) {
			sn.Footer = render(ctx, NodeFooter(n))
		}
		out = append(out, sn)
	}
	return out
}

// hasFooter reports whether this card kind draws a bottom strip. An org card
// has none (an organization is not running or idle) and a volume or forward
// card wears a face of its own instead.
func hasFooter(n graph.Node) bool {
	switch n.Kind {
	case graph.KindOrg, graph.KindVolume, graph.KindForward:
		return false
	}
	return true
}

// render renders a component to a string. A render error leaves the piece
// empty rather than failing the whole poll, the canvas keeps working and the
// gap is visible.
func render(ctx context.Context, comp templ.Component) string {
	var buf bytes.Buffer
	if err := comp.Render(ctx, &buf); err != nil {
		return ""
	}
	return buf.String()
}

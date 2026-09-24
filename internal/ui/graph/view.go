// Package graph draws one canvas (ui-plan §2): <graph-canvas>, a
// <graph-node> per card, the edges, notes and boxes, the legend and the
// toolbar. The graph service decides every card; handlers map its view
// into these structs and the templ only draws. Env cards bring their own
// body (Node.Card, internal/ui/graph/cards).
package graph

import (
	"strconv"

	"github.com/a-h/templ"

	"github.com/FyrmForge/stackr/internal/ui/graph/cards"
)

// View is one canvas.
type View struct {
	Base    string // the canvas's path, "" on home; its helper routes are Base+"/-/..."
	Query   string // the show params as the URL carries them ("" or "?system=0&..."), kept on every helper route
	Events  string // the SSE URL; "" = no stream
	Toggles bool   // draw the system/refs/startup/traffic switches (env)
	Show    Show
	Focus   string // ?focus=: the card to centre
	Nodes   []Node
	Edges   []Edge
	Notes   []Note
	Divider int // world x of the system column's wall; 0 = none
	Compare []Rung
	Create  []Create     // the level's create dialogs the viewer may open
	Lanes   []cards.Lane // env traffic at the last sample; nil = no lanes layer
}

// Create opens a create dialog in the drawer.
type Create struct{ Label, URL string }

// Show is what the server draws; each off flag is a "=0" query param.
type Show struct{ System, Refs, Startup, Traffic bool }

// Node is one card.
type Node struct {
	ID, Kind, Name, Detail string
	Slug                   string // the drill-down path segment; a connector's id
	Status                 string // one word, "" = nothing to show
	X, Y, W, H             int    // H includes 30 px per sub-tile
	System, Static         bool
	Color                  string // env colour, "" = none
	Deck                   int    // drill-down cards: 0 to 2 layers under the card
	Href                   string // drill-down page; "" = none
	Drawer                 string // drawer GET, with ?drawer=&tab=; "" = none
	Push                   string // the URL the drawer pushes: ?drawer=<id>&tab=<tab>
	Subs                   []Sub
	Card                   templ.Component // replaces the generic body and sub-tiles (env kinds)
	Footer                 templ.Component // replaces the generic footer; the stream sends it too
}

// Sub is a sub-tile under a card.
type Sub struct{ ID, Kind, Name, Status string }

// Edge is one server-drawn edge; the canvas re-paths it.
type Edge struct{ Kind, From, To string }

// Note is an annotation: kind note (text) or box (a labelled frame).
type Note struct {
	ID, Kind, Text string
	X, Y, W, H     int
}

// Rung is one env of the stack's ladder in the env-compare pill.
type Rung struct {
	Name, Color, Href string
	Release           int // 0 = never released
	Behind            bool
}

// Route is a helper route of this canvas, show params kept.
func (v View) Route(name string) string { return v.Base + "/-/" + name + v.Query }

// Foot is what the card's footer is, here and on the stream.
func (n Node) Foot() templ.Component {
	if n.Footer != nil {
		return n.Footer
	}
	return Footer(n.ID, n.Status)
}

// Toggle is the page URL with one show param flipped.
func (v View) Toggle(name string) string {
	s := v.Show
	switch name {
	case "system":
		s.System = !s.System
	case "refs":
		s.Refs = !s.Refs
	case "startup":
		s.Startup = !s.Startup
	case "traffic":
		s.Traffic = !s.Traffic
	}
	path := v.Base
	if path == "" {
		path = "/"
	}
	return path + s.Query()
}

// Query spells the off flags, "" when everything is drawn.
func (s Show) Query() string {
	q := ""
	for _, f := range []struct {
		on   bool
		name string
	}{{s.System, "system"}, {s.Refs, "refs"}, {s.Startup, "startup"}, {s.Traffic, "traffic"}} {
		if !f.on {
			q += "&" + f.name + "=0"
		}
	}
	if q == "" {
		return ""
	}
	return "?" + q[1:]
}

var edgeLabels = map[string]string{
	"ref": "uses", "ingress": "ingress", "egress": "egress", "shared": "shared config",
	"startup": "starts after", "config": "config repo", "source": "source repo",
}

// legend is the edge kinds on this canvas, in a fixed order.
func (v View) legend() []string {
	var out []string
	for _, k := range []string{"ref", "ingress", "egress", "shared", "startup", "config", "source"} {
		for _, e := range v.Edges {
			if e.Kind == k {
				out = append(out, k)
				break
			}
		}
	}
	return out
}

func itoa(n int) string { return strconv.Itoa(n) }

func boolStr(b bool) string { return strconv.FormatBool(b) }

// deck is the look of 1 or 2 cards stacked under a drill-down card.
func deck(n int) string {
	switch {
	case n >= 2:
		return "shadow-[5px_5px_0_-1px_#cbd5e1,10px_10px_0_-2px_#e2e8f0]"
	case n == 1:
		return "shadow-[5px_5px_0_-1px_#cbd5e1]"
	}
	return ""
}

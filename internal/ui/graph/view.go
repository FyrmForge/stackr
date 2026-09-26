// Package graph draws one canvas (ui-plan §2) in v0's look: <graph-canvas>,
// a <graph-node> per card, the edges, notes and boxes, the server boundary,
// the legend, the controls and the View panel. The graph service decides
// every card; handlers map its view into these structs and the templ only
// draws. Env cards bring their own body (Node.Card, internal/ui/graph/cards).
package graph

import (
	"maps"
	"slices"
	"strconv"

	"github.com/a-h/templ"

	c "github.com/FyrmForge/stackr/internal/ui/components"
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
	Walled  bool // system cards and the rest both drawn: the server boundary between them
	Divider int  // world x of that boundary (0 is a real place)
	Empty   string
	Compare []Rung
	Create  []Create     // the level's create dialogs the viewer may open
	Lanes   []cards.Lane // env traffic at the last sample; nil = no lanes layer
	Banner  *Banner      // the strip over the canvas; nil = none
}

// Banner is v0's orgPlanBanner: a strip over the canvas linking to what
// it is about, warning tone unless Danger.
type Banner struct {
	Text, Href string
	Danger     bool
}

// Create opens a create dialog in the drawer, or with Nav goes to URL (the
// setup wizard is a page).
type Create struct {
	Label, URL string
	Nav        bool
}

// Show is what the server draws; each off flag is a "=0" query param.
type Show struct{ System, Refs, Startup, Traffic bool }

// Node is one card.
type Node struct {
	ID, Kind, Name, Detail string
	Slug                   string // the drill-down path segment; a connector's id
	Status                 string // one word, "" = nothing to show
	X, Y, W, H             int    // the card's own box; sub-tiles hang below it
	System, Static         bool
	Color                  string // env hue, "" = none
	Deck                   int    // drill-down cards: 0 to 2 layers under the card
	Href                   string // drill-down page; "" = none
	Drawer                 string // drawer GET, with ?drawer=&tab=; "" = none
	Push                   string // the URL the drawer pushes: ?drawer=<id>&tab=<tab>
	Subs                   []Sub
	Card                   templ.Component // replaces the generic face and sub-tiles (env kinds)
	Footer                 templ.Component // replaces the generic footer; the stream sends it too
}

// Sub is a sub-tile under a card; Drawer is its own drawer GET (env
// cards), which a fresh load of ?drawer=<sub id> opens.
type Sub struct{ ID, Kind, Name, Status, Drawer string }

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

// Foot is what the card's footer is, here and on the stream: v0's strip
// by kind ("open settings" on vars and connectors, the status word on the
// rest).
func (n Node) Foot() templ.Component {
	if n.Footer != nil {
		return n.Footer
	}
	return cards.Footer(n.ID, cards.FooterView{Kind: n.Kind, Status: n.Status})
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
	}{
		{s.System, "system"},
		{s.Refs, "refs"},
		{s.Startup, "startup"},
		{s.Traffic, "traffic"},
	} {
		if !f.on {
			q += "&" + f.name + "=0"
		}
	}
	if q == "" {
		return ""
	}
	return "?" + q[1:]
}

// Edge looks (v0 edgeCurve): one place, drawn on the paths and on the
// legend swatches alike.
var edgeLooks = map[string]templ.Attributes{
	"ref":     {"stroke": "rgb(var(--rw-strong))", "stroke-dasharray": "4 5"},
	"ingress": {"stroke": "rgb(var(--rw-accent))", "stroke-opacity": "0.7"},
	"egress":  {"stroke": "var(--rw-amber)", "stroke-opacity": "0.6"},
	"shared":  {"stroke": "rgb(var(--rw-accent))", "stroke-dasharray": "5 4", "stroke-opacity": "0.6"},
	"startup": {"stroke": "var(--rw-amber)", "stroke-width": "1.2", "stroke-opacity": "0.5", "stroke-dasharray": "1 5"},
	"config":  {"stroke": "var(--rw-edge-connector)", "stroke-opacity": "0.75"},
	"source":  {"stroke": "var(--rw-edge-connector)", "stroke-dasharray": "4 6", "stroke-opacity": "0.4"},
}

// EdgeAttrs is an edge path's stroke for its kind (the dev gallery draws
// its sample edges with it too).
func EdgeAttrs(kind string) templ.Attributes {
	a := templ.Attributes{"fill": "none", "stroke-width": "1.5", "stroke-linecap": "round"}
	maps.Copy(a, edgeLooks[kind])
	return a
}

// legendRows is v0's legend order, with the words each kind reads as.
var legendRows = []struct{ kind, label string }{
	{"ingress", "public route"},
	{"egress", "outbound"},
	{"config", "config as code"},
	{"source", "source repo"},
	{"shared", "managed instance"},
	{"ref", "reference"},
	{"startup", "startup order"},
}

// legend is the legend rows of the edge kinds on this canvas.
func (v View) legend() []struct{ kind, label string } {
	var out []struct{ kind, label string }
	for _, r := range legendRows {
		if slices.ContainsFunc(v.Edges, func(e Edge) bool { return e.Kind == r.kind }) {
			out = append(out, r)
		}
	}
	return out
}

// wall is the boundary's run: 120 px past the top and bottom cards (v0).
func (v View) wall() (top, bottom int) {
	for i, n := range v.Nodes {
		if i == 0 || n.Y < top {
			top = n.Y
		}
		if i == 0 || n.Y+n.H > bottom {
			bottom = n.Y + n.H
		}
	}
	return top - 120, bottom + 120
}

// hue is the class that sets --env-c to an env's colour; "" for none.
func hue(color string) string {
	if !slices.Contains(c.EnvColors, color) {
		return ""
	}
	return "env-c-" + color
}

func itoa(n int) string {
	return strconv.Itoa(n)
}

// behindTitle spells the arrow's tooltip; a rung with no release yet is
// not "release #0".
func behindTitle(r Rung) string {
	if r.Release == 0 {
		return "No release yet; the env below runs one"
	}
	return "Runs release #" + itoa(r.Release) + "; the env below runs a newer one"
}

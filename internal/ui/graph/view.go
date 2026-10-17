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
	"strings"

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
	Create  []Create        // the level's create dialogs the viewer may open
	Lanes   []cards.Lane    // env traffic at the last sample; nil = no lanes layer
	Banner  *Banner         // the strip over the canvas; nil = none
	SyncBar *SyncBar        // the env sync review's strip; nil = none
	Over    templ.Component // drawn over the canvas with the chrome (the Cluster map's legend); nil = none
	Review  string          // the review the page carries, "sync=dev&drop=a,b"; kept on the helper routes and the show switches
	FanGap  int             // px between edge ends fanned out on one card side; 0 = the canvas default
}

// SyncBar is the strip under the banner while an env sync is under review:
// the count, Review (the drawer's tab), Deploy (the drawer's confirm; nil
// = the viewer cannot, or the plan is blocked or empty) and Cancel (the env
// page, no review).
type SyncBar struct {
	Text           string
	Review, Cancel string // URLs
	Deploy         *c.ConfirmView
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
	// Members makes the card a container (the Cluster map's network box):
	// the canvas keeps it wrapped around these cards, Pad (top right bottom
	// left) out and at least MinW wide, and it drags them along.
	Members []string
	Pad     [4]int
	MinW    int
	// UserW and UserH are a box's hand-set size: it never wraps smaller.
	UserW, UserH   int
	System, Static bool
	Color          string // env hue, "" = none
	Deck           int    // drill-down cards: 0 to 2 layers under the card
	Href           string // drill-down page; "" = none
	Drawer         string // drawer GET, with ?drawer=&tab=; "" = none
	Push           string // the URL the drawer pushes: ?drawer=<id>&tab=<tab>
	Subs           []Sub
	Card           templ.Component // replaces the generic face and sub-tiles (env kinds)
	Footer         templ.Component // replaces the generic footer; the stream sends it too
}

// Sub is a sub-tile under a card; Drawer is its own drawer GET (env
// cards), which a fresh load of ?drawer=<sub id> opens.
type Sub struct{ ID, Kind, Name, Status, Drawer string }

// Edge is one server-drawn edge; the canvas re-paths it. Label rides its
// middle ("" = none). Dir draws an arrow From -> To on a tunnel; Drawer is
// the GET a click on the line sends (into #drawer-body) and Push the URL it
// pushes, "" Drawer = the line is not clickable.
type Edge struct {
	Kind, From, To, Label string
	Dir                   bool
	Drawer, Push          string
}

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

// Route is a helper route of this canvas, show params and review kept.
func (v View) Route(name string) string { return v.Base + "/-/" + name + v.carry(v.Query) }

// carry is a "?a=b" query with the review's params added.
func (v View) carry(q string) string {
	switch {
	case v.Review == "":
		return q
	case q == "":
		return "?" + v.Review
	}
	return q + "&" + v.Review
}

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
	return path + v.carry(s.Query())
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
	// The Cluster map's: encrypted tunnels between nodes (green over the
	// internet, blue on a private network), a NAT's port forward (long dashes,
	// dotted while unconfirmed), backups to an outside store.
	"tunnel":         {"stroke": "var(--rw-tunnel-net)", "stroke-width": "6"},
	"tunnel-lan":     {"stroke": "var(--rw-tunnel-lan)", "stroke-width": "5"},
	"tunnel-down":    {"stroke": "rgb(var(--rw-danger))", "stroke-width": "4.5", "stroke-opacity": "0.6", "stroke-dasharray": "6 6", "stroke-linecap": "butt"},
	"forward":        {"stroke": "var(--rw-env-sky)", "stroke-width": "2", "stroke-dasharray": "8 3"},
	"forward-unsure": {"stroke": "var(--rw-env-sky)", "stroke-width": "2", "stroke-dasharray": "2 4"},
	"backup":         {"stroke": "rgb(var(--rw-faint))", "stroke-dasharray": "2 4"},
	// Egress on the Cluster map: own apps leaving by the node's own uplink
	// (thin, faded), and traffic exiting through another node (dashed).
	"egress-own": {"stroke": "var(--rw-amber)", "stroke-width": "1", "stroke-opacity": "0.35"},
	"egress-via": {"stroke": "var(--rw-amber)", "stroke-dasharray": "3 4", "stroke-opacity": "0.5"},
}

// edgeCores hollow a kind into a pipe: a background-coloured stroke drawn
// over the edge leaves two parallel lines (a tunnel looks like one).
var edgeCores = map[string]templ.Attributes{
	"tunnel":      {"stroke-width": "2.6"},
	"tunnel-lan":  {"stroke-width": "2.2"},
	"tunnel-down": {"stroke-width": "1.9"},
}

// EdgeCore is the pipe's inner stroke for a kind; ok false for a plain line.
func EdgeCore(kind string) (templ.Attributes, bool) {
	core, ok := edgeCores[kind]
	if !ok {
		return nil, false
	}
	a := templ.Attributes{"fill": "none", "stroke": "rgb(var(--rw-bg))", "stroke-linecap": "round"}
	maps.Copy(a, core)
	return a, true
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
	{"ingress", "ingress"},
	{"egress", "egress"},
	{"egress-own", "egress, own apps"},
	{"egress-via", "egress through another node"},
	{"config", "config as code"},
	{"source", "source repo"},
	{"shared", "managed instance"},
	{"ref", "reference"},
	{"startup", "startup order"},
	{"tunnel", "encrypted tunnel, internet"},
	{"tunnel-lan", "encrypted tunnel, private"},
	{"tunnel-down", "tunnel down"},
	{"forward", "port forward"},
	{"forward-unsure", "port forward, unconfirmed"},
	{"backup", "backup"},
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

// tunnels says the canvas draws a mesh tunnel, whose arrow is the side
// that dials.
func (v View) tunnels() bool {
	return slices.ContainsFunc(v.Edges, func(e Edge) bool { return strings.HasPrefix(e.Kind, "tunnel") })
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

// sizeVal is a box's saved size field: empty until it is resized by hand,
// so a plain drag of the box posts no size.
func sizeVal(n int) string {
	if n <= 0 {
		return ""
	}
	return itoa(n)
}

// behindTitle spells the arrow's tooltip; a rung with no release yet is
// not "release #0".
func behindTitle(r Rung) string {
	if r.Release == 0 {
		return "No release yet; the env below runs one"
	}
	return "Runs release #" + itoa(r.Release) + "; the env below runs a newer one"
}

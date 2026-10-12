// Package cluster draws the Cluster map (lane U2) on the shared graph
// canvas: network boxes, the nodes in them, tunnels, public arrows, router
// markers and S3 backups. The service decides every fact (ClusterMap); the
// handler maps it into Map; Build places it. Placement is geometry only:
// boxes left to right, public nodes on a box's top edge, the rest in a grid
// below, each node's envs dealt under its card.
package cluster

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/FyrmForge/stackr/internal/ui/graph"
)

// Where things live; the node drawer and the add-node steps are the admin
// drawer's Nodes tab (internal/web/handler/admin/nodes.go).
const (
	Base       = "/-/cluster"
	nodesBase  = "/-/admin/nodes"
	AddNodeURL = nodesBase + "/add"
)

// NodeDrawer is a node's drawer GET (tab "" = its first), NodePush the URL
// the browser shows with it open.
func NodeDrawer(id, tab string) string {
	if tab == "" {
		return nodesBase + "/" + id
	}
	return nodesBase + "/" + id + "?tab=" + tab
}

func NodePush(id, tab string) string {
	if tab == "" {
		return "?drawer=node:" + id
	}
	return "?drawer=node:" + id + "&tab=" + tab
}

// Map is the service's ClusterMap as the templ reads it.
type Map struct {
	Nodes   []Node
	Nets    []Net
	Links   []Link
	Backups []Backup
}

type Node struct {
	ID, Name, Role, Status, Docker, Net string
	Tags, Public, Domains, ExitEnvs     []string
	Envs                                []Env
}

type Env struct{ Name, Href, Health string }

type Net struct {
	ID, CIDR string
	Nodes    []string
	Router   bool
}

type Link struct {
	A, B, Kind string
	MS         float64 // < 0 = not measured
}

type Backup struct {
	ID, Name string
	Nodes    []string
}

// Geometry, in world pixels.
const (
	cardW, cardH = 220, 132
	deckStep     = 28  // each env sub-card hangs this much lower (graph.css)
	maxEnvs      = 3   // envs dealt under a card; the fourth sub-card is "+N more"
	slotPitch    = 244 // public nodes along a box's top edge
	gapX         = 130 // between nodes in a row: room for a tunnel label
	rowGap       = 36
	padX         = 40
	headW        = 250 // the box's name sits left of its top-edge items
	boxGap       = 220 // between boxes
	top          = 260 // every box's top edge
	arrowLen     = 136
	routerSize   = 44
	minBoxW      = 360
)

// Shape is everything drawn behind the cards.
type Shape struct {
	Boxes   []Box
	Routers []Router
	Tunnels []Tunnel
	Arrows  []Arrow
	S3s     []S3
	Labels  []Label
	// Extent is the world box the picture spans, for the canvas to fit.
	X, Y, W, H int
}

type Box struct {
	X, Y, W, H int
	Name, Sub  string
}

type Router struct{ X, Y int } // top-left of the square

type Tunnel struct{ D string }

// Arrow is a public ingress or exit arrow, or a backup line.
type Arrow struct {
	D    string
	Kind string // ingress | exit | short | backup
}

type S3 struct {
	X, Y, W, H int
	Name       string
}

// Label is text on a plate. Anchor is start, middle or end.
type Label struct {
	X, Y, W int
	Text    string
	Anchor  string
	Mono    bool
}

type rect struct{ x, y int }

func (r rect) cx() int { return r.x + cardW/2 }
func (r rect) cy() int { return r.y + cardH/2 }

func plate(x, y int, text, anchor string) Label {
	w := int(math.Ceil(6.7*float64(len([]rune(text))))) + 10
	return Label{X: x, Y: y, W: w, Text: text, Anchor: anchor, Mono: true}
}

func (l Label) plateX() int {
	switch l.Anchor {
	case "middle":
		return l.X - l.W/2
	case "end":
		return l.X - l.W
	}
	return l.X - 4
}

// deck is how far a node's env sub-cards hang under its card.
func deck(n Node) int { return deckStep * len(subs(n)) }

// subs are the envs dealt under a card: the worst three, then a "+N more".
func subs(n Node) []Env {
	if len(n.Envs) <= maxEnvs {
		return n.Envs
	}
	out := slices.Clone(n.Envs[:maxEnvs])
	return append(out, Env{Name: fmt.Sprintf("+%d more", len(n.Envs)-maxEnvs), Health: "more"})
}

func hasTag(n Node, t string) bool { return slices.Contains(n.Tags, t) }

func netTitle(c Net) (name, sub string) {
	switch {
	case c.ID == "unknown":
		return "Not reported", "no interfaces seen yet"
	case c.CIDR == "":
		return "Public host", "public address only"
	case c.Router:
		return c.CIDR, "behind a router"
	}
	return c.CIDR, "private network"
}

// Build places the map and returns it as a graph canvas view.
func Build(m Map, base string, notes []graph.Note, create []graph.Create) graph.View {
	byID := map[string]Node{}
	for _, n := range m.Nodes {
		byID[n.ID] = n
	}
	pos := map[string]rect{}
	netOf := map[string]int{}
	var sh Shape
	nx := padX
	bottom := top
	type slot struct {
		cx, y int // centre x of the item and its top y
		in    bool
		exit  []string
		host  []string
	}
	var slots []slot
	for ni, c := range m.Nets {
		var edge, inner []Node
		for _, id := range c.Nodes {
			n, ok := byID[id]
			if !ok {
				continue
			}
			netOf[id] = ni
			if len(n.Public) > 0 {
				edge = append(edge, n)
			} else {
				inner = append(inner, n)
			}
		}
		// Top-edge items: the router marker first, then public nodes.
		items := len(edge)
		if c.Router {
			items++
		}
		edgeBand, k := 0, 0
		var grp slot
		if c.Router {
			cx := nx + headW + k*slotPitch + cardW/2
			sh.Routers = append(sh.Routers, Router{cx - routerSize/2, top - routerSize/2})
			grp = slot{cx: cx, y: top - routerSize/2}
			for _, n := range inner {
				if hasTag(n, "ingress") {
					grp.in = true
					grp.host = append(grp.host, n.Domains...)
				}
				grp.exit = append(grp.exit, n.ExitEnvs...)
			}
			if grp.in || len(grp.exit) > 0 {
				slots = append(slots, grp)
			}
			edgeBand = routerSize / 2
			k++
		}
		for _, n := range edge {
			x := nx + headW + k*slotPitch
			pos[n.ID] = rect{x, top - 66}
			edgeBand = max(edgeBand, 66+deck(n))
			if hasTag(n, "ingress") || len(n.ExitEnvs) > 0 {
				s := slot{cx: x + cardW/2, y: top - 66, exit: n.ExitEnvs, host: n.Domains}
				s.in = hasTag(n, "ingress")
				slots = append(slots, s)
			}
			k++
		}
		// The rest, two to a row.
		y0 := top + max(90, edgeBand+32)
		cols := min(len(inner), 2)
		y := y0
		innerBottom := 0
		for i := 0; i < len(inner); i += 2 {
			rowDeck := 0
			for j := i; j < min(i+2, len(inner)); j++ {
				pos[inner[j].ID] = rect{nx + padX + (j-i)*(cardW+gapX), y}
				rowDeck = max(rowDeck, deck(inner[j]))
			}
			innerBottom = y + cardH + rowDeck + 30
			y += cardH + rowDeck + rowGap
		}
		w := max(minBoxW, 2*padX+cols*cardW+max(cols-1, 0)*gapX)
		if items > 0 {
			w = max(w, headW+items*slotPitch-(slotPitch-cardW)+padX)
		}
		b := max(innerBottom, top+edgeBand+30+32)
		name, sub := netTitle(c)
		sh.Boxes = append(sh.Boxes, Box{X: nx, Y: top, W: w, H: b - top, Name: name, Sub: sub})
		bottom = max(bottom, b)
		nx += w + boxGap
	}

	// A tunnel leaves a card by the side facing the other box, or by the top
	// when a neighbour in its row would be crossed.
	anchor := func(r rect, net int, right bool) (int, int) {
		for id, o := range pos {
			if netOf[id] == net && o.y == r.y && ((right && o.x > r.x) || (!right && o.x < r.x)) {
				return r.cx(), r.y
			}
		}
		if right {
			return r.x + cardW, r.cy()
		}
		return r.x, r.cy()
	}

	// Tunnels, one per pair.
	pair := map[[2]int]int{}
	for _, l := range m.Links {
		a, aok := pos[l.A]
		b, bok := pos[l.B]
		if !aok || !bok {
			continue
		}
		label := l.Kind
		if l.MS >= 0 {
			label += " · " + trimMS(l.MS) + " ms"
		}
		na, nb := netOf[l.A], netOf[l.B]
		var d string
		var lx, ly int
		switch {
		case na != nb:
			if a.x > b.x {
				a, b = b, a
				na, nb = nb, na
			}
			x1, y1 := anchor(a, na, true)
			x2, y2 := anchor(b, nb, false)
			key := [2]int{na, nb}
			up := 70 + 28*pair[key]
			pair[key]++
			cy := min(y1, y2) - up
			d = fmt.Sprintf("M%d,%d Q%d,%d %d,%d", x1, y1, (x1+x2)/2, cy, x2, y2)
			lx, ly = (x1+2*((x1+x2)/2)+x2)/4, (y1+2*cy+y2)/4
		case a.y == b.y:
			if a.x > b.x {
				a, b = b, a
			}
			y := a.cy()
			d = fmt.Sprintf("M%d,%d L%d,%d", a.x+cardW, y, b.x, y)
			lx, ly = (a.x+cardW+b.x)/2, y
		default:
			// ponytail: same box, different rows; bulges out the left side. Boxes
			// with three or more rows can cross a neighbour; route around when seen.
			x := min(a.x, b.x)
			cx := x - 70
			d = fmt.Sprintf("M%d,%d Q%d,%d %d,%d", a.x, a.cy(), cx, (a.cy()+b.cy())/2, b.x, b.cy())
			lx, ly = (a.x+2*cx+b.x)/4, (a.cy()+b.cy())/2
		}
		sh.Tunnels = append(sh.Tunnels, Tunnel{D: d})
		sh.Labels = append(sh.Labels, plate(lx, ly+4, label, "middle"))
	}

	// Public ingress and exit arrows rise from the box edge.
	for _, s := range slots {
		if s.in {
			x := s.cx - 30
			sh.Arrows = append(sh.Arrows, Arrow{D: fmt.Sprintf("M%d,%d L%d,%d", x, s.y-arrowLen, x, s.y-2), Kind: "ingress"})
			txt := "ingress"
			if len(s.host) > 0 {
				txt = s.host[0]
				if len(s.host) > 1 {
					txt += fmt.Sprintf(" +%d", len(s.host)-1)
				}
			}
			sh.Labels = append(sh.Labels, plate(x-8, s.y-arrowLen/2+4, txt, "end"))
		}
		if len(s.exit) > 0 {
			x := s.cx + 30
			sh.Arrows = append(sh.Arrows, Arrow{D: fmt.Sprintf("M%d,%d L%d,%d", x, s.y-2, x, s.y-arrowLen), Kind: "exit"})
			txt := s.exit[0]
			verb := " exits here"
			if len(s.exit) > 1 {
				txt += fmt.Sprintf(" +%d", len(s.exit)-1)
				verb = " exit here"
			}
			sh.Labels = append(sh.Labels, plate(x+8, s.y-arrowLen/2+4, txt+verb, "start"))
		}
	}
	for _, r := range sh.Routers { // the router hands traffic to the box below it
		cx := r.X + routerSize/2
		sh.Arrows = append(sh.Arrows,
			Arrow{D: fmt.Sprintf("M%d,%d L%d,%d", cx+10, r.Y+routerSize, cx+10, r.Y+routerSize+36), Kind: "short"},
			Arrow{D: fmt.Sprintf("M%d,%d L%d,%d", cx-10, r.Y+routerSize+36, cx-10, r.Y+routerSize), Kind: "short"})
	}

	// Backups sit under everything, each below the first node that writes to it.
	type want struct {
		b  Backup
		cx int
	}
	var ws []want
	for _, b := range m.Backups {
		for _, id := range b.Nodes {
			if r, ok := pos[id]; ok {
				ws = append(ws, want{b, r.cx()})
				break
			}
		}
	}
	slices.SortStableFunc(ws, func(a, b want) int { return a.cx - b.cx })
	s3y := bottom + 90
	next := padX
	for _, w := range ws {
		x := max(next, w.cx-100)
		sh.S3s = append(sh.S3s, S3{X: x, Y: s3y, W: 200, H: 44, Name: "S3 · " + w.b.Name})
		next = x + 260
		for _, id := range w.b.Nodes {
			r, ok := pos[id]
			if !ok {
				continue
			}
			sy := r.y + cardH + deck(byID[id])
			sh.Arrows = append(sh.Arrows, Arrow{
				D:    fmt.Sprintf("M%d,%d C%d,%d %d,%d %d,%d", r.x+40, sy, r.x+40, sy+70, x+100, s3y-70, x+100, s3y-2),
				Kind: "backup",
			})
		}
		bottom = s3y + 44
	}

	// Extent: boxes, arrows above them, S3 below.
	sh.X, sh.Y, sh.W, sh.H = 0, top-66-arrowLen-20, max(nx-boxGap, padX)+padX, bottom+padX-(top-66-arrowLen-20)
	for _, n := range m.Nodes { // a node's own card beyond a box's edge counts too
		if r, ok := pos[n.ID]; ok {
			sh.W = max(sh.W, r.x+cardW+padX)
		}
	}

	v := graph.View{
		Base:   base,
		Fixed:  true,
		Notes:  notes,
		Create: create,
		Under:  netLayer(sh),
		Over:   legends(),
	}
	for _, n := range m.Nodes {
		r, ok := pos[n.ID]
		if !ok {
			continue
		}
		v.Nodes = append(v.Nodes, graph.Node{
			ID: "node:" + n.ID, Kind: "node", Name: n.Name,
			X: r.x, Y: r.y, W: cardW, H: cardH, Static: true,
			Card: nodeCard(n),
		})
	}
	return v
}

func trimMS(ms float64) string {
	if ms < 10 {
		return strconv.FormatFloat(ms, 'f', 1, 64)
	}
	return strconv.FormatFloat(math.Round(ms), 'f', 0, 64)
}

// Health words and what each dot looks like (classes the stylesheet has).
func dotClass(h string) string {
	switch h {
	case "healthy":
		return "bg-rw-success"
	case "degraded":
		return "bg-rw-warn"
	case "down":
		return "bg-rw-danger"
	case "moving":
		return "env-c-sky"
	}
	return "bg-rw-faint"
}

func healthWord(h string) string {
	if h == "" {
		return ""
	}
	return strings.ToUpper(h[:1]) + h[1:]
}

func statusTone(s string) (word, text, dot string) {
	switch s {
	case "online":
		return "Online", "text-rw-success", "bg-rw-success"
	case "unreachable":
		return "Not answering", "text-rw-danger", "bg-rw-danger"
	}
	return healthWord(s), "text-rw-warn", "bg-rw-warn"
}

func plural(n int, one string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + one + "s"
}

// Subtitle is the header line: "4 nodes · 2 networks".
func Subtitle(m Map) string {
	return plural(len(m.Nodes), "node") + " · " + plural(len(m.Nets), "network")
}

func itoa(n int) string { return strconv.Itoa(n) }

// Package cluster draws the Cluster map (lane U2) on the shared graph
// canvas: network boxes, the nodes, NATs, the internet and backup stores
// as ordinary cards, and tunnels, public traffic, port forwards and backups
// as ordinary edges, so they route, fan out and follow a drag the way the
// stack and env graphs do. The service decides every fact (ClusterMap); the
// handler maps it into Map; Build places it. A network box is a container
// card: the canvas keeps it wrapped around its cards and drags them along.
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
	// Placed is where admins dropped cards ("node:", "nat:", "s3:" ids and
	// "internet"), in world pixels. A box goes where its cards are.
	Placed map[string][2]int
}

type Node struct {
	ID, Name, Role, Status, Docker, Net string
	Tags, Public, Domains, ExitEnvs     []string
	Envs                                []Env
	NoForward                           bool // ingress behind a NAT the internet does not reach
}

type Env struct{ Name, Href, Health string }

type Net struct {
	ID, CIDR string
	Nodes    []string
	Router   bool
	Forward  string // the node the NAT forwards to; "" = not known
	Unsure   bool   // Forward came from inside the network
}

type Link struct {
	A, B, Kind string
	MS         float64 // < 0 = not measured
	Via        string  // relay node name, "" = direct
}

type Backup struct {
	ID, Name string
	Nodes    []string
}

// Geometry, in world pixels.
const (
	cardW      = 240
	faceH      = 156 // a node card without envs
	envRowH    = 22
	maxEnvs    = 3 // env rows on a card; more becomes "+N more"
	natW, natH = 104, 48
	sideW      = 168 // the internet and backup cards
	sideH      = 52
	padX       = 32 // a box's inside, left and right
	headH      = 64 // a box's header: its name and sub
	padB       = 32
	gapX       = 96
	gapY       = 56
	boxGap     = 160
	top        = 240 // every box's top edge before any drop
	minBoxW    = 320
)

// IDs of the cards that are not nodes.
const internetID = "internet"

func natID(net string) string { return "nat:" + net }
func boxID(net string) string { return "net:" + net }
func nodeID(id string) string { return "node:" + id }
func s3ID(id string) string   { return "s3:" + id }

// rows are the envs a card lists: the worst three, then "+N more".
func rows(n Node) []Env {
	if len(n.Envs) <= maxEnvs {
		return n.Envs
	}
	out := slices.Clone(n.Envs[:maxEnvs])
	return append(out, Env{Name: fmt.Sprintf("+%d more", len(n.Envs)-maxEnvs), Health: "more"})
}

func cardH(n Node) int {
	if r := len(rows(n)); r > 0 {
		return faceH + 14 + r*envRowH
	}
	return faceH
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

type rect struct{ x, y, w, h int }

// Build places the map and returns it as a graph canvas view.
func Build(m Map, base string, notes []graph.Note, create []graph.Create) graph.View {
	byID := map[string]Node{}
	for _, n := range m.Nodes {
		byID[n.ID] = n
	}
	place := func(id string, r rect) rect {
		if p, ok := m.Placed[id]; ok {
			r.x, r.y = p[0], p[1]
		}
		return r
	}
	var cards, boxes []graph.Node
	nx, bottom := 0, top
	for _, c := range m.Nets {
		// Default spots: the NAT first, then the nodes two to a row.
		var members []string
		var rs []rect
		y := top + headH
		add := func(id string, r rect, card graph.Node) {
			r = place(id, r)
			card.ID, card.X, card.Y, card.W, card.H = id, r.x, r.y, r.w, r.h
			cards = append(cards, card)
			members = append(members, id)
			rs = append(rs, r)
		}
		var ns []Node
		for _, id := range c.Nodes {
			if n, ok := byID[id]; ok {
				ns = append(ns, n)
			}
		}
		cols := min(len(ns), 2)
		w := max(minBoxW, 2*padX+cols*cardW+max(cols-1, 0)*gapX)
		if c.Router {
			add(natID(c.ID), rect{nx + padX, y, natW, natH}, graph.Node{Kind: "nat", Name: "NAT", Card: natCard()})
			y += natH + gapY
		}
		for i := 0; i < len(ns); i += 2 {
			rowH := 0
			for j := i; j < min(i+2, len(ns)); j++ {
				n := ns[j]
				h := cardH(n)
				add(nodeID(n.ID), rect{nx + padX + (j-i)*(cardW+gapX), y, cardW, h},
					graph.Node{Kind: "node", Name: n.Name, Card: nodeCard(n)})
				rowH = max(rowH, h)
			}
			y += rowH + gapY
		}
		if len(members) == 0 {
			continue
		}
		b := wrap(rs, minBoxW)
		name, sub := netTitle(c)
		boxes = append(boxes, graph.Node{
			ID: boxID(c.ID), Kind: "net", Name: name, X: b.x, Y: b.y, W: b.w, H: b.h,
			Members: members, Pad: [4]int{headH, padX, padB, padX}, MinW: minBoxW,
			Card: netCard(name, sub),
		})
		bottom = max(bottom, y-gapY+padB)
		nx += w + boxGap
	}

	// Edges. Public traffic of a node with its own address runs between it
	// and the internet; a node behind a router's runs through the NAT, which
	// forwards to the node the probe found.
	var edges []graph.Edge
	for _, l := range m.Links {
		label := l.Kind
		if l.Via != "" {
			label += " · via " + l.Via
		}
		if l.MS >= 0 {
			label += " · " + trimMS(l.MS) + " ms"
		}
		edges = append(edges, graph.Edge{Kind: "tunnel", From: nodeID(l.A), To: nodeID(l.B), Label: label})
	}
	public := func(from string, in bool, hosts, exits []string) {
		if in {
			edges = append(edges, graph.Edge{Kind: "ingress", From: internetID, To: from, Label: hostsLabel(hosts)})
		}
		if len(exits) > 0 {
			edges = append(edges, graph.Edge{Kind: "egress", From: from, To: internetID, Label: exitsLabel(exits)})
		}
	}
	for _, c := range m.Nets {
		var in bool
		var hosts, exits []string
		for _, id := range c.Nodes {
			n, ok := byID[id]
			if !ok {
				continue
			}
			if len(n.Public) > 0 || !c.Router {
				public(nodeID(n.ID), hasTag(n, "ingress") && len(n.Public) > 0, n.Domains, n.ExitEnvs)
				continue
			}
			in = in || hasTag(n, "ingress")
			if hasTag(n, "ingress") {
				hosts = append(hosts, n.Domains...)
			}
			exits = append(exits, n.ExitEnvs...)
		}
		if !c.Router {
			continue
		}
		public(natID(c.ID), in, hosts, exits)
		if _, ok := byID[c.Forward]; ok {
			kind := "forward"
			if c.Unsure {
				kind = "forward-unsure"
			}
			edges = append(edges, graph.Edge{Kind: kind, From: natID(c.ID), To: nodeID(c.Forward)})
		}
	}
	for _, b := range m.Backups {
		for _, id := range b.Nodes {
			if _, ok := byID[id]; ok {
				edges = append(edges, graph.Edge{Kind: "backup", From: nodeID(id), To: s3ID(b.ID)})
			}
		}
	}

	// The internet above the boxes, backup stores below them, when an edge
	// reaches them.
	if slices.ContainsFunc(edges, func(e graph.Edge) bool { return e.From == internetID || e.To == internetID }) {
		r := place(internetID, rect{max(nx-boxGap, sideW)/2 - sideW/2, top - 200, sideW, sideH})
		cards = append(cards, graph.Node{ID: internetID, Kind: "internet", Name: "Internet",
			X: r.x, Y: r.y, W: r.w, H: r.h, Card: sideCard("Internet", "globe")})
	}
	sx := 0
	for _, b := range m.Backups {
		if !slices.ContainsFunc(edges, func(e graph.Edge) bool { return e.To == s3ID(b.ID) }) {
			continue
		}
		r := place(s3ID(b.ID), rect{sx, bottom + 140, sideW, sideH})
		cards = append(cards, graph.Node{ID: s3ID(b.ID), Kind: "s3", Name: b.Name,
			X: r.x, Y: r.y, W: r.w, H: r.h, Card: sideCard("S3 · "+b.Name, "bucket")})
		sx += sideW + gapX
	}

	return graph.View{
		Base:   base,
		Notes:  notes,
		Create: create,
		Over:   legend(),
		Nodes:  append(boxes, cards...),
		Edges:  edges,
	}
}

// wrap is the box around rs, as the canvas computes it (graph-canvas wrap).
func wrap(rs []rect, minW int) rect {
	x0, y0, x1, y1 := rs[0].x, rs[0].y, rs[0].x+rs[0].w, rs[0].y+rs[0].h
	for _, r := range rs[1:] {
		x0, y0 = min(x0, r.x), min(y0, r.y)
		x1, y1 = max(x1, r.x+r.w), max(y1, r.y+r.h)
	}
	b := rect{x0 - padX, y0 - headH, 0, y1 + padB - (y0 - headH)}
	b.w = max(x1+padX-b.x, minW)
	return b
}

func hostsLabel(hosts []string) string {
	switch len(hosts) {
	case 0:
		return "ingress"
	case 1:
		return hosts[0]
	}
	return fmt.Sprintf("%s +%d", hosts[0], len(hosts)-1)
}

func exitsLabel(exits []string) string {
	if len(exits) == 1 {
		return exits[0] + " exits here"
	}
	return fmt.Sprintf("%s +%d exit here", exits[0], len(exits)-1)
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

// Movable reports whether a drop on id is kept: node, NAT, internet and
// backup cards. A box is where its cards are, so its own drop is not.
func Movable(id string) bool {
	return id == internetID || strings.HasPrefix(id, "node:") || strings.HasPrefix(id, "nat:") || strings.HasPrefix(id, "s3:")
}

// publicIP is the card's Public row: the first address, IPv4 first.
func publicIP(n Node) string {
	if len(n.Public) == 0 {
		return ""
	}
	a := n.Public[0]
	for _, p := range n.Public {
		if !strings.Contains(p, ":") {
			a = p
			break
		}
	}
	if len(n.Public) > 1 {
		a += fmt.Sprintf(" +%d", len(n.Public)-1)
	}
	return a
}

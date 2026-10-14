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
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

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
	Egress  []Egress
	// Placed is where admins dropped cards ("node:", "nat:", "s3:" ids and
	// "internet", "net:" boxes), in world pixels. A box stays where it was
	// dropped and grows to hold its cards.
	Placed map[string][2]int
}

type Node struct {
	ID, Name, Role, Status, Docker, Arch, Net string
	Tags, Public, Domains                     []string
	ExitOwn, ExitOthers                       []string          // env paths leaving here: this node's own, other nodes'
	DomainEnvs                                map[string]string // domain -> env path
	Envs                                      []Env
	NoForward                                 bool     // ingress behind a NAT the internet does not reach
	ReachedVia                                []string // nodes the others' traffic to it is routed through
	ViaDown                                   []string // the ReachedVia hops that are not online
	Behind, CertExpired                       bool     // the worker runs another build; its cert ran out
	CertDays                                  int      // days left under 14; 0 = none to say
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
	Via        string  // the node the pair is routed through, "" = direct
	Down       bool    // a direct tunnel whose end is not online
	Ends       [2]End  // [0] is A's side
}

// End is one side of a tunnel, as that node reports it. Rx and Tx are -1
// when the node did not report them; a zero Handshake is not reported.
type End struct {
	Dials     bool   // has an endpoint for the other side
	Endpoint  string // where it dials, "" = waits
	SeenFrom  string // where the other side's packets arrive from
	Handshake time.Time
	Rx, Tx    int64
}

// TunnelDrawer is a tunnel's drawer GET, the pair in a fixed order so one
// pair has one URL; TunnelPush the URL the browser shows with it open.
func TunnelDrawer(a, b string) string {
	a, b = pairOrder(a, b)
	return Base + "/tunnel?a=" + url.QueryEscape(a) + "&b=" + url.QueryEscape(b)
}

func TunnelPush(a, b string) string {
	a, b = pairOrder(a, b)
	return "?drawer=tunnel:" + url.QueryEscape(a) + ":" + url.QueryEscape(b)
}

func pairOrder(a, b string) (string, string) {
	if a > b {
		return b, a
	}
	return a, b
}

// Egress is the envs of node From whose traffic leaves at node To.
type Egress struct {
	From, To string
	Envs     []string
}

type Backup struct {
	ID, Name string
	Nodes    []string
}

// Geometry, in world pixels.
const (
	cardW      = 240
	faceH      = 164 // a node card without envs: head, tags, IP row and footer come to about 158, plus the border
	envRowH    = 22
	maxEnvs    = 3 // env rows on a card; more becomes "+N more"
	natW, natH = 104, 48
	sideW      = 168 // the internet and backup cards
	sideH      = 52
	padX       = 32 // a box's inside, left and right
	headH      = 64 // a box's header: its name and sub
	padB       = 32
	gapX       = 96
	boxGapY    = 140 // between stacked boxes: room for the tunnels across
	netLeft    = 0
	inetGap    = 40 // an internet card sits this far above its node's row
	minBoxW    = 320
	fanGap     = 28 // tunnels leaving one card side, apart
)

// IDs of the cards that are not nodes. Each node or NAT with public
// traffic has its own internet card, so lines never converge on one.
func internetID(from string) string { return "internet:" + from }
func natID(net string) string       { return "nat:" + net }
func boxID(net string) string       { return "net:" + net }
func nodeID(id string) string       { return "node:" + id }
func s3ID(id string) string         { return "s3:" + id }

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

// publicTraffic: the node has its own ingress or exit edge to the internet
// (not through a NAT), as Build draws them.
func publicTraffic(n Node, c Net) bool {
	if len(n.Public) == 0 && c.Router {
		return false
	}
	return hasTag(n, "ingress") && len(n.Public) > 0 || len(n.ExitOwn)+len(n.ExitOthers) > 0
}

// ownCard: the node has an internet card of its own. One with no public
// address leaves through its router's NAT, or its network's shared gateway
// card; a network that was not reported has no gateway to speak of.
func ownCard(n Node, c Net) bool { return len(n.Public) > 0 || c.ID == "unknown" }

// egressKind is the look of an internet edge: full when the node is an exit
// for others, thin when only its own apps leave there, "" when none do.
func egressKind(own, others []string) string {
	switch {
	case len(others) > 0:
		return "egress"
	case len(own) > 0:
		return "egress-own"
	}
	return ""
}

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
	at := map[string]rect{} // where each card landed, for its internet card
	top := map[string]int{} // the top of each card's row
	// Default spots: boxes stacked top to bottom, each one row: the NAT,
	// the node it forwards to, nodes with public traffic, the rest. Tunnels
	// between boxes then cross only the gap; each internet card sits above
	// its own card, outside the box.
	by := 0
	for _, c := range m.Nets {
		var members []string
		var rs []rect
		add := func(id string, r rect, card graph.Node) {
			lift := r.y - (by + headH) // how far below its row top the card sits
			r = place(id, r)
			card.ID, card.X, card.Y, card.W, card.H = id, r.x, r.y, r.w, r.h
			at[id] = r
			top[id] = r.y - lift // follows the card when it is dragged
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
		rank := func(n Node) int {
			switch {
			case c.Router && n.ID == c.Forward:
				return 0
			case publicTraffic(n, c):
				return 1
			}
			return 2
		}
		slices.SortStableFunc(ns, func(a, b Node) int { return rank(a) - rank(b) })
		rowY, rowH, x := by+headH, faceH, netLeft+padX
		for _, n := range ns {
			rowH = max(rowH, cardH(n))
		}
		if c.Router {
			add(natID(c.ID), rect{x, rowY + (faceH-natH)/2, natW, natH}, graph.Node{Kind: "nat", Name: "NAT", Card: natCard()})
			x += natW + gapX
		}
		for _, n := range ns {
			add(nodeID(n.ID), rect{x, rowY, cardW, cardH(n)}, graph.Node{Kind: "node", Name: n.Name, Card: nodeCard(n)})
			x += cardW + gapX
		}
		if len(members) == 0 {
			continue
		}
		// A network with one node is no grouping: its cards stand alone.
		if len(ns) >= 2 {
			dropped, moved := m.Placed[boxID(c.ID)] // where the box was dropped
			b := wrap(rs, minBoxW, dropped, moved)
			at[boxID(c.ID)], top[boxID(c.ID)] = b, b.y+headH // the shared internet card sits above it
			name, sub := netTitle(c)
			size := m.Placed["size:"+boxID(c.ID)] // a hand-set size: the box is at least this big
			boxes = append(boxes, graph.Node{
				ID: boxID(c.ID), Kind: "net", Name: name, X: b.x, Y: b.y, W: max(b.w, size[0]), H: max(b.h, size[1]),
				Members: members, Pad: [4]int{headH, padX, padB, padX}, MinW: minBoxW,
				UserW: size[0], UserH: size[1], Card: netCard(name, sub),
			})
		}
		by = rowY + rowH + padB + boxGapY
	}
	bottom := max(by-boxGapY, headH+faceH+padB)

	// Edges. Public traffic of a node with its own address runs between it
	// and the internet; a node behind a router's runs through the NAT, which
	// forwards to the node the probe found.
	var edges []graph.Edge
	for _, l := range m.Links {
		label := "" // colour tells internet from private; the label is the latency
		if l.MS >= 0 {
			label = trimMS(l.MS) + " ms"
		}
		kind := "tunnel"
		if l.Kind != "internet" {
			kind = "tunnel-lan"
		}
		if l.Down {
			kind, label = "tunnel-down", "down"
		}
		e := graph.Edge{Kind: kind, From: nodeID(l.A), To: nodeID(l.B), Label: label,
			Drawer: TunnelDrawer(l.A, l.B), Push: TunnelPush(l.A, l.B)}
		// An arrow runs from the side that dials to the one that waits; when
		// both dial, or nobody is known to, there is none.
		if !l.Down && l.Ends[0].Dials != l.Ends[1].Dials {
			e.Dir = true
			if l.Ends[1].Dials {
				e.From, e.To = e.To, e.From
			}
		}
		edges = append(edges, e)
	}
	// The internet card says what reaches it, so its edges carry no labels.
	// Full egress when the node is an exit for others, thin when only its
	// own apps leave there. Several nodes can feed one card (src).
	public := func(from string, src ...string) {
		info, ok := Internet(m, from)
		if !ok || !info.In && !info.leaves() {
			return
		}
		inet := internetID(from)
		r := at[from]
		r = place(inet, rect{r.x + (r.w-sideW)/2, top[from] - headH - inetGap - sideH, sideW, sideH})
		cards = append(cards, graph.Node{ID: inet, Kind: "internet", Name: "Internet",
			X: r.x, Y: r.y, W: r.w, H: r.h, Card: inetCard(from)})
		if info.In {
			edges = append(edges, graph.Edge{Kind: "ingress", From: inet, To: from})
		}
		egress := func(card string, own, others []string) {
			if k := egressKind(own, others); k != "" {
				edges = append(edges, graph.Edge{Kind: k, From: card, To: inet})
			}
		}
		if len(src) == 0 {
			egress(from, info.EgressOwn, info.EgressOthers)
		}
		for _, id := range src {
			egress(nodeID(id), byID[id].ExitOwn, byID[id].ExitOthers)
		}
	}
	for _, c := range m.Nets {
		var gateway []string // nodes whose traffic leaves through the network's gateway
		for _, id := range c.Nodes {
			n, ok := byID[id]
			switch {
			case !ok:
			case ownCard(n, c):
				public(nodeID(n.ID))
			case !c.Router:
				gateway = append(gateway, id)
			}
		}
		if len(gateway) > 0 {
			if _, ok := at[boxID(c.ID)]; ok {
				public(boxID(c.ID), gateway...)
			}
		}
		if !c.Router {
			continue
		}
		public(natID(c.ID))
		if _, ok := byID[c.Forward]; ok {
			kind := "forward"
			if c.Unsure {
				kind = "forward-unsure"
			}
			edges = append(edges, graph.Edge{Kind: kind, From: natID(c.ID), To: nodeID(c.Forward)})
		}
	}
	// A node whose traffic leaves through another node draws an arrow to it.
	for _, e := range m.Egress {
		_, from := byID[e.From]
		_, to := byID[e.To]
		if from && to {
			edges = append(edges, graph.Edge{Kind: "egress-via", From: nodeID(e.From), To: nodeID(e.To)})
		}
	}
	for _, b := range m.Backups {
		for _, id := range b.Nodes {
			if _, ok := byID[id]; ok {
				edges = append(edges, graph.Edge{Kind: "backup", From: nodeID(id), To: s3ID(b.ID)})
			}
		}
	}

	// Backup stores below the boxes, when an edge reaches them.
	sx := netLeft
	for _, b := range m.Backups {
		if !slices.ContainsFunc(edges, func(e graph.Edge) bool { return e.To == s3ID(b.ID) }) {
			continue
		}
		r := place(s3ID(b.ID), rect{sx, bottom + 120, sideW, sideH})
		cards = append(cards, graph.Node{ID: s3ID(b.ID), Kind: "s3", Name: b.Name,
			X: r.x, Y: r.y, W: r.w, H: r.h, Card: sideCard("S3 · "+b.Name, "bucket", nil)})
		sx += sideW + gapX
	}

	return graph.View{
		FanGap: fanGap,
		Base:   base,
		Notes:  notes,
		Create: create,
		Over:   legend(),
		Nodes:  append(boxes, cards...),
		Edges:  edges,
	}
}

// wrap is the box around rs, as the canvas computes it (graph-canvas wrap):
// a dropped box keeps its top left corner unless a card pokes past it.
func wrap(rs []rect, minW int, at [2]int, moved bool) rect {
	x0, y0, x1, y1 := rs[0].x, rs[0].y, rs[0].x+rs[0].w, rs[0].y+rs[0].h
	for _, r := range rs[1:] {
		x0, y0 = min(x0, r.x), min(y0, r.y)
		x1, y1 = max(x1, r.x+r.w), max(y1, r.y+r.h)
	}
	b := rect{x: x0 - padX, y: y0 - headH}
	if moved {
		b.x, b.y = min(b.x, at[0]), min(b.y, at[1])
	}
	b.w, b.h = max(x1+padX-b.x, minW), y1+padB-b.y
	return b
}

// Inet is what an internet card's drawer lists: the domains that come in
// through it (each with its env), the envs that leave through it (this
// node's own apps, and apps of other nodes it is the exit for), the public
// addresses, and the port forward of a NAT.
type Inet struct {
	Name         string // the node, "NAT" or the network
	In           bool   // the internet reaches ingress here
	Gateway      bool   // a network's shared card: its nodes leave through the gateway
	Public       []string
	Ingress      []InetRoute
	EgressOwn    []string // env paths
	EgressOthers []string // env paths of other nodes
	Behind       bool     // a NAT's card: the node behind it is Forward
	Forward      string
	Unsure       bool
	NoForward    bool
}

type InetRoute struct{ Host, Env string }

// leaves: some env's traffic leaves the internet here.
func (i Inet) leaves() bool { return len(i.EgressOwn)+len(i.EgressOthers) > 0 }

// InternetDrawer is an internet card's drawer GET (from is the card it hangs
// off, "node:<id>", "nat:<net>" or "net:<net>"; ids hold ":" and "/", so a
// query param); InternetPush the URL the browser shows with it open.
func InternetDrawer(from string) string { return Base + "/internet?from=" + url.QueryEscape(from) }

func InternetPush(from string) string { return "?drawer=" + url.QueryEscape("internet:"+from) }

// Internet is the internet card of from: a node with its own address, a
// router's NAT, or a network whose nodes have none and leave through its
// gateway. ok is false for anything else.
func Internet(m Map, from string) (Inet, bool) {
	byID := map[string]Node{}
	for _, n := range m.Nodes {
		byID[n.ID] = n
	}
	routes := func(n Node) []InetRoute {
		var rs []InetRoute
		for _, h := range n.Domains {
			rs = append(rs, InetRoute{Host: h, Env: n.DomainEnvs[h]})
		}
		return rs
	}
	if id, ok := strings.CutPrefix(from, "node:"); ok {
		n, ok := byID[id]
		if !ok {
			return Inet{}, false
		}
		for _, c := range m.Nets {
			if slices.Contains(c.Nodes, id) && !ownCard(n, c) {
				return Inet{}, false // behind the NAT card or the network's
			}
		}
		own := hasTag(n, "ingress") && len(n.Public) > 0
		info := Inet{Name: n.Name, In: own, Public: n.Public, EgressOwn: n.ExitOwn, EgressOthers: n.ExitOthers}
		if own {
			info.Ingress = routes(n)
		}
		return info, true
	}
	if net, ok := strings.CutPrefix(from, "nat:"); ok {
		for _, c := range m.Nets {
			if c.ID != net || !c.Router {
				continue
			}
			nat := Inet{Name: "NAT", Behind: true, Unsure: c.Unsure}
			if f, ok := byID[c.Forward]; ok {
				nat.Forward = f.Name
			}
			for _, id := range c.Nodes {
				n, ok := byID[id]
				if !ok || len(n.Public) > 0 {
					continue
				}
				if hasTag(n, "ingress") {
					nat.In = true
					nat.Ingress = append(nat.Ingress, routes(n)...)
				}
				nat.EgressOwn = append(nat.EgressOwn, n.ExitOwn...)
				nat.EgressOthers = append(nat.EgressOthers, n.ExitOthers...)
				nat.NoForward = nat.NoForward || n.NoForward
			}
			return nat, true
		}
		return Inet{}, false
	}
	net, ok := strings.CutPrefix(from, "net:")
	if !ok {
		return Inet{}, false
	}
	for _, c := range m.Nets {
		if c.ID != net || c.Router {
			continue
		}
		gw, has := Inet{Name: c.CIDR, Gateway: true}, false
		for _, id := range c.Nodes {
			if n, ok := byID[id]; ok && !ownCard(n, c) {
				has = true
				gw.EgressOwn = append(gw.EgressOwn, n.ExitOwn...)
				gw.EgressOthers = append(gw.EgressOthers, n.ExitOthers...)
			}
		}
		return gw, has
	}
	return Inet{}, false
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
	case "tunnel-down":
		return "Tunnel down, host alive", "text-rw-warn", "bg-rw-warn"
	case "cert-expired":
		return "Certificate expired: rejoin", "text-rw-danger", "bg-rw-danger"
	case "not-rejoined":
		return "Not rejoined", "text-rw-warn", "bg-rw-warn"
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
	return strings.HasPrefix(id, "internet:") || strings.HasPrefix(id, "node:") || strings.HasPrefix(id, "nat:") || strings.HasPrefix(id, "s3:")
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

// buildInfo is the card footer's right side: "docker 29.9.0 · amd64", the
// arch alone when docker is unknown.
func buildInfo(n Node) string {
	switch {
	case n.Docker != "" && n.Arch != "":
		return "docker " + n.Docker + " · " + n.Arch
	case n.Docker != "":
		return "docker " + n.Docker
	}
	return n.Arch
}

// viaList is the nodes a routed node is reached through, for its card.
func viaList(n Node) string {
	var out []string
	for _, h := range n.ReachedVia {
		if slices.Contains(n.ViaDown, h) {
			h += " (down)"
		}
		out = append(out, h)
	}
	return strings.Join(out, ", ")
}

// viaClass warns when a hop the node is reached through is down.
func viaClass(n Node) string {
	if len(n.ViaDown) > 0 {
		return "text-rw-warn"
	}
	return "text-rw-text"
}

// hasNote: the card has a line for behind or a cert note.
func hasNote(n Node) bool { return n.Behind || n.CertExpired || n.CertDays > 0 }

package canvas

import (
	"strconv"
	"strings"

	"github.com/a-h/templ"

	"github.com/FyrmForge/stackr/internal/service"
	ui "github.com/FyrmForge/stackr/internal/ui/graph"
	"github.com/FyrmForge/stackr/internal/ui/graph/cards"
)

// drawerPath is where a card's drawer lives, relative to the canvas it
// sits on (see drawer.go); "" = none here (the env kinds bring their own).
func drawerPath(n service.GraphNode) string {
	switch n.Kind {
	case "org", "stack", "env":
		return "/" + n.Slug + "/-/drawer"
	case "connector":
		return "/-/connectors/" + n.Slug
	case "vars":
		return "/-/vars"
	}
	return ""
}

// mapView is the service's view as the templ reads it: links, drawer
// routes and the stream URL added.
func mapView(gv service.GraphView, l level, sh service.GraphShow, focus string) ui.View {
	v := ui.View{
		Base: l.base, Toggles: l.scope.Kind == service.CanvasEnv, Focus: focus, Divider: gv.Divider,
		Show: ui.Show{System: sh.System, Refs: sh.Refs, Startup: sh.Startup, Traffic: sh.Traffic},
	}
	v.Query = v.Show.Query()
	for _, n := range gv.Nodes {
		u := ui.Node{ID: n.ID, Kind: n.Kind, Name: n.Name, Slug: n.Slug, Detail: n.Detail, Status: n.Status, X: n.X, Y: n.Y, W: n.W, H: n.H,
			System: n.System, Static: n.Static, Color: n.Color, Deck: n.Deck}
		switch n.Kind {
		case "org":
			u.Href = "/" + n.Slug
		case "stack", "env":
			u.Href = l.base + "/" + n.Slug
		}
		if p := drawerPath(n); p != "" {
			u.Drawer, u.Push = l.base+p, "?drawer="+n.ID
		}
		for _, s := range n.Subs {
			u.Subs = append(u.Subs, ui.Sub{ID: s.ID, Kind: s.Kind, Name: s.Name, Status: s.Status})
		}
		if l.scope.Kind == service.CanvasEnv {
			envCard(&u, n, l.base)
		}
		v.Nodes = append(v.Nodes, u)
	}
	for _, e := range gv.Edges {
		v.Edges = append(v.Edges, ui.Edge{Kind: e.Kind, From: e.From, To: e.To})
	}
	for _, a := range gv.Notes {
		v.Notes = append(v.Notes, ui.Note{ID: a.ID, Kind: a.Kind, Text: a.Text, X: a.X, Y: a.Y, W: a.W, H: a.H})
	}
	for _, r := range gv.Compare {
		v.Compare = append(v.Compare, ui.Rung{Name: r.Name, Color: r.Color, Href: l.base + "/" + r.Slug, Release: r.Release, Behind: r.Behind})
	}
	if l.scope.Kind == service.CanvasEnv && sh.Traffic {
		v.Lanes = []cards.Lane{} // non-nil: draw the lanes layer; build fills it
	}
	sep := "?"
	if v.Query != "" {
		sep = "&"
	}
	v.Events = v.Route("events") + sep + "n=" + Sig(v)
	return v
}

// envDrawer is where an env card's (or sub-tile's) drawer lives and the tab
// it opens on; "" = none (ghost refs, the internet, an undeclared volume).
func envDrawer(base, kind, id, slug string) (url, tab string) {
	switch kind {
	case "service", "image", "cron", "function":
		return base + "/-/tiles/" + slug, "status"
	case "managed":
		return base + "/-/instances/" + slug, "slices"
	case "slice":
		return base + "/-/slices/" + id, "bindings"
	case "volume":
		if strings.HasPrefix(id, "volume:") { // mounted, never declared
			return "", ""
		}
		return base + "/-/volumes/" + id, "backups"
	case "proxy":
		return base + "/-/proxy", "routes"
	case "vars":
		return base + "/-/vars", "editor"
	}
	return "", ""
}

// envCard gives an env node session C's card (internal/ui/graph/cards):
// body, chips, sub-tiles and the live footer the stream re-sends.
func envCard(u *ui.Node, n service.GraphNode, base string) {
	cv := cards.CardView{ID: n.ID, Kind: n.Kind, Name: n.Name, Detail: n.Detail, Host: n.Host}
	if url, tab := envDrawer(base, n.Kind, n.ID, n.Slug); url != "" {
		cv.Drawer, cv.Tab = url+"?tab="+tab, tab
	}
	if len(n.Volumes) > 0 {
		cv.Volumes = n.Volumes[0]
		if len(n.Volumes) > 1 {
			cv.Volumes += " +" + strconv.Itoa(len(n.Volumes)-1)
		}
	}
	f := cards.FooterView{Status: n.Status, Waiting: n.Waiting, Up: n.Running, Want: n.Replicas, Count: n.Params + n.Secrets}
	if n.Status == "none" {
		f.Status = ""
	}
	if r := n.LastRun; r != nil {
		f.LastRun = r.Status + " · " + r.CreatedAt.Local().Format("Jan 2 15:04")
	} else if n.Kind == "cron" || n.Kind == "function" {
		f.LastRun = "never run"
	}
	if len(n.Domains) > 0 {
		f.Domain, f.More = n.Domains[0], len(n.Domains)-1
	}
	cv.Footer = f
	u.Subs = u.Subs[:0]
	for _, s := range n.Subs {
		sv := cards.SubView{ID: s.ID, Kind: s.Kind, Label: s.Name}
		var url, tab string
		switch s.Kind {
		case "replica": // its tile's drawer
			url, tab = envDrawer(base, n.Kind, n.ID, n.Slug)
			sv.Label += " · " + s.Status
		case "managed": // the hosting instance under a slice
			sv.Kind = "instance"
			url, tab = envDrawer(base, "managed", s.ID, s.Slug)
		default:
			url, tab = envDrawer(base, s.Kind, s.ID, "")
		}
		if url != "" {
			sv.Drawer, sv.Tab = url+"?tab="+tab, tab
		}
		cv.Subs = append(cv.Subs, sv)
		u.Subs = append(u.Subs, ui.Sub{ID: s.ID, Kind: s.Kind, Name: s.Name, Status: s.Status, Drawer: sv.Drawer})
	}
	u.Drawer, u.Push = cv.Drawer, "?drawer="+n.ID
	u.Card = templ.Join(cards.Card(cv), cards.Subs(cv))
	u.Footer = cards.Footer(cv.ID, cv.Footer)
}

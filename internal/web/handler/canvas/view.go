package canvas

import (
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

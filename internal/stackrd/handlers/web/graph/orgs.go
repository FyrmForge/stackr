package graph

import "sort"

// OrgSummary is one organization as the root canvas sees it.
type OrgSummary struct {
	ID     string
	Name   string
	Slug   string
	Href   string
	Stacks int
	// Nav navigates to Href instead of opening it as a drawer. The root canvas
	// always navigates; the org's own card on its own canvas sets this to reach
	// the settings page.
	Nav     bool
	Members int
	// SetupPending: the wizard is still open, so the org answers nothing but
	// its own setup. Clicking the card is a redirect (owner) or a holding page
	// (everyone else), and a card that says "0 stacks" gives no hint of that.
	SetupPending bool
}

// BuildOrgs draws the root canvas: one card per organization the viewer belongs
// to. Deliberately edgeless, organizations are isolated from each other by
// definition, so there is nothing to connect. This level is a launcher, and the
// canvas is here for consistency with every level below (and because the
// drill-down animation only exists on canvases).
func BuildOrgs(orgs []OrgSummary, positions map[string][2]float64) Graph {
	var g Graph
	for _, o := range orgs {
		n := Node{
			ID:     OrgNodeID(o.ID),
			Kind:   KindOrg,
			Name:   o.Name,
			Detail: orgDetail(o.Stacks, o.Members, o.SetupPending),
			Href:   o.Href,
			Nav:    o.Href != "",
			Deck:   deckLayers(o.Stacks),
		}
		applyPosition(&n, positions)
		g.Nodes = append(g.Nodes, n)
	}
	layoutOrgs(g.Nodes, positions)
	return g
}

// OrgNodeID is the root canvas's stable card id. Keyed on the org's UUID rather
// than its slug: a rename changes the slug, and the card should keep its place.
func OrgNodeID(id string) string { return "org:" + id }

// orgDetail is the card's whole subtitle: what the org holds and who is in it.
// The org card has no status footer (an org is not running or idle), so this one
// line carries everything the card says beyond its name.
func orgDetail(stacks, members int, setupPending bool) string {
	if setupPending {
		return "Setup unfinished"
	}
	s := countOf(stacks, "stack")
	if members > 0 {
		return s + " · " + countOf(members, "member")
	}
	return s // membership unreadable or empty: say nothing rather than "0 members"
}

// layoutOrgs wraps the cards into a grid. With no edges there is no flow to
// read, so the only job is keeping a long membership list from becoming one
// endless column.
const orgsPerColumn = 4

func layoutOrgs(nodes []Node, positions map[string][2]float64) {
	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.ID)
	}
	sort.Strings(ids) // the order placeColumns fills columns in
	col := make(map[string]float64, len(ids))
	for i, id := range ids {
		col[id] = float64(i/orgsPerColumn) * colGap
	}
	placeColumns(nodes, positions, func(n Node) float64 { return col[n.ID] })
}

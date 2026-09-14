package graph

// VarCards is one scope's pair of variable cards, plain values and secrets,
// plus the cards that reference them. Two cards rather than one because "who
// can read this" is the only question the canvas can usefully answer about a
// variable, and it is the one that splits them.
//
// Only names and counts ever reach the graph: Node is serialized straight to
// the browser by the canvas status endpoints, so a value must never live here.
type VarCards struct {
	Scope string // "stack" | "org", the node id suffix
	Label string // card title prefix, e.g. "Organization"
	Href  string // the variables editor; cards open Href+"/panel" in the drawer

	Plain, Secret int // how many variables of each class the scope holds

	// Node ids of the cards that reference at least one variable of each class.
	PlainBy, SecretBy []string
}

// VarsNodeID / SecretsNodeID are the stable ids of a scope's two cards.
// "vars:org" predates the split, so the plain card keeps that id and saved
// drag positions survive.
func VarsNodeID(scope string) string    { return "vars:" + scope }
func SecretsNodeID(scope string) string { return "secrets:" + scope }

// nodes returns the cards to draw (a class with no variables gets none).
func (v VarCards) nodes(positions map[string][2]float64) []Node {
	var out []Node
	add := func(id, name, noun, class string, count int) {
		if count == 0 {
			return
		}
		// No Nav: like service tiles, these open the right drawer in place
		// (canvas panelURL appends /panel to Href). The class query is how the
		// drawer knows which of the two cards was clicked.
		n := Node{
			ID:     id,
			Kind:   KindVars,
			Name:   name,
			Detail: countOf(count, noun),
			Href:   v.Href + "?class=" + class,
		}
		applyPosition(&n, positions)
		out = append(out, n)
	}
	add(VarsNodeID(v.Scope), v.Label+" variables", "variable", "plain", v.Plain)
	add(SecretsNodeID(v.Scope), v.Label+" secrets", "secret", "secret", v.Secret)
	return out
}

// AddVarCards drops a scope's variable cards onto an already-built graph,
// the env canvas builds its tile graph first and layers these on, where
// BuildStack/BuildOrg take the cards as an argument instead.
func (g *Graph) AddVarCards(v VarCards, positions map[string][2]float64) {
	g.Nodes = append(g.Nodes, v.nodes(positions)...)
	have := map[string]bool{}
	for _, n := range g.Nodes {
		have[n.ID] = true
	}
	g.Edges = append(g.Edges, v.edges(have)...)
}

// edges wires each card to the cards that actually read from it. have filters
// out consumers that aren't on this canvas.
func (v VarCards) edges(have map[string]bool) []Edge {
	var out []Edge
	add := func(id string, consumers []string) {
		if !have[id] {
			return
		}
		for _, c := range consumers {
			if have[c] {
				out = append(out, Edge{From: id, To: c, Kind: "shared"})
			}
		}
	}
	add(VarsNodeID(v.Scope), v.PlainBy)
	add(SecretsNodeID(v.Scope), v.SecretBy)
	return out
}

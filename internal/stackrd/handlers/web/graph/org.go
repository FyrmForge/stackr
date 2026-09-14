package graph

import (
	"strconv"
)

// StackSummary is one stack as the org canvas sees it.
type StackSummary struct {
	ID     string
	Slug   string
	Name   string
	Href   string
	Envs   int
	Status string // worst status across the stack's tiles

	// How many proxied hostnames the stack's tiles serve. > 0 puts it behind
	// the Traefik card; the hostnames themselves live one level down.
	DomainCount int

	// Live port-forward relays into this stack's tiles.
	ForwardCount int

	// Org-shared instances this stack's tiles consume.
	InstanceIDs []string

	// ConfigConnectorID is the connector hosting this stack's config-as-code
	// repo, "" when the stack is not config-managed. SourceConnectorIDs are
	// the connectors its tiles clone service source through. The two say
	// different things, so they are drawn as different lines.
	ConfigConnectorID  string
	SourceConnectorIDs []string
}

// OrgInstance is a tile shared with the whole org (a database everyone
// provisions from, a singleton service).
type OrgInstance struct {
	ID     string
	Name   string
	Detail string
	Engine string // storage engine, for the card's icon
	Status string
	Href   string

	// How this instance is reachable from outside: proxied hostnames, a
	// published host port, or neither.
	Domains      []string
	ExternalPort int
}

// OrgConnector is an external integration owned by the org (a GitHub app
// install, an S3 destination).
type OrgConnector struct {
	ID       string
	Name     string
	Detail   string
	Provider string // "github", picks the card's icon
	Href     string
}

// BuildOrg draws one organization: its stacks, the instances they share, the
// connectors feeding them, and the org's variable and secret cards wired to the
// stacks that read them.
//
// The org itself is NOT a card here. It used to be one, linking to the settings
// page, but a card standing for the canvas you are already looking at is a
// tautology, and it read as an eighth stack. Org administration is a Settings
// link in the page header instead (handler/org/graph.templ).
// Deliberately edgeless: the org owns every card here, so one line per stack
// would say nothing the canvas doesn't already say by containing them.
func BuildOrg(stacks []StackSummary, instances []OrgInstance, connectors []OrgConnector, vars VarCards, positions map[string][2]float64) Graph {
	var g Graph
	for _, s := range stacks {
		n := Node{
			ID:          StackNodeID(s.Slug),
			Kind:        KindStack,
			Name:        s.Name,
			Detail:      envCount(s.Envs),
			Status:      s.Status,
			Href:        s.Href,
			Nav:         s.Href != "",
			DomainCount: s.DomainCount,
			Deck:        deckLayers(s.Envs),

			ForwardCount: s.ForwardCount,
		}
		applyPosition(&n, positions)
		g.Nodes = append(g.Nodes, n)
	}
	for _, i := range instances {
		n := Node{
			ID:           DBNodeID(i.ID),
			Kind:         KindManaged,
			Name:         i.Name,
			Detail:       i.Detail,
			Engine:       i.Engine,
			Status:       i.Status,
			Href:         i.Href,
			Domains:      i.Domains,
			ExternalPort: i.ExternalPort,
		}
		applyPosition(&n, positions)
		g.Nodes = append(g.Nodes, n)
	}
	for _, c := range connectors {
		n := Node{
			ID:     ConnectorNodeID(c.ID),
			Kind:   KindConnector,
			Name:   c.Name,
			Detail: c.Detail,
			Engine: c.Provider,
			Href:   c.Href,
			Nav:    c.Href != "",
		}
		applyPosition(&n, positions)
		g.Nodes = append(g.Nodes, n)
	}
	g.Nodes = append(g.Nodes, vars.nodes(positions)...)

	// The same Traefik card as every level below, in front of the stacks that
	// serve something.
	hasIngress := false
	for _, s := range stacks {
		if s.DomainCount > 0 {
			hasIngress = true
			g.Edges = append(g.Edges, Edge{From: ProxyNodeID, To: StackNodeID(s.Slug), Kind: "ingress"})
		}
	}
	// Org-shared instances carry their own exposure, a bucket everyone uses,
	// served over a hostname, belongs on this level too.
	for _, i := range instances {
		if len(i.Domains) > 0 {
			hasIngress = true
			g.Edges = append(g.Edges, Edge{From: ProxyNodeID, To: DBNodeID(i.ID), Kind: "ingress"})
		}
	}
	if hasIngress {
		g.Nodes = append(g.Nodes, proxyNode(positions))
	}

	hasPorts := false
	for _, i := range instances {
		if i.ExternalPort > 0 {
			hasPorts = true
			g.Edges = append(g.Edges, Edge{From: HostNodeID, To: DBNodeID(i.ID), Kind: "port"})
		}
	}
	if hasPorts {
		n := Node{ID: HostNodeID, Kind: KindHost, Name: "Host network", Detail: "published ports", Status: "running"}
		applyPosition(&n, positions)
		g.Nodes = append(g.Nodes, n)
	}

	have := map[string]bool{}
	for _, n := range g.Nodes {
		have[n.ID] = true
	}
	for _, s := range stacks {
		for _, id := range s.InstanceIDs {
			if to := DBNodeID(id); have[to] {
				g.Edges = append(g.Edges, Edge{From: StackNodeID(s.Slug), To: to, Kind: "shared"})
			}
		}
	}
	g.Edges = append(g.Edges, vars.edges(have)...)

	// Connectors feed the stacks they serve. A stack whose infrastructure is
	// defined in a config repo gets the stronger "config" line; one that merely
	// builds services from repos on the same account gets the weaker "source"
	// line. Never both between one pair, being config-managed by a connector
	// says everything the source relationship would.
	for _, s := range stacks {
		to := StackNodeID(s.Slug)
		drawn := map[string]bool{}
		if id := s.ConfigConnectorID; id != "" && have[ConnectorNodeID(id)] {
			g.Edges = append(g.Edges, Edge{From: ConnectorNodeID(id), To: to, Kind: "config"})
			drawn[id] = true
		}
		for _, id := range s.SourceConnectorIDs {
			if drawn[id] || !have[ConnectorNodeID(id)] {
				continue
			}
			g.Edges = append(g.Edges, Edge{From: ConnectorNodeID(id), To: to, Kind: "source"})
			drawn[id] = true
		}
	}

	// No layout here: the caller runs g.Arrange with the user's style.
	return g
}

// StackNodeID / ConnectorNodeID are the org canvas's stable ids.
func StackNodeID(slug string) string   { return "stack:" + slug }
func ConnectorNodeID(id string) string { return "connector:" + id }

func envCount(n int) string {
	if n == 1 {
		return "1 environment"
	}
	return strconv.Itoa(n) + " environments"
}

// countOf pluralises a card's subtitle: "1 secret", "3 variables".
func countOf(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

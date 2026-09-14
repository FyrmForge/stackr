package graph

import (
	"strconv"
)

// EnvSummary is one environment as the stack canvas sees it. The handler does
// the rollups (it owns the tiles); this package only draws them.
type EnvSummary struct {
	ID     string
	Slug   string
	Name   string
	Href   string
	Tiles  int
	Status string // worst tile status in the env: error > building > running
	Staged bool   // pending staged changes or a config plan waiting
	Color  string // envcolor CSS value

	// Proxied hostnames of every tile in the env. Non-empty puts the env
	// behind the Traefik card, same as an app one level down.
	Domains []string

	// Stack-scoped instances and out-of-env shared instances the env's tiles
	// provision from. Each becomes an edge to the matching card.
	InstanceIDs []string
	RefIDs      []string

	// Live port-forward relays into this env's tiles (count chip on the card,
	// same rollup idea as DomainCount one level up).
	ForwardCount int
}

// StackInstance is a stack-scoped managed instance: this canvas's resident in
// the tree, the same way an org-scoped one is a card on the org canvas. Slices
// and env-scoped instances live one level down, inside the env cards.
type StackInstance struct {
	ID     string
	Name   string
	Detail string // "postgres · stack-scoped"
	Engine string
	Status string
	Href   string

	// How this instance is reachable from outside: proxied hostnames, a
	// published host port, or neither.
	Domains      []string
	ExternalPort int
}

// BuildStack draws one stack: its environments, the stack-scoped instances
// they share, ghost cards for shared instances living elsewhere, and the
// stack's variable and secret cards wired to the environments that read them.
// Traffic is not built here: the status handler rolls container flows up onto
// these cards with RollupTraffic.
func BuildStack(envs []EnvSummary, instances []StackInstance, refs []Reference, vars VarCards, positions map[string][2]float64) Graph {
	var g Graph
	for _, e := range envs {
		n := Node{
			ID:      EnvNodeID(e.Slug),
			Kind:    KindEnv,
			Name:    e.Name,
			Detail:  tileCount(e.Tiles),
			Status:  e.Status,
			Href:    e.Href,
			Nav:     e.Href != "",
			Domains: e.Domains,
			Deck:    deckLayers(e.Tiles),
			Color:   e.Color,

			ForwardCount: e.ForwardCount,
		}
		if e.Staged {
			n.Staged = "pending"
		}
		applyPosition(&n, positions)
		g.Nodes = append(g.Nodes, n)
	}
	for _, in := range instances {
		n := Node{
			ID:           DBNodeID(in.ID),
			Kind:         KindManaged,
			Name:         in.Name,
			Detail:       in.Detail,
			Engine:       in.Engine,
			Status:       in.Status,
			Href:         in.Href,
			Domains:      in.Domains,
			ExternalPort: in.ExternalPort,
		}
		applyPosition(&n, positions)
		g.Nodes = append(g.Nodes, n)
	}
	for _, r := range refs {
		n := Node{
			ID:           RefNodeID(r.InstanceID),
			Kind:         KindRef,
			Name:         r.Name,
			Detail:       r.Detail,
			Engine:       r.Engine,
			Href:         r.Href,
			Domains:      r.Domains,
			ExternalPort: r.ExternalPort,
		}
		applyPosition(&n, positions)
		g.Nodes = append(g.Nodes, n)
	}

	g.Nodes = append(g.Nodes, vars.nodes(positions)...)

	// One Traefik card for the whole stack, in front of every env that has a
	// proxied host, the same synthetic node as the env canvas, one level up.
	hasIngress := false
	for _, e := range envs {
		if len(e.Domains) > 0 {
			hasIngress = true
			g.Edges = append(g.Edges, Edge{From: ProxyNodeID, To: EnvNodeID(e.Slug), Kind: "ingress"})
		}
	}
	// Managed instances can be proxied and published too, an s3 bucket behind
	// a hostname, a postgres on a host port. Those cards exist on this canvas,
	// so the same two system cards serve them.
	for _, in := range instances {
		if len(in.Domains) > 0 {
			hasIngress = true
			g.Edges = append(g.Edges, Edge{From: ProxyNodeID, To: DBNodeID(in.ID), Kind: "ingress"})
		}
	}
	for _, r := range refs {
		if len(r.Domains) > 0 {
			hasIngress = true
			g.Edges = append(g.Edges, Edge{From: ProxyNodeID, To: RefNodeID(r.InstanceID), Kind: "ingress"})
		}
	}
	if hasIngress {
		g.Nodes = append(g.Nodes, proxyNode(positions))
	}

	hasPorts := false
	for _, in := range instances {
		if in.ExternalPort > 0 {
			hasPorts = true
			g.Edges = append(g.Edges, Edge{From: HostNodeID, To: DBNodeID(in.ID), Kind: "port"})
		}
	}
	for _, r := range refs {
		if r.ExternalPort > 0 {
			hasPorts = true
			g.Edges = append(g.Edges, Edge{From: HostNodeID, To: RefNodeID(r.InstanceID), Kind: "port"})
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
	for _, e := range envs {
		from := EnvNodeID(e.Slug)
		for _, id := range e.InstanceIDs {
			if to := DBNodeID(id); have[to] {
				g.Edges = append(g.Edges, Edge{From: from, To: to, Kind: "shared"})
			}
		}
		for _, id := range e.RefIDs {
			if to := RefNodeID(id); have[to] {
				g.Edges = append(g.Edges, Edge{From: from, To: to, Kind: "shared"})
			}
		}
	}
	g.Edges = append(g.Edges, vars.edges(have)...)
	// No layout here: the caller runs g.Arrange with the user's style.
	return g
}

// EnvNodeID is the stable node id for an environment card. Slug-based like the
// tile ids, so a rebuilt environment keeps its place on the canvas.
func EnvNodeID(slug string) string { return "env:" + slug }

// deckLayers is how many card layers sit behind an env/stack card: one per
// thing inside, two at most.
func deckLayers(n int) int {
	if n > 2 {
		return 2
	}
	if n < 0 {
		return 0
	}
	return n
}

func tileCount(n int) string {
	if n == 1 {
		return "1 tile"
	}
	return strconv.Itoa(n) + " tiles"
}

package graph

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildOrgLinksStacksToSharedInstances(t *testing.T) {
	stacks := []StackSummary{
		{Slug: "shop", Name: "Shop", Href: "/acme/shop", Envs: 2, Status: "running", InstanceIDs: []string{"pg-1"}},
		{Slug: "blog", Name: "Blog", Href: "/acme/blog", Envs: 1, InstanceIDs: []string{"pg-1", "gone"}},
	}
	inst := []OrgInstance{{ID: "pg-1", Name: "shared-pg", Detail: "postgres · shared"}}
	conns := []OrgConnector{{ID: "c1", Name: "github", Detail: "github"}}

	vars := VarCards{Scope: "org", Label: "Organization", Href: "/orgs/acme/settings/variables", Plain: 3, Secret: 1,
		PlainBy: []string{StackNodeID("shop")}, SecretBy: []string{StackNodeID("shop"), StackNodeID("gone")}}

	g := BuildOrg(stacks, inst, conns, vars, nil)
	g.Arrange(ArrangeClusters, nil)

	require.Len(t, g.Nodes, 6, "%+v", g.Nodes) // 2 stacks + 1 instance + 1 connector + vars + secrets
	// both stacks -> pg-1, vars -> shop, secrets -> shop; "gone" is not a card
	require.Len(t, g.Edges, 4, "%+v", g.Edges)
	byID := map[string]Node{}
	for _, n := range g.Nodes {
		byID[n.ID] = n
	}
	shop := byID[StackNodeID("shop")]
	require.Equal(t, KindStack, shop.Kind, "stack card wrong: %+v", shop)
	require.Equal(t, "2 environments", shop.Detail, "stack card wrong: %+v", shop)
	require.Equal(t, "/acme/shop", shop.Href, "stack card wrong: %+v", shop)
	v := byID[VarsNodeID("org")]
	require.Equal(t, "3 variables", v.Detail, "vars card wrong: %+v", v)
	require.Equal(t, "/orgs/acme/settings/variables?class=plain", v.Href, "vars card wrong: %+v", v)
	s := byID[SecretsNodeID("org")]
	require.Equal(t, "1 secret", s.Detail, "secrets card wrong: %+v", s)
	require.Equal(t, KindVars, s.Kind, "secrets card wrong: %+v", s)
	// Connectors/vars feed the stacks, shared instances sit beyond them.
	require.Less(t, byID[ConnectorNodeID("c1")].X, shop.X, "columns out of order: %+v", byID)
	require.Less(t, shop.X, byID[DBNodeID("pg-1")].X, "columns out of order: %+v", byID)
	// Two cards in the same column must not land on the same row.
	require.NotEqual(t, byID[ConnectorNodeID("c1")].Y, byID[VarsNodeID("org")].Y, "connector and vars cards overlap at y=%v", byID[VarsNodeID("org")].Y)
}

// Icons come from Engine, so the org canvas must carry it for both the managed
// instances and the connectors, their subtitles are display copy.
func TestBuildOrgCardsCarryTheirTech(t *testing.T) {
	g := BuildOrg(nil,
		[]OrgInstance{{ID: "pg-1", Name: "shared-pg", Detail: "postgres · managed", Engine: "postgres"}},
		[]OrgConnector{{ID: "c1", Name: "GitHub · acme", Detail: "github", Provider: "github"}},
		VarCards{}, nil)
	want := map[string]string{DBNodeID("pg-1"): "postgres", ConnectorNodeID("c1"): "github"}
	for _, n := range g.Nodes {
		if e, ok := want[n.ID]; ok {
			assert.Equal(t, e, n.Engine, "%s engine", n.ID)
			delete(want, n.ID)
		}
	}
	for id := range want {
		assert.Fail(t, "no card for "+id)
	}
}

// A connector feeding a stack draws one line, and being config-managed by it
// outranks merely building source from it, otherwise a config-as-code stack
// gets two lines to the same connector.
func TestBuildOrgDrawsConfigAndSourceConnectorEdges(t *testing.T) {
	stacks := []StackSummary{
		// config-managed by c1, and its tiles also clone from c1 and from c2
		{Slug: "shop", Name: "Shop", ConfigConnectorID: "c1", SourceConnectorIDs: []string{"c1", "c2"}},
		// only builds source from c1
		{Slug: "blog", Name: "Blog", SourceConnectorIDs: []string{"c1"}},
		// references a connector that is not a card on this canvas
		{Slug: "old", Name: "Old", SourceConnectorIDs: []string{"gone"}},
	}
	conns := []OrgConnector{
		{ID: "c1", Name: "GitHub · acme", Provider: "github"},
		{ID: "c2", Name: "GitHub · other", Provider: "github"},
	}
	g := BuildOrg(stacks, nil, conns, VarCards{}, nil)

	got := map[string]string{} // "<from>-><to>" -> kind
	for _, e := range g.Edges {
		got[e.From+"->"+e.To] = e.Kind
	}
	want := map[string]string{
		ConnectorNodeID("c1") + "->" + StackNodeID("shop"): "config",
		ConnectorNodeID("c2") + "->" + StackNodeID("shop"): "source",
		ConnectorNodeID("c1") + "->" + StackNodeID("blog"): "source",
	}
	for k, v := range want {
		assert.Equal(t, v, got[k], "edge %s", k)
	}
	assert.Len(t, g.Edges, len(want), "%+v", g.Edges)
}

func TestBuildOrgWithoutVariablesDrawsNoVarsCard(t *testing.T) {
	g := BuildOrg([]StackSummary{{Slug: "shop", Name: "Shop"}}, nil, nil, VarCards{Scope: "org", Href: "/orgs/acme/settings/variables"}, nil)
	for _, n := range g.Nodes {
		require.NotEqual(t, KindVars, n.Kind, "unexpected vars card: %+v", n)
	}
}

func TestBuildOrgPutsProxyInFrontOfServingStacks(t *testing.T) {
	stacks := []StackSummary{
		{Slug: "shop", Name: "Shop", DomainCount: 3},
		{Slug: "internal", Name: "Internal"}, // nothing published
	}
	g := BuildOrg(stacks, nil, nil, VarCards{Scope: "org"}, nil)
	g.Arrange(ArrangeClusters, nil)

	var proxy, shop, internal Node
	for _, n := range g.Nodes {
		switch n.ID {
		case ProxyNodeID:
			proxy = n
		case StackNodeID("shop"):
			shop = n
		case StackNodeID("internal"):
			internal = n
		}
	}
	require.NotEmpty(t, proxy.ID, "proxy missing or not left of the stacks: %+v", proxy)
	require.Less(t, proxy.X, shop.X, "proxy missing or not left of the stacks: %+v", proxy)
	// graph.js clamps non-system cards right of the system zone; anything
	// auto-placed there would jump on its first drag.
	assertSystemZoneIsProxyOnly(t, g)
	require.Equal(t, 3, shop.DomainCount, "domain counts wrong: %d / %d", shop.DomainCount, internal.DomainCount)
	require.Equal(t, 0, internal.DomainCount, "domain counts wrong: %d / %d", shop.DomainCount, internal.DomainCount)
	require.Len(t, g.Edges, 1, "want one ingress edge to shop, got %+v", g.Edges)
	require.Equal(t, "ingress", g.Edges[0].Kind, "want one ingress edge to shop, got %+v", g.Edges)
	require.Equal(t, StackNodeID("shop"), g.Edges[0].To, "want one ingress edge to shop, got %+v", g.Edges)
}

func TestBuildOrgShowsSharedInstanceExposure(t *testing.T) {
	stacks := []StackSummary{{Slug: "shop", Name: "Shop", InstanceIDs: []string{"s3-1"}}}
	inst := []OrgInstance{{ID: "s3-1", Name: "assets", Domains: []string{"cdn.example.com"}, ExternalPort: 9000}}

	g := BuildOrg(stacks, inst, nil, VarCards{Scope: "org"}, nil)

	var ingress, port bool
	for _, e := range g.Edges {
		if e.To != DBNodeID("s3-1") {
			continue
		}
		switch {
		case e.From == ProxyNodeID && e.Kind == "ingress":
			ingress = true
		case e.From == HostNodeID && e.Kind == "port":
			port = true
		}
	}
	require.True(t, ingress, "want ingress+port edges to the shared instance, got %+v", g.Edges)
	require.True(t, port, "want ingress+port edges to the shared instance, got %+v", g.Edges)
	assertSystemZoneIsProxyOnly(t, g)
}

// The org canvas never draws the org itself. A card standing for the canvas
// you are already looking at says nothing, and it read as one more stack,
// org administration is the header's Settings link (handler/org/graph.templ).
func TestBuildOrgDrawsNoCardForItself(t *testing.T) {
	g := BuildOrg([]StackSummary{{Slug: "shop", Name: "Shop"}}, nil, nil, VarCards{}, nil)

	for _, n := range g.Nodes {
		require.NotEqual(t, KindOrg, n.Kind, "org canvas drew a card for itself: %+v", n)
	}
	require.Empty(t, g.Edges, "the org owns every card here; edges say nothing")
}

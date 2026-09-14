package graph

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuildStackLinksEnvsToWhatTheyUse(t *testing.T) {
	envs := []EnvSummary{
		{Slug: "production", Name: "Production", Href: "/o/s/production", Tiles: 3, Status: "running",
			InstanceIDs: []string{"pg-1"}, RefIDs: []string{"inst-1"}},
		{Slug: "staging", Name: "Staging", Href: "/o/s/staging", Tiles: 1, Status: "error",
			InstanceIDs: []string{"pg-1"}}, // same instance, one card
	}
	inst := []StackInstance{{ID: "pg-1", Name: "shared-pg", Detail: "postgres · stack-scoped"}}
	refs := []Reference{{InstanceID: "inst-1", Name: "other-pg", Detail: "postgres · shared"}}

	g := BuildStack(envs, inst, refs, VarCards{Scope: "stack"}, nil)
	g.Arrange(ArrangeClusters, nil)

	require.Len(t, g.Nodes, 4, "%+v", g.Nodes) // 2 envs + 1 instance + 1 ghost
	require.Len(t, g.Edges, 3, "%+v", g.Edges) // prod->inst, prod->ref, staging->inst
	var prod Node
	for _, n := range g.Nodes {
		if n.ID == EnvNodeID("production") {
			prod = n
		}
	}
	require.Equal(t, KindEnv, prod.Kind, "env card wrong: %+v", prod)
	require.Equal(t, "3 tiles", prod.Detail, "env card wrong: %+v", prod)
	require.Equal(t, "/o/s/production", prod.Href, "env card wrong: %+v", prod)
	// Columns: envs left of the instances they share, ghosts furthest right.
	byID := map[string]Node{}
	for _, n := range g.Nodes {
		byID[n.ID] = n
	}
	// Edge-driven layout: the envs feed the instance and the ghost, so both
	// sit one column right of the envs, on separate rows, not stacked.
	require.Less(t, byID[EnvNodeID("production")].X, byID[DBNodeID("pg-1")].X, "columns out of order: %+v", byID)
	require.Less(t, byID[EnvNodeID("production")].X, byID[RefNodeID("inst-1")].X, "columns out of order: %+v", byID)
	require.NotEqual(t, byID[DBNodeID("pg-1")].Y, byID[RefNodeID("inst-1")].Y, "instance and ghost stacked on one row: %+v", byID)
}

func TestBuildStackKeepsSavedPositions(t *testing.T) {
	envs := []EnvSummary{{Slug: "production", Name: "Production", Tiles: 1}}
	pos := map[string][2]float64{EnvNodeID("production"): {440, 308}}
	g := BuildStack(envs, nil, nil, VarCards{Scope: "stack"}, pos)
	g.Arrange(ArrangeClusters, pos)
	require.Equal(t, 440.0, g.Nodes[0].X, "saved position not applied: %+v", g.Nodes[0])
	require.Equal(t, 308.0, g.Nodes[0].Y, "saved position not applied: %+v", g.Nodes[0])
	require.True(t, g.Nodes[0].Saved, "saved position not applied: %+v", g.Nodes[0])
}

func TestBuildStackDropsEdgesToMissingCards(t *testing.T) {
	// An env referencing an instance that isn't drawn (e.g. owned in a stack
	// the viewer can't see) must not leave a dangling edge.
	envs := []EnvSummary{{Slug: "production", Tiles: 1, InstanceIDs: []string{"gone"}}}
	g := BuildStack(envs, nil, nil, VarCards{Scope: "stack"}, nil)
	require.Empty(t, g.Edges, "want no edges")
}

func TestBuildStackPutsProxyInFrontOfDomainedEnvs(t *testing.T) {
	envs := []EnvSummary{
		{Slug: "production", Name: "Production", Domains: []string{"shop.example.com"}},
		{Slug: "staging", Name: "Staging"}, // no proxied host: no chip, no edge
	}
	g := BuildStack(envs, nil, nil, VarCards{Scope: "stack"}, nil)
	g.Arrange(ArrangeClusters, nil)

	var proxy, prod, stag Node
	for _, n := range g.Nodes {
		switch n.ID {
		case ProxyNodeID:
			proxy = n
		case EnvNodeID("production"):
			prod = n
		case EnvNodeID("staging"):
			stag = n
		}
	}
	require.NotEmpty(t, proxy.ID, "proxy missing: %+v", proxy)
	require.Less(t, proxy.X, prod.X, "proxy not left of the envs: %+v", proxy)
	assertSystemZoneIsProxyOnly(t, g)
	require.Len(t, prod.Domains, 1, "domain chips wrong: %+v / %+v", prod.Domains, stag.Domains)
	require.Empty(t, stag.Domains, "domain chips wrong: %+v / %+v", prod.Domains, stag.Domains)
	var ingress []Edge
	for _, e := range g.Edges {
		if e.Kind == "ingress" {
			ingress = append(ingress, e)
		}
	}
	require.Len(t, ingress, 1, "want one ingress edge to production, got %+v", ingress)
	require.Equal(t, EnvNodeID("production"), ingress[0].To, "want one ingress edge to production, got %+v", ingress)

	g2 := BuildStack([]EnvSummary{{Slug: "staging", Name: "Staging"}}, nil, nil, VarCards{Scope: "stack"}, nil)
	require.Len(t, g2.Nodes, 1, "no domains anywhere should mean no proxy card: %+v", g2.Nodes)
}

func TestBuildStackWiresVarCardsOnlyToConsumers(t *testing.T) {
	envs := []EnvSummary{
		{Slug: "production", Name: "Production"},
		{Slug: "staging", Name: "Staging"}, // references nothing stack-scoped
	}
	vars := VarCards{
		Scope: "stack", Label: "Stack", Href: "/acme/shop/settings",
		Plain: 2, Secret: 1,
		PlainBy:  []string{EnvNodeID("production")},
		SecretBy: []string{EnvNodeID("production"), EnvNodeID("deleted")},
	}
	g := BuildStack(envs, nil, nil, vars, nil)
	g.Arrange(ArrangeClusters, nil)

	byID := map[string]Node{}
	for _, n := range g.Nodes {
		byID[n.ID] = n
	}
	require.Equal(t, "2 variables", byID[VarsNodeID("stack")].Detail, "var cards wrong: %+v / %+v", byID[VarsNodeID("stack")], byID[SecretsNodeID("stack")])
	require.Equal(t, "1 secret", byID[SecretsNodeID("stack")].Detail, "var cards wrong: %+v / %+v", byID[VarsNodeID("stack")], byID[SecretsNodeID("stack")])
	require.Less(t, byID[VarsNodeID("stack")].X, byID[EnvNodeID("production")].X, "var cards should sit left of the envs that read them")
	assertSystemZoneIsProxyOnly(t, g)
	// production reads both classes; staging reads neither; "deleted" is not a card
	want := map[string]bool{
		VarsNodeID("stack") + "->" + EnvNodeID("production"):    true,
		SecretsNodeID("stack") + "->" + EnvNodeID("production"): true,
	}
	got := map[string]bool{}
	for _, e := range g.Edges {
		got[e.From+"->"+e.To] = true
		require.Equal(t, "shared", e.Kind, "unexpected edge kind: %+v", e)
	}
	require.Len(t, got, len(want), "want %v, got %v", want, got)
	for k := range want {
		require.True(t, got[k], "missing edge %s (got %v)", k, got)
	}
}

func TestLayoutStepsOverHandDraggedCards(t *testing.T) {
	// The user dragged production into the column the vars card would take.
	pos := map[string][2]float64{EnvNodeID("production"): {0, 80}}
	envs := []EnvSummary{{Slug: "production", Name: "Production"}}
	vars := VarCards{Scope: "stack", Label: "Stack", Plain: 1, Href: "/x"}

	g := BuildStack(envs, nil, nil, vars, pos)
	g.Arrange(ArrangeClusters, pos)

	byID := map[string]Node{}
	for _, n := range g.Nodes {
		byID[n.ID] = n
	}
	prod, card := byID[EnvNodeID("production")], byID[VarsNodeID("stack")]
	require.Equal(t, 0.0, prod.X, "saved position moved: %+v", prod)
	require.Equal(t, 80.0, prod.Y, "saved position moved: %+v", prod)
	require.False(t, card.X == prod.X && card.Y == prod.Y, "vars card placed underneath the dragged env card at %v,%v", card.X, card.Y)
}

func TestBuildStackShowsInstanceExposure(t *testing.T) {
	envs := []EnvSummary{{Slug: "production", Name: "Production", InstanceIDs: []string{"s3-1"}, RefIDs: []string{"inst-2"}}}
	// A bucket served over a hostname, and a shared postgres on a host port.
	inst := []StackInstance{{ID: "s3-1", Name: "assets", Domains: []string{"cdn.example.com"}}}
	refs := []Reference{{InstanceID: "inst-2", Name: "shared-pg", ExternalPort: 5432}}

	g := BuildStack(envs, inst, refs, VarCards{Scope: "stack"}, nil)
	g.Arrange(ArrangeClusters, nil)

	var kinds []string
	for _, e := range g.Edges {
		switch {
		case e.From == ProxyNodeID && e.To == DBNodeID("s3-1") && e.Kind == "ingress":
			kinds = append(kinds, "ingress")
		case e.From == HostNodeID && e.To == RefNodeID("inst-2") && e.Kind == "port":
			kinds = append(kinds, "port")
		}
	}
	require.Len(t, kinds, 2, "want an ingress and a port edge, got %v from %+v", kinds, g.Edges)
	byID := map[string]Node{}
	for _, n := range g.Nodes {
		byID[n.ID] = n
	}
	require.NotEmpty(t, byID[ProxyNodeID].ID, "both system cards should exist: %+v", byID)
	require.NotEmpty(t, byID[HostNodeID].ID, "both system cards should exist: %+v", byID)
	require.NotEqual(t, byID[ProxyNodeID].Y, byID[HostNodeID].Y, "system cards stacked on the same row at y=%v", byID[ProxyNodeID].Y)
	assertSystemZoneIsProxyOnly(t, g)
	require.Equal(t, "cdn.example.com", byID[DBNodeID("s3-1")].Domains[0], "exposure not carried onto the cards: %+v", byID)
	require.Equal(t, 5432, byID[RefNodeID("inst-2")].ExternalPort, "exposure not carried onto the cards: %+v", byID)
}

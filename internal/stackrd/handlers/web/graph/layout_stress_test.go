package graph

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// countPassUnders is the layout quality metric: straight-line edges crossing
// a card that is not one of their endpoints (margin 0, the server-side view).
func countPassUnders(g Graph) int {
	byID := map[string]Node{}
	for _, n := range g.Nodes {
		byID[n.ID] = n
	}
	count := 0
	for _, e := range g.Edges {
		a, b := byID[e.From], byID[e.To]
		for _, n := range g.Nodes {
			if n.ID == e.From || n.ID == e.To {
				continue
			}
			if segHitsRect(a.X+CardW/2, a.Y+CardH/2, b.X+CardW/2, b.Y+CardH/2,
				n.X, n.Y, CardW, CardH+float64(len(n.Subtiles))*30) {
				count++
			}
		}
	}
	return count
}

// Stress topologies: the layout must stay clean as environments grow past
// what the demo env looks like. Bounds are loose on purpose, they catch
// regressions, not perfection.
func TestLayoutStressTopologies(t *testing.T) {
	// (a) 6 independent islands: app + db + bucket, all behind the proxy
	t.Run("six islands", func(t *testing.T) {
		var apps, dbs []repo.Tile
		pairs := map[repo.Tile][]repo.Tile{}
		domains := map[string][]string{}
		for i := 1; i <= 6; i++ {
			a := mkApp(i, "")
			d1, d2 := mkDB(i*10, 0), mkDB(i*10+1, 0)
			apps = append(apps, a)
			dbs = append(dbs, d1, d2)
			pairs[a] = []repo.Tile{d1, d2}
			domains[a.ID] = []string{fmt.Sprintf("a%d.dev", i)}
		}
		// clusters staggers the bands so even the proxy fan stays clear; flow
		// keeps strict columns, which accepts the fan slicing through them,
		// its bound only guards against getting worse.
		bound := map[ArrangeStyle]int{ArrangeClusters: 1, ArrangeFlow: 14}
		for _, style := range []ArrangeStyle{ArrangeClusters, ArrangeFlow} {
			g := Build(apps, dbs, nil, domains, nil, nil, nil, refs(pairs))
			g.Arrange(style, nil)
			checkInvariants(t, g)
			assert.LessOrEqual(t, countPassUnders(g), bound[style], "%s: six islands regressed", style)
		}
	})

	// (b) microservice mesh: 8 apps in a chain with skip links + own dbs
	t.Run("mesh with skips", func(t *testing.T) {
		var apps, dbs []repo.Tile
		for i := 1; i <= 8; i++ {
			apps = append(apps, mkApp(i, ""))
			dbs = append(dbs, mkDB(i, 0))
		}
		pairs := map[repo.Tile][]repo.Tile{}
		for i := 0; i < 8; i++ {
			targets := []repo.Tile{dbs[i]}
			if i+1 < 8 {
				targets = append(targets, apps[i+1])
			}
			if i+3 < 8 {
				targets = append(targets, apps[i+3]) // skip link
			}
			pairs[apps[i]] = targets
		}
		for _, style := range []ArrangeStyle{ArrangeClusters, ArrangeFlow} {
			g := Build(apps, dbs, nil, nil, nil, nil, nil, refs(pairs))
			g.Arrange(style, nil)
			checkInvariants(t, g)
			assert.LessOrEqual(t, countPassUnders(g), 4, "%s: mesh should stay mostly clean", style)
		}
	})

	// (c) the shop shape, doubled: 8 apps, shared dbs, multi-target crons
	t.Run("double shop", func(t *testing.T) {
		var apps, dbs []repo.Tile
		for i := 1; i <= 8; i++ {
			apps = append(apps, mkApp(i, ""))
		}
		for i := 1; i <= 6; i++ {
			dbs = append(dbs, mkDB(i, 0))
		}
		crons := []repo.Tile{mkApp(101, ""), mkApp(102, ""), mkApp(103, "")}
		pairs := map[repo.Tile][]repo.Tile{
			apps[0]: {apps[1], apps[2], apps[3], dbs[0]},
			apps[1]: {apps[2], dbs[1]},
			apps[2]: {apps[3], dbs[2]},
			apps[3]: {dbs[2]},
			apps[4]: {apps[5], apps[6], apps[7], dbs[3]},
			apps[5]: {apps[6], dbs[4]},
			apps[6]: {apps[7], dbs[5]},
			apps[7]: {dbs[5]},
			// crons fanning across several targets, like reconcile-stock
			crons[0]: {apps[2], dbs[2], apps[3]},
			crons[1]: {dbs[1]},
			crons[2]: {apps[6], dbs[5]},
		}
		all := append(append([]repo.Tile{}, apps...), crons...)
		domains := map[string][]string{apps[0].ID: {"x.dev"}, apps[4].ID: {"y.dev"}}
		for _, style := range []ArrangeStyle{ArrangeClusters, ArrangeFlow} {
			g := Build(all, dbs, nil, domains, nil, nil, nil, refs(pairs))
			g.Arrange(style, nil)
			checkInvariants(t, g)
			n := countPassUnders(g)
			t.Logf("%s: %d nodes, %d edges, %d pass-unders", style, len(g.Nodes), len(g.Edges), n)
			assert.LessOrEqual(t, n, 5, "%s: double shop too messy", style)
		}
	})
}

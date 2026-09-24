package graph

import (
	"slices"
	"sort"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/canvas"
)

// arrange places the cards without a saved position (graph-ref §6). Saved
// positions always win. ponytail: one layered engine for the env canvas
// (v0's "flow"), no clusters, satellites or crossing polish; add them if a
// real env reads badly.
func arrange(v *View, saved map[string]canvas.Point) {
	var taken []box
	for i := range v.Nodes {
		n := &v.Nodes[i]
		if p, ok := saved[n.ID]; ok {
			n.X, n.Y, n.Saved = p.X, p.Y, true
			taken = append(taken, boxOf(*n))
		}
	}
	switch v.Scope.Kind {
	case canvas.Home:
		grid(v, &taken)
	case canvas.Org, canvas.Stack:
		columns(v, &taken)
	case canvas.Env:
		layers(v, &taken)
	}
}

type box struct{ x, y, w, h int }

func boxOf(n Node) box { return box{n.X, n.Y, n.W, n.H} }

func (a box) hits(b box) bool {
	return a.x < b.x+b.w+GapX/2 && b.x < a.x+a.w+GapX/2 && a.y < b.y+b.h+GapY/2 && b.y < a.y+a.h+GapY/2
}

func free(b box, taken []box) bool {
	return !slices.ContainsFunc(taken, b.hits)
}

func snap(v int) int {
	if v < 0 {
		return -snap(-v)
	}
	return (v + Grid/2) / Grid * Grid
}

// put places n at (x, y), or steps down until it fits.
func put(n *Node, x, y int, taken *[]box) {
	b := box{snap(x), snap(y), n.W, n.H}
	for !free(b, *taken) {
		b.y = snap(b.y + CardH + GapY)
	}
	n.X, n.Y = b.x, b.y
	*taken = append(*taken, b)
}

const (
	pitchX = CardW + GapX
	pitchY = CardH + GapY
)

// grid is the home canvas: a wrapping grid, four across.
func grid(v *View, taken *[]box) {
	cell := 0
	for i := range v.Nodes {
		n := &v.Nodes[i]
		if n.Saved {
			continue
		}
		for {
			b := box{snap(cell % 4 * pitchX), snap(cell / 4 * pitchY), n.W, n.H}
			cell++
			if free(b, *taken) {
				n.X, n.Y = b.x, b.y
				*taken = append(*taken, b)
				break
			}
		}
	}
}

// columns is the org and stack canvas: sources on the left (connectors,
// vars), the drill-down cards in the next column, in the order given.
func columns(v *View, taken *[]box) {
	for i := range v.Nodes {
		n := &v.Nodes[i]
		if n.Saved {
			continue
		}
		col := 1
		if n.Kind == KindConnector || n.Kind == KindVars {
			col = 0
		}
		put(n, col*(pitchX+GapX), 0, taken)
	}
}

// layers is the env canvas: the system column behind the divider, then
// workload cards in columns by longest path along the edges (consumer
// left of what it uses), a newcomer beside a saved neighbour, loners in a
// row below everything.
func layers(v *View, taken *[]box) {
	idx := map[string]int{}
	for i, n := range v.Nodes {
		idx[n.ID] = i
	}
	depth := make([]int, len(v.Nodes))
	linked := make([]bool, len(v.Nodes))
	nbr := make([][]int, len(v.Nodes))
	for range v.Nodes { // ponytail: Bellman-style relax, capped at n rounds for cycles
		for _, e := range v.Edges {
			a, okA := idx[e.From]
			b, okB := idx[e.To]
			if !okA || !okB || v.Nodes[a].System || v.Nodes[b].System {
				continue
			}
			linked[a], linked[b] = true, true
			depth[b] = max(depth[b], min(depth[a]+1, len(v.Nodes)))
		}
	}
	for _, e := range v.Edges {
		a, okA := idx[e.From]
		b, okB := idx[e.To]
		if okA && okB {
			nbr[a], nbr[b] = append(nbr[a], b), append(nbr[b], a)
		}
	}
	x0 := 0
	if v.Divider > 0 {
		x0 = v.Divider + sysGap
	}
	order := make([]int, len(v.Nodes))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool { return depth[order[i]] < depth[order[j]] })
	var loners []int
	for _, i := range order {
		n := &v.Nodes[i]
		switch {
		case n.Saved:
		case n.System:
			put(n, 0, 0, taken)
		case !linked[i]:
			loners = append(loners, i)
		case beside(v, n, nbr[i], taken):
		default:
			put(n, x0+depth[i]*pitchX, 0, taken)
		}
	}
	bottom := 0
	for _, b := range *taken {
		bottom = max(bottom, b.y+b.h)
	}
	for k, i := range loners {
		put(&v.Nodes[i], x0+k*pitchX, bottom+2*GapY, taken)
	}
}

// beside puts a newcomer next to its first saved neighbour: left, right,
// above, below; false when none is free.
func beside(v *View, n *Node, nbr []int, taken *[]box) bool {
	for _, j := range nbr {
		m := v.Nodes[j]
		if !m.Saved {
			continue
		}
		for _, d := range [][2]int{{-pitchX, 0}, {pitchX, 0}, {0, -pitchY}, {0, m.H + GapY}} {
			b := box{snap(m.X + d[0]), snap(m.Y + d[1]), n.W, n.H}
			if free(b, *taken) && (v.Divider == 0 || b.x >= v.Divider) {
				n.X, n.Y = b.x, b.y
				*taken = append(*taken, b)
				return true
			}
		}
	}
	return false
}

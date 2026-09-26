package graph

import (
	"cmp"
	"maps"
	"math"
	"slices"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/canvas"
)

// arrange places the cards without a saved position (graph-ref §6): a port
// of v0's layout, same arithmetic, same cards in the same places. Saved
// positions always win. Home wraps its org cards into columns; every level
// below lays connected groups out as islands (v0's "clusters", which it used
// for org, stack and env alike). Deterministic: same graph, same result.
func arrange(v *View, saved map[string]canvas.Point) {
	for i := range v.Nodes {
		n := &v.Nodes[i]
		p, ok := saved[n.ID]
		if !ok {
			continue
		}
		n.X = p.X
		n.Y = p.Y
		n.Saved = true
	}
	switch v.Scope.Kind {
	case canvas.Home:
		layoutOrgs(v.Nodes)
	case canvas.Org, canvas.Stack, canvas.Env:
		layoutClusters(v)
	}
}

// ---- home -------------------------------------------------------------------

// orgsPerColumn wraps the home canvas: with no edges there is no flow to
// read, so the only job is keeping a long org list from becoming one column.
const orgsPerColumn = 4

// layoutOrgs is the home canvas: org cards by id, four to a column.
func layoutOrgs(nodes []Node) {
	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.ID)
	}
	slices.Sort(ids) // the order placeColumns fills columns in
	col := make(map[string]float64, len(ids))
	for i, id := range ids {
		col[id] = float64(i/orgsPerColumn) * colGap
	}
	placeColumns(nodes, func(n Node) float64 { return col[n.ID] })
}

// overlaps reports whether a card at (x, y) would sit too close to one at
// (ox, oy): the card box plus the clear gaps around it.
func overlaps(x, y, ox, oy float64) bool {
	return x < ox+CardW+GapX && ox < x+CardW+GapX &&
		y < oy+CardH+GapY && oy < y+CardH+GapY
}

// placeColumns fills unsaved cards into their column top to bottom,
// stepping over any row a saved card occupies: without that step a new card
// can land exactly under one the user placed there and disappear.
func placeColumns(nodes []Node, colX func(Node) float64) {
	type box struct{ x, y float64 }
	var taken []box
	free := func(x, y float64) bool {
		for _, b := range taken {
			if overlaps(x, y, b.x, b.y) {
				return false
			}
		}
		return true
	}
	var idx []int
	for i, n := range nodes {
		if n.Saved {
			taken = append(taken, box{float64(n.X), float64(n.Y)})
			continue
		}
		idx = append(idx, i)
	}
	slices.SortStableFunc(idx, func(a, b int) int { return strings.Compare(nodes[a].ID, nodes[b].ID) })
	next := map[float64]float64{} // per column, not per kind: kinds can share one
	for _, i := range idx {
		x := colX(nodes[i])
		y := rowStart + next[x]*rowGap
		for !free(x, y) {
			next[x]++
			y = rowStart + next[x]*rowGap
		}
		nodes[i].X = int(math.Round(x))
		nodes[i].Y = int(math.Round(y))
		next[x]++
		taken = append(taken, box{x, y})
	}
}

// ---- org, stack, env: islands ---------------------------------------------

// pair is one deduped undirected link, kept in its build direction.
type pair struct{ from, to int }

// pt is a card's working position. v0 computed in float64; the port keeps
// its arithmetic and rounds to ints once, at the end.
type pt struct{ x, y float64 }

// layout is one clusters run over a canvas. Cards are addressed by their
// index in nodes; every order that matters is sorted by card id.
type layout struct {
	nodes   []Node
	pos     []pt
	placed  []bool
	links   []pair
	seen    map[pair]bool
	adj     [][]int
	satHost map[int]int // satellite -> host
	sats    []int       // satellites, id order
	perHost map[int]int // satellites attached so far, per host
}

// layoutClusters is v0's Arrange with its clusters engine: a newcomer
// beside its saved neighbour, the system column, each connected group as
// its own island with its satellites beside their host, a catch-all for
// satellites of hand-placed hosts, loners in a row below, the system cards
// re-centred on their fan, then a polish pass nudging cards off edges.
func layoutClusters(v *View) {
	l := newLayout(v)
	l.newcomers()
	var system, connected, orphans []int
	for i, n := range l.nodes {
		if l.placed[i] {
			continue
		}
		switch {
		case n.System:
			system = append(system, i)
		case len(l.adj[i]) == 0:
			orphans = append(orphans, i)
		default:
			connected = append(connected, i)
		}
	}
	l.settle(system, colSystemX, rowStart)
	spine, needLeft := l.satellites(connected)
	maxY := l.clusterLayout(spine, connected, needLeft)
	// catch-all: satellites whose host was hand-anchored or otherwise
	// placed outside the engines
	hosts := map[int]bool{}
	for _, h := range l.satHost {
		hosts[h] = true
	}
	maxY = max(maxY, l.attachSats(hosts))
	for i := range l.nodes {
		if l.placed[i] {
			maxY = max(maxY, l.pos[i].y)
		}
	}
	l.orphanRow(orphans, maxY)
	l.recentre(system)
	l.nudgeOffEdges()
	for i := range l.nodes {
		n := &l.nodes[i]
		if n.Saved {
			continue
		}
		n.X = int(math.Round(l.pos[i].x))
		n.Y = int(math.Round(l.pos[i].y))
	}
}

// newLayout reads the canvas: saved cards count as placed, and the links
// are one deduped line per pair of cards on the canvas, however many kinds
// of edge the pair carries. Egress edges are skipped, as v0 skipped its
// traffic edges: both come from the last traffic sample, and a layout that
// re-arranges with whatever talked lately reads as broken.
func newLayout(v *View) *layout {
	idx := make(map[string]int, len(v.Nodes))
	for i, n := range v.Nodes {
		idx[n.ID] = i
	}
	l := &layout{
		nodes:   v.Nodes,
		pos:     make([]pt, len(v.Nodes)),
		placed:  make([]bool, len(v.Nodes)),
		seen:    map[pair]bool{},
		adj:     make([][]int, len(v.Nodes)),
		satHost: map[int]int{},
		perHost: map[int]int{},
	}
	for i, n := range v.Nodes {
		l.pos[i] = pt{float64(n.X), float64(n.Y)}
		l.placed[i] = n.Saved
	}
	for _, e := range v.Edges {
		if e.Kind == EdgeEgress {
			continue
		}
		a, okA := idx[e.From]
		b, okB := idx[e.To]
		if !okA || !okB || a == b {
			continue
		}
		if l.seen[pair{a, b}] || l.seen[pair{b, a}] {
			continue
		}
		l.seen[pair{a, b}] = true
		l.links = append(l.links, pair{a, b})
		l.adj[a] = append(l.adj[a], b)
		l.adj[b] = append(l.adj[b], a)
	}
	return l
}

// snap rounds v to the grid with v0's formula: int() truncates toward
// zero, so a negative value snaps a cell high; kept so positions match v0.
func snap(v float64) float64 {
	return float64(int((v+Grid/2)/Grid)) * Grid
}

// byID orders cards by id, the tie-break every v0 sort ends on.
func (l *layout) byID(a, b int) int {
	return strings.Compare(l.nodes[a].ID, l.nodes[b].ID)
}

// subRoom is the room the sub-tile strips under card i take. Replica subs
// do not count: they exist only when Build reads status and Place builds
// without it, so counting them would move cards between the two.
func (l *layout) subRoom(i int) float64 {
	k := 0
	for _, s := range l.nodes[i].Subs {
		if s.Kind != KindReplica {
			k++
		}
	}
	return float64(k) * SubH
}

// tall is card i's collision height: the card plus its sub-tile strips, or
// a card lands where another's strip renders. CardH even for a short
// volume card, like v0.
func (l *layout) tall(i int) float64 {
	return CardH + l.subRoom(i)
}

// collides reports whether card skip at (x, y) would sit within the gaps of
// any placed card.
func (l *layout) collides(x, y float64, skip int) bool {
	hs := l.tall(skip)
	for j := range l.nodes {
		if j == skip || !l.placed[j] {
			continue
		}
		m := l.pos[j]
		if x < m.x+CardW+GapX && m.x < x+CardW+GapX &&
			y < m.y+l.tall(j)+GapY && m.y < y+hs+GapY {
			return true
		}
	}
	return false
}

// newcomers puts a new card whose neighbour was dragged somewhere specific
// beside that neighbour, not off in a fresh column with an edge crossing
// the canvas: one card-width left (flows read left to right), else right,
// above, below, else the left slot nudged down until it overlaps nothing.
func (l *layout) newcomers() {
	var fresh []int
	for i := range l.nodes {
		if !l.placed[i] {
			fresh = append(fresh, i)
		}
	}
	slices.SortFunc(fresh, l.byID)
	offs := [][2]float64{
		{-CardW - 80, 0},
		{CardW + 80, 0},
		{0, -CardH - 60},
		{0, CardH + 60},
	}
	for _, i := range fresh {
		anchor := -1
		for _, nb := range l.adj[i] {
			if !l.nodes[nb].Saved {
				continue
			}
			if anchor < 0 || l.nodes[nb].ID < l.nodes[anchor].ID {
				anchor = nb
			}
		}
		if anchor < 0 {
			continue // no hand-placed neighbour; the engines handle it
		}
		a := l.pos[anchor]
		var x, y float64
		found := false
		for _, off := range offs {
			x = snap(a.x + off[0])
			y = snap(a.y + off[1])
			if !l.collides(x, y, i) {
				found = true
				break
			}
		}
		if !found {
			x = snap(a.x - CardW - 80)
			y = snap(a.y)
			for l.collides(x, y, i) {
				y = snap(y + rowGap)
			}
		}
		l.pos[i] = pt{x, y}
		l.placed[i] = true
	}
}

// byName orders a column: the ingress proxy above the rest (a request flows
// proxy first), then by name, then by id.
func (l *layout) byName(idxs []int) {
	notProxy := func(i int) int {
		if l.nodes[i].Kind == KindProxy {
			return 0
		}
		return 1
	}
	slices.SortFunc(idxs, func(a, b int) int {
		return cmp.Or(
			cmp.Compare(notProxy(a), notProxy(b)),
			strings.Compare(l.nodes[a].Name, l.nodes[b].Name),
			l.byID(a, b),
		)
	})
}

// desired is the average Y of card i's placed neighbours, where it wants to
// be; false when none is placed yet.
func (l *layout) desired(i int) (float64, bool) {
	sum := 0.0
	cnt := 0
	for _, nb := range l.adj[i] {
		if l.placed[nb] {
			sum += l.pos[nb].y
			cnt++
		}
	}
	if cnt == 0 {
		return 0, false
	}
	return sum / float64(cnt), true
}

// settle places one column at x from startY down: each card at the average
// height of its placed neighbours, pushed down just enough to never
// overlap. It sorts idxs in place (callers read that order) and returns the
// lowest Y it used.
func (l *layout) settle(idxs []int, x, startY float64) float64 {
	l.byName(idxs)
	slices.SortStableFunc(idxs, func(a, b int) int {
		ya, oka := l.desired(a)
		yb, okb := l.desired(b)
		switch {
		case oka && okb:
			return cmp.Compare(ya, yb)
		case oka:
			return -1
		case okb:
			return 1
		}
		return 0
	})
	nextY := startY
	maxY := startY
	for _, i := range idxs {
		y := nextY
		d, ok := l.desired(i)
		if ok && d > y {
			y = d
		}
		y = snap(y)
		for l.collides(x, y, i) {
			y = snap(y + rowGap)
		}
		l.pos[i] = pt{x, y}
		l.placed[i] = true
		maxY = max(maxY, y)
		// leave room for the sub-tiles stacked under this one
		nextY = y + rowGap + l.subRoom(i)
	}
	return maxY
}

// layer is each member's longest-path depth along edge direction, capped so
// a cycle can't spin forever: the left-to-right order inside an island.
func (l *layout) layer(members map[int]bool) map[int]int {
	depth := map[int]int{}
	for range len(members) {
		changed := false
		for _, k := range l.links {
			if !members[k.from] || !members[k.to] {
				continue
			}
			d := depth[k.from] + 1
			if d > depth[k.to] && d <= len(members) {
				depth[k.to] = d
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	return depth
}

// satellites splits the connected cards. A card with exactly one link,
// hosted by a busier workload card (2+ links), skips the column engine and
// attaches beside its host, so its edge is one card long: a cron and the app
// it triggers, a db and the app that reads it. A lone app behind the proxy
// is not the proxy's satellite, and a pure pair stays a two-card island.
// needLeft: some satellite points into its host (a trigger) and parks left.
func (l *layout) satellites(connected []int) (spine []int, needLeft bool) {
	for _, i := range connected {
		if len(l.adj[i]) == 1 {
			h := l.adj[i][0]
			if !l.nodes[h].System && len(l.adj[h]) >= 2 {
				l.satHost[i] = h
				if l.seen[pair{i, h}] {
					needLeft = true
				}
				continue
			}
		}
		spine = append(spine, i)
	}
	l.sats = slices.SortedFunc(maps.Keys(l.satHost), l.byID)
	return spine, needLeft
}

// attachSats places every unplaced satellite whose host is in hosts and
// returns the lowest Y it used. Satellites cycle around their host (right,
// tucked below-right, under, above-right; mirrored left for a trigger)
// instead of stacking on one side, where the chord to the second would pass
// under the first. The per-host count runs across calls.
func (l *layout) attachSats(hosts map[int]bool) float64 {
	low := float64(rowStart)
	for _, s := range l.sats {
		h := l.satHost[s]
		if l.placed[s] || !hosts[h] {
			continue
		}
		hostH := l.tall(h)
		k := l.perHost[h]
		l.perHost[h]++
		offs := [][2]float64{ // read: right first, then in-and-down so chords never shadow
			{localGap, 0},
			{localGap / 2, rowGap},
			{0, hostH + 60},
			{localGap, -rowGap},
		}
		if l.seen[pair{s, h}] { // trigger: left first, then in-and-up
			offs = [][2]float64{
				{-localGap, 0},
				{-localGap / 2, -rowGap},
				{0, -(CardH + 60)},
				{-localGap, rowGap},
			}
		}
		off := offs[k%len(offs)]
		x := snap(l.pos[h].x + off[0])
		y := snap(l.pos[h].y + off[1])
		for l.collides(x, y, s) {
			y = snap(y + rowGap)
		}
		l.pos[s] = pt{x, y}
		l.placed[s] = true
		low = max(low, y+l.tall(s))
	}
	return low
}

// components numbers the connected components of set minus without,
// walking ids in order so the numbering is stable.
func (l *layout) components(set, without map[int]bool) map[int]int {
	comp := map[int]int{}
	next := 0
	for _, id := range slices.SortedFunc(maps.Keys(set), l.byID) {
		if without[id] {
			continue
		}
		if _, done := comp[id]; done {
			continue
		}
		queue := []int{id}
		comp[id] = next
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			for _, nb := range l.adj[cur] {
				if !set[nb] || without[nb] {
					continue
				}
				if _, done := comp[nb]; !done {
					comp[nb] = next
					queue = append(queue, nb)
				}
			}
		}
		next++
	}
	return comp
}

// clusterLayout lays the spine out as islands: connected components of the
// workload graph minus hubs. A hub is a card whose removal splits its
// neighbours into 3+ groups (a shared instance, a vars card everyone reads);
// hubs get their own column on the left, and each island lays out as a
// small left-to-right flow in its own horizontal band. Returns the lowest Y.
func (l *layout) clusterLayout(spine, full []int, needLeft bool) float64 {
	// Two graphs: hubs are detected on the full workload graph (the
	// satellites are what give welded groups their size), islands are built
	// from the spine only.
	fullSet := map[int]bool{}
	for _, i := range full {
		fullSet[i] = true
	}
	spineSet := map[int]bool{}
	for _, i := range spine {
		spineSet[i] = true
	}
	deg := func(i int) int {
		n := 0
		for _, nb := range l.adj[i] {
			if fullSet[nb] {
				n++
			}
		}
		return n
	}
	// highest degree first, so the biggest weld is tested before the groups
	// it created are attributed to someone else
	var candidates []int
	for _, i := range spine {
		if deg(i) >= 3 {
			candidates = append(candidates, i)
		}
	}
	slices.SortFunc(candidates, func(a, b int) int {
		return cmp.Or(cmp.Compare(deg(b), deg(a)), l.byID(a, b))
	})
	hubs := map[int]bool{}
	for _, c := range candidates {
		without := maps.Clone(hubs)
		without[c] = true
		comp := l.components(fullSet, without)
		size := map[int]int{}
		for _, g := range comp {
			size[g]++
		}
		// Only groups of 2+ count: an app with three leaf satellites (db,
		// bucket, cron) is an island's centre, not a hub between islands.
		groups := map[int]bool{}
		for _, nb := range l.adj[c] {
			g, ok := comp[nb]
			if ok && size[g] >= 2 {
				groups[g] = true
			}
		}
		if len(groups) >= 3 {
			hubs[c] = true
		}
	}

	comp := l.components(spineSet, hubs)
	byComp := map[int][]int{}
	for _, i := range spine {
		c, ok := comp[i]
		if ok {
			byComp[c] = append(byComp[c], i)
		}
	}
	// biggest island first, ties broken by their smallest card id
	type cluster struct {
		members []int
		key     int
	}
	var clusters []cluster
	for _, c := range slices.Sorted(maps.Keys(byComp)) {
		clusters = append(clusters, cluster{
			members: byComp[c],
			key:     slices.MinFunc(byComp[c], l.byID),
		})
	}
	slices.SortFunc(clusters, func(a, b cluster) int {
		return cmp.Or(cmp.Compare(len(b.members), len(a.members)), l.byID(a.key, b.key))
	})

	// Hubs get the column between the system zone and the islands, trigger
	// satellites the one after that; islands shift right only for columns
	// that actually exist.
	baseX := float64(colSystemX + colGap)
	var hubIdx []int
	if len(hubs) > 0 {
		for _, i := range spine {
			if hubs[i] {
				hubIdx = append(hubIdx, i)
			}
		}
		baseX += localGap
	}
	if needLeft {
		baseX += localGap
	}

	bandY := float64(rowStart)
	maxY := float64(rowStart)
	for ci, cl := range clusters {
		// stagger the bands into a staircase: a straight left wall of
		// islands puts every card in the proxy fan's path; each band indents
		// a half step so the chords to lower bands clear the ones above
		indent := float64(ci) * (localGap / 2)
		members := map[int]bool{}
		for _, i := range cl.members {
			members[i] = true
		}
		depth := l.layer(members)
		colsOf := map[int][]int{}
		for _, i := range cl.members {
			colsOf[depth[i]] = append(colsOf[depth[i]], i)
		}
		bandMax := bandY
		for _, d := range slices.Sorted(maps.Keys(colsOf)) {
			bandMax = max(bandMax, l.settle(colsOf[d], baseX+indent+float64(d)*localGap, bandY))
		}
		// this island's satellites join it now, so the next band starts below them
		bandMax = max(bandMax, l.attachSats(members))
		maxY = max(maxY, bandMax)
		bandY = snap(bandMax + rowGap*1.5)
	}

	// hubs settle beside the islands they serve, at the average height of
	// their neighbours, which settle does once the islands are placed
	if len(hubIdx) > 0 {
		maxY = max(maxY, l.settle(hubIdx, colSystemX+colGap, rowStart))
		hubSet := map[int]bool{}
		for _, i := range hubIdx {
			hubSet[i] = true
		}
		maxY = max(maxY, l.attachSats(hubSet))
	}
	return maxY
}

// orphanRow parks the unconnected cards below everything with a visual
// gap, wrapping every four so a pile of them stays a block.
func (l *layout) orphanRow(orphans []int, maxY float64) {
	if len(orphans) == 0 {
		return
	}
	l.byName(orphans)
	y := snap(maxY + rowGap*1.5)
	x := float64(colSystemX + colGap)
	for k, i := range orphans {
		if k > 0 && k%4 == 0 {
			x = colSystemX + colGap
			y = snap(y + rowGap)
		}
		for l.collides(x, y, i) {
			y = snap(y + rowGap)
		}
		l.pos[i] = pt{x, y}
		l.placed[i] = true
		x += CardW + GapX + 20
	}
}

// recentre moves each system card to the vertical middle of its fan: it
// was settled first, at the top, but a proxy wired to six islands reads
// best in the middle, the fan spreading evenly instead of the bottom chords
// slicing through every island above.
func (l *layout) recentre(system []int) {
	for _, i := range system {
		d, ok := l.desired(i)
		if !ok {
			continue
		}
		y := snap(d)
		for l.collides(l.pos[i].x, y, i) {
			y = snap(y + rowGap)
		}
		l.pos[i].y = y
	}
}

// segHitsRect reports whether the segment (x1,y1)-(x2,y2) crosses the given
// rectangle (Liang-Barsky clip).
func segHitsRect(x1, y1, x2, y2, rx, ry, rw, rh float64) bool {
	dx := x2 - x1
	dy := y2 - y1
	t0 := 0.0
	t1 := 1.0
	clip := func(p, q float64) bool {
		if p == 0 {
			return q >= 0
		}
		r := q / p
		if p < 0 {
			if r > t1 {
				return false
			}
			t0 = max(t0, r)
		} else {
			if r < t0 {
				return false
			}
			t1 = min(t1, r)
		}
		return true
	}
	return clip(-dx, x1-rx) && clip(dx, rx+rw-x1) && clip(-dy, y1-ry) && clip(dy, ry+rh-y1)
}

// hits reports whether link s, drawn centre to centre, passes under card i
// (not one of its ends), the card box inflated by margin m.
func (l *layout) hits(s pair, i int, m float64) bool {
	if i == s.from || i == s.to {
		return false
	}
	a := l.pos[s.from]
	b := l.pos[s.to]
	c := l.pos[i]
	return segHitsRect(
		a.x+CardW/2, a.y+l.tall(s.from)/2,
		b.x+CardW/2, b.y+l.tall(s.to)/2,
		c.x-m, c.y-m, CardW+2*m, l.tall(i)+2*m,
	)
}

// crossings counts (link, card) pairs where the link passes under a placed
// card, boxes inflated by m.
func (l *layout) crossings(segs []pair, m float64) int {
	n := 0
	for _, s := range segs {
		for i := range l.nodes {
			if l.placed[i] && l.hits(s, i, m) {
				n++
			}
		}
	}
	return n
}

// nudgeOffEdges greedily relocates auto-placed cards while any edge passes
// under a card that is not one of its ends. Boxes are inflated by a small
// margin so the client's curved edges, which bow off the straight chord
// tested here, mostly stay clear too; a large one would make every near
// slot look occupied and the greedy would fling cards far away. Saved and
// system cards never move.
// ponytail: greedy with a fixed candidate set, it can park on a local
// optimum; a real search (annealing) if topologies show up it can't clean.
func (l *layout) nudgeOffEdges() {
	const margin = 12.0
	var segs []pair
	for _, k := range l.links {
		if l.placed[k.from] && l.placed[k.to] {
			segs = append(segs, k)
		}
	}
	// Only engage when an edge really crosses a card (no margin): the
	// margined score also counts near-misses, and chasing those on a clean
	// canvas just shuffles cards for nothing.
	if l.crossings(segs, 0) == 0 {
		return
	}
	// One crossing costs as much as ~400px of extra edge: a short slide that
	// clears an edge is worth it, flinging a card across the canvas is not.
	// The length term keeps the result compact.
	score := func() float64 {
		total := 0.0
		for _, s := range segs {
			dx := l.pos[s.from].x - l.pos[s.to].x
			dy := l.pos[s.from].y - l.pos[s.to].y
			total += dx*dx + dy*dy
		}
		return float64(l.crossings(segs, margin))*400*400 + total/64
	}
	// involved: card i is the crossed card or an end of a crossing link
	involved := func(i int) bool {
		for _, s := range segs {
			for j := range l.nodes {
				if l.placed[j] && l.hits(s, j, margin) && (j == i || s.from == i || s.to == i) {
					return true
				}
			}
		}
		return false
	}
	var movable []int
	for i, n := range l.nodes {
		if l.placed[i] && !n.Saved && !n.System {
			movable = append(movable, i)
		}
	}
	slices.SortFunc(movable, l.byID)
	offsets := [][2]float64{
		{0, -rowGap},
		{0, rowGap},
		{-localGap, 0},
		{localGap, 0},
		{-localGap, -rowGap},
		{localGap, -rowGap},
		{-localGap, rowGap},
		{localGap, rowGap},
		{0, -2 * rowGap},
		{0, 2 * rowGap},
		{-2 * localGap, 0},
		{2 * localGap, 0},
		{-localGap, 2 * rowGap},
		{localGap, 2 * rowGap},
		{0, -3 * rowGap},
		{0, 3 * rowGap},
	}
	const minX = colSystemX + colGap - 80 // just right of the divider, like a hand drag

	moves := map[int]int{}
	best := score()
	for range 40 {
		if l.crossings(segs, margin) == 0 {
			return
		}
		improved := false
		for _, i := range movable {
			// a card that keeps getting shoved is the score chasing noise;
			// cap it so the layout stays recognisable
			if moves[i] >= 4 || !involved(i) {
				continue
			}
			// best-of-candidates, not first-improvement: a greedy first
			// pick happily trades one violation for another and stalls
			o := l.pos[i]
			bp := o
			bs := best
			for _, off := range offsets {
				x := snap(o.x + off[0])
				y := snap(o.y + off[1])
				if x < minX || l.collides(x, y, i) {
					continue
				}
				l.pos[i] = pt{x, y}
				sc := score()
				if sc < bs-0.5 {
					bp = pt{x, y}
					bs = sc
				}
			}
			l.pos[i] = bp
			if bs < best {
				best = bs
				improved = true
				moves[i]++
			}
		}
		if !improved {
			return
		}
	}
}

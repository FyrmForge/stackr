package graph

import (
	"encoding/json"
	"sort"
)

// ArrangeStyle picks the auto-layout engine for cards without a saved
// position. It lives in the user's graph-prefs blob: the canvas client owns
// that JSON, the server reads this one key back out of it.
type ArrangeStyle string

const (
	// ArrangeClusters lays each connected group out as its own island, an
	// app stays beside its databases, buckets and crons. Cards that stitch
	// several islands together (a shared instance) become hubs in their own
	// column. The default: real environments are mostly parallel app-stacks.
	ArrangeClusters ArrangeStyle = "clusters"
	// ArrangeFlow layers cards left to right by edge direction: triggers and
	// ingress first, the services they call next, what those read after.
	ArrangeFlow ArrangeStyle = "flow"
)

// localGap is the column pitch inside an island and the offset a satellite
// keeps from its host, tighter than the canvas-wide colGap.
const localGap = CardW + 80

// pair is one deduped undirected edge, kept in its build direction.
type pair struct{ from, to string }

// StyleFromPrefs reads the arrange key out of a graph-prefs JSON blob.
// Anything unrecognised (including "") is the default.
func StyleFromPrefs(prefs string) ArrangeStyle {
	var p struct {
		Arrange string `json:"arrange"`
	}
	_ = json.Unmarshal([]byte(prefs), &p)
	if ArrangeStyle(p.Arrange) == ArrangeFlow {
		return ArrangeFlow
	}
	return ArrangeClusters
}

// Arrange assigns positions to every node that has no saved one; saved drags
// always win. Call it after the graph is fully assembled (references, var
// cards) so every card takes part. Deterministic: same graph, same style,
// same result.
func (g *Graph) Arrange(style ArrangeStyle, positions map[string][2]float64) {
	nodes, edges := g.Nodes, g.Edges
	idx := map[string]int{}
	for i := range nodes {
		idx[nodes[i].ID] = i
	}
	placed := map[string]bool{}
	for i := range nodes {
		if _, saved := positions[nodes[i].ID]; saved {
			placed[nodes[i].ID] = true
			nodes[i].Saved = true
		}
	}

	// Deduped edge list over nodes actually on the canvas: layout math wants
	// one line per pair, however many kinds of edge the pair carries.
	var links []pair
	seen := map[pair]bool{}
	adj := map[string][]string{}
	for _, e := range edges {
		if e.Kind == "traffic" {
			// Observed flows are drawn but never drive placement: they change
			// with whatever talked in the last two minutes, and a layout that
			// re-arranges differently every click reads as broken.
			continue
		}
		if _, ok := idx[e.From]; !ok {
			continue
		}
		if _, ok := idx[e.To]; !ok {
			continue
		}
		if e.From == e.To || seen[pair{e.From, e.To}] || seen[pair{e.To, e.From}] {
			continue
		}
		seen[pair{e.From, e.To}] = true
		links = append(links, pair{e.From, e.To})
		adj[e.From] = append(adj[e.From], e.To)
		adj[e.To] = append(adj[e.To], e.From)
	}

	// Card height including the sub-tile strips hanging below it, the
	// collision box must reserve them, or a card lands where another card's
	// sharedpg strip renders and they visually overlap.
	tall := func(id string) float64 {
		if i, ok := idx[id]; ok {
			return CardH + float64(len(nodes[i].Subtiles))*30
		}
		return CardH
	}
	collides := func(x, y float64, skip string) bool {
		hs := tall(skip)
		for _, m := range nodes {
			if m.ID == skip || !placed[m.ID] {
				continue
			}
			if x < m.X+CardW+CardGapX && m.X < x+CardW+CardGapX &&
				y < m.Y+tall(m.ID)+CardGapY && m.Y < y+hs+CardGapY {
				return true
			}
		}
		return false
	}
	snap := func(v float64) float64 { return float64(int((v+GridPx/2)/GridPx)) * GridPx }

	// Hand-arranged canvases: a new card whose neighbor was dragged somewhere
	// specific belongs beside that neighbor, not off in a fresh column with an
	// edge crossing the whole canvas. Place it one card-width to the left
	// (flows read left to right), nudged down until it overlaps nothing.
	var newcomers []int
	for i, n := range nodes {
		if !placed[n.ID] {
			newcomers = append(newcomers, i)
		}
	}
	sort.Slice(newcomers, func(a, b int) bool { return nodes[newcomers[a]].ID < nodes[newcomers[b]].ID })
	for _, i := range newcomers {
		n := &nodes[i]
		var anchor *Node
		for _, nb := range adj[n.ID] {
			if _, saved := positions[nb]; !saved {
				continue
			}
			m := &nodes[idx[nb]]
			if anchor == nil || m.ID < anchor.ID {
				anchor = m
			}
		}
		if anchor == nil {
			continue // no hand-placed neighbor; the engines below handle it
		}
		var x, y float64
		found := false
		for _, off := range [][2]float64{
			{-CardW - 80, 0}, {CardW + 80, 0}, {0, -CardH - 60}, {0, CardH + 60},
		} {
			x, y = snap(anchor.X+off[0]), snap(anchor.Y+off[1])
			if !collides(x, y, n.ID) {
				found = true
				break
			}
		}
		if !found {
			x, y = snap(anchor.X-CardW-80), snap(anchor.Y)
			for collides(x, y, n.ID) {
				y = snap(y + rowGap)
			}
		}
		n.X, n.Y = x, y
		placed[n.ID] = true
	}

	isSystem := func(n Node) bool { return n.Kind == KindProxy || n.Kind == KindHost }
	var system, connected, orphans []int
	for i, n := range nodes {
		if placed[n.ID] {
			continue
		}
		switch {
		case isSystem(n):
			system = append(system, i)
		case len(adj[n.ID]) == 0:
			orphans = append(orphans, i)
		default:
			connected = append(connected, i)
		}
	}

	byName := func(idxs []int) {
		sort.Slice(idxs, func(a, b int) bool {
			// ingress proxy above host-port publishing, like a request flows
			ka, kb := nodes[idxs[a]].Kind, nodes[idxs[b]].Kind
			if ka != kb && (ka == KindProxy || kb == KindProxy) {
				return ka == KindProxy
			}
			if nodes[idxs[a]].Name != nodes[idxs[b]].Name {
				return nodes[idxs[a]].Name < nodes[idxs[b]].Name
			}
			return nodes[idxs[a]].ID < nodes[idxs[b]].ID
		})
	}
	// average Y of already-placed neighbors, where this node "wants" to be
	desired := func(id string) (float64, bool) {
		sum, cnt := 0.0, 0
		for _, nb := range adj[id] {
			if placed[nb] {
				sum += nodes[idx[nb]].Y
				cnt++
			}
		}
		if cnt == 0 {
			return 0, false
		}
		return sum / float64(cnt), true
	}
	// settle one column: each card at the average height of its placed
	// neighbors, pushed down just enough to never overlap (top-to-bottom).
	// startY is where the column begins, rowStart, or a cluster's band.
	settle := func(idxs []int, x, startY float64) float64 {
		byName(idxs)
		sort.SliceStable(idxs, func(a, b int) bool {
			ya, oka := desired(nodes[idxs[a]].ID)
			yb, okb := desired(nodes[idxs[b]].ID)
			if oka && okb {
				return ya < yb
			}
			return oka && !okb
		})
		nextY, maxY := startY, startY
		for _, i := range idxs {
			y := nextY
			if d, ok := desired(nodes[i].ID); ok && d > y {
				y = d
			}
			y = snap(y)
			for collides(x, y, nodes[i].ID) {
				y = snap(y + rowGap)
			}
			nodes[i].X = x
			nodes[i].Y = y
			placed[nodes[i].ID] = true
			if y > maxY {
				maxY = y
			}
			// leave room for sub-tiles stacked under this one
			nextY = y + rowGap + float64(len(nodes[i].Subtiles))*30
		}
		return maxY
	}

	settle(system, colSystemX, rowStart)

	// Directed layer depth: longest path along edge direction, capped so a
	// cycle can't spin forever. Shared by flow (columns) and clusters (the
	// left-to-right order inside one island).
	layer := func(members map[string]bool) map[string]int {
		depth := map[string]int{}
		for range members {
			changed := false
			for _, l := range links {
				if !members[l.from] || !members[l.to] {
					continue
				}
				if d := depth[l.from] + 1; d > depth[l.to] && d <= len(members) {
					depth[l.to] = d
					changed = true
				}
			}
			if !changed {
				break
			}
		}
		return depth
	}

	// Satellites: a card with exactly one link, hosted by a busier card, a
	// cron and the app it triggers, a db and the app that reads it. They skip
	// the column engines entirely and attach right beside their host, so their
	// edge is one card-length long instead of spanning columns. The host must
	// be a workload card with 2+ links: a lone app behind the proxy is not a
	// satellite of the proxy, and a pure pair (app + its only db) stays a
	// two-card island rather than orbiting itself.
	satHost := map[string]string{}
	var spine []int
	needLeft := false
	for _, i := range connected {
		id := nodes[i].ID
		if nb := adj[id]; len(nb) == 1 {
			if hi, ok := idx[nb[0]]; ok && !isSystem(nodes[hi]) && len(adj[nb[0]]) >= 2 {
				satHost[id] = nb[0]
				if seen[pair{id, nb[0]}] { // points into its host (a trigger): attaches left
					needLeft = true
				}
				continue
			}
		}
		spine = append(spine, i)
	}

	// attachSats places every not-yet-placed satellite whose host is in the
	// given set, and reports the lowest Y it used. Satellites cycle around
	// their host (right, tucked below-right, under, above-right) instead of
	// stacking on one side, where the chord to the second would pass under
	// the first (the hand-layout lesson: use whichever side is free). Called
	// per island by the cluster engine so each band reserves its room, and
	// once by the flow engine.
	gap := float64(colGap)
	if style == ArrangeClusters {
		gap = localGap
	}
	satIDs := make([]string, 0, len(satHost))
	for id := range satHost {
		satIDs = append(satIDs, id)
	}
	sort.Strings(satIDs)
	perHost := map[string]int{}
	attachSats := func(hosts map[string]bool) float64 {
		low := float64(rowStart)
		for _, id := range satIDs {
			if placed[id] || !hosts[satHost[id]] {
				continue
			}
			host := nodes[idx[satHost[id]]]
			hostH := CardH + float64(len(host.Subtiles))*30
			k := perHost[satHost[id]]
			perHost[satHost[id]]++
			var offs [][2]float64
			if seen[pair{id, satHost[id]}] { // trigger: left first, then in-and-up
				offs = [][2]float64{{-gap, 0}, {-gap / 2, -rowGap}, {0, -(CardH + 60)}, {-gap, rowGap}}
			} else { // read: right first, then in-and-down so chords never shadow
				offs = [][2]float64{{gap, 0}, {gap / 2, rowGap}, {0, hostH + 60}, {gap, -rowGap}}
			}
			off := offs[k%len(offs)]
			x, y := snap(host.X+off[0]), snap(host.Y+off[1])
			for collides(x, y, id) {
				y = snap(y + rowGap)
			}
			nodes[idx[id]].X, nodes[idx[id]].Y = x, y
			placed[id] = true
			if h := y + CardH + float64(len(nodes[idx[id]].Subtiles))*30; h > low {
				low = h
			}
		}
		return low
	}

	var maxY float64
	if style == ArrangeFlow {
		maxY = flowLayout(nodes, spine, needLeft, layer, settle)
		all := map[string]bool{}
		for id := range satHost {
			all[satHost[id]] = true
		}
		if y := attachSats(all); y > maxY {
			maxY = y
		}
	} else {
		maxY = clusterLayout(nodes, spine, connected, adj, needLeft, layer, settle, snap, attachSats)
	}
	// catch-all: satellites whose host was hand-anchored or otherwise
	// placed outside the engines
	{
		all := map[string]bool{}
		for id := range satHost {
			all[satHost[id]] = true
		}
		if y := attachSats(all); y > maxY {
			maxY = y
		}
	}

	for _, n := range nodes {
		if placed[n.ID] && n.Y > maxY {
			maxY = n.Y
		}
	}

	// park unconnected cards in a row below everything, with a visual gap
	if len(orphans) > 0 {
		byName(orphans)
		y := snap(maxY + rowGap*1.5)
		x := float64(colSystemX + colGap)
		for k, i := range orphans {
			if k > 0 && k%4 == 0 { // wrap so a pile of orphans stays a block
				x = colSystemX + colGap
				y = snap(y + rowGap)
			}
			for collides(x, y, nodes[i].ID) {
				y = snap(y + rowGap)
			}
			nodes[i].X = x
			nodes[i].Y = y
			placed[nodes[i].ID] = true
			x += CardW + CardGapX + 20
		}
	}

	// Re-center the system cards on their fan: they were settled first (top
	// of the column), but a proxy wired to six islands reads best at the
	// vertical middle of them, the fan spreads evenly instead of the bottom
	// chords slicing through every island above.
	for _, i := range system {
		if d, ok := desired(nodes[i].ID); ok {
			y := snap(d)
			for collides(nodes[i].X, y, nodes[i].ID) {
				y = snap(y + rowGap)
			}
			nodes[i].Y = y
		}
	}

	// Polish pass: nudge cards until no edge passes under a card it doesn't
	// touch. The engines place cards well enough that a handful of greedy
	// single-card moves closes the gap; hand-tuning proved the moves that
	// matter are exactly these, sidestep off a lane, or slide along it.
	nudgeOffEdges(nodes, links, idx, placed, isSystem, snap)
}

// segHitsRect reports whether the segment (x1,y1)-(x2,y2) crosses the given
// rectangle (Liang-Barsky clip).
func segHitsRect(x1, y1, x2, y2, rx, ry, rw, rh float64) bool {
	dx, dy := x2-x1, y2-y1
	t0, t1 := 0.0, 1.0
	clip := func(p, q float64) bool {
		if p == 0 {
			return q >= 0
		}
		r := q / p
		if p < 0 {
			if r > t1 {
				return false
			}
			if r > t0 {
				t0 = r
			}
		} else {
			if r < t0 {
				return false
			}
			if r < t1 {
				t1 = r
			}
		}
		return true
	}
	return clip(-dx, x1-rx) && clip(dx, rx+rw-x1) && clip(-dy, y1-ry) && clip(dy, ry+rh-y1)
}

// nudgeOffEdges greedily relocates auto-placed cards while any edge passes
// under a card that is not one of its endpoints. Card boxes are inflated by a
// margin so the client's curved edges, which bow off the straight line the
// server can test, stay clear too. Saved and system cards never move.
// greedy with a fixed candidate set, it can park on a local
// optimum; a real search (annealing) is the upgrade if topologies show up
// that it can't clean.
func nudgeOffEdges(nodes []Node, links []pair, idx map[string]int, placed map[string]bool, isSystem func(Node) bool, snap func(float64) float64) {
	height := func(i int) float64 { return CardH + float64(len(nodes[i].Subtiles))*30 }
	// segments between placed endpoints, card centers as the proxy line
	type seg struct{ a, b int }
	var segs []seg
	for _, l := range links {
		if placed[l.from] && placed[l.to] {
			segs = append(segs, seg{idx[l.from], idx[l.to]})
		}
	}
	crossed := func(s seg, i int) bool {
		if i == s.a || i == s.b {
			return false
		}
		ax, ay := nodes[s.a].X+CardW/2, nodes[s.a].Y+height(s.a)/2
		bx, by := nodes[s.b].X+CardW/2, nodes[s.b].Y+height(s.b)/2
		// A small fixed margin only: the client's curves bow off this straight
		// chord and a large margin would catch those too, but it also makes
		// every near slot look occupied, the greedy then fixes crossings by
		// flinging cards far away, and a compact layout with a rare grazing
		// curve reads far better than a sprawled one with none.
		const m = 12.0
		return segHitsRect(ax, ay, bx, by,
			nodes[i].X-m, nodes[i].Y-m, CardW+2*m, height(i)+2*m)
	}
	crossings := func() int {
		n := 0
		for _, s := range segs {
			for i := range nodes {
				if placed[nodes[i].ID] && crossed(s, i) {
					n++
				}
			}
		}
		return n
	}
	// One crossing costs as much as ~400px of extra edge: a short slide that
	// clears an edge is worth it, flinging a card across the canvas is not.
	// The length term is what keeps the result compact, the first version
	// scored crossings alone and "solved" them by scattering the layout.
	score := func() float64 {
		total := 0.0
		for _, sg := range segs {
			dx := nodes[sg.a].X - nodes[sg.b].X
			dy := nodes[sg.a].Y - nodes[sg.b].Y
			total += dx*dx + dy*dy
		}
		// compare lengths on a sqrt-free scale that still orders sensibly
		return float64(crossings())*400*400 + total/64
	}
	overlapsAny := func(i int, x, y float64) bool {
		for j := range nodes {
			if j == i || !placed[nodes[j].ID] {
				continue
			}
			if x < nodes[j].X+CardW+CardGapX && nodes[j].X < x+CardW+CardGapX &&
				y < nodes[j].Y+height(j)+CardGapY && nodes[j].Y < y+height(i)+CardGapY {
				return true
			}
		}
		return false
	}

	var movable []int
	for i := range nodes {
		if placed[nodes[i].ID] && !nodes[i].Saved && !isSystem(nodes[i]) {
			movable = append(movable, i)
		}
	}
	sort.Slice(movable, func(a, b int) bool { return nodes[movable[a]].ID < nodes[movable[b]].ID })

	offsets := [][2]float64{
		{0, -rowGap}, {0, rowGap}, {-localGap, 0}, {localGap, 0},
		{-localGap, -rowGap}, {localGap, -rowGap}, {-localGap, rowGap}, {localGap, rowGap},
		{0, -2 * rowGap}, {0, 2 * rowGap}, {-2 * localGap, 0}, {2 * localGap, 0},
		{-localGap, 2 * rowGap}, {localGap, 2 * rowGap}, {0, -3 * rowGap}, {0, 3 * rowGap},
	}
	const minX = colSystemX + colGap - 80 // just right of the divider, like a hand drag

	moves := map[int]int{}

	// Only engage when an edge really crosses a card (no margin): the
	// margined score used below also counts near-misses, and chasing those
	// on an already-clean canvas just shuffles cards for nothing.
	raw := 0
	for _, sg := range segs {
		for i := range nodes {
			if !placed[nodes[i].ID] || i == sg.a || i == sg.b {
				continue
			}
			ax, ay := nodes[sg.a].X+CardW/2, nodes[sg.a].Y+height(sg.a)/2
			bx, by := nodes[sg.b].X+CardW/2, nodes[sg.b].Y+height(sg.b)/2
			if segHitsRect(ax, ay, bx, by, nodes[i].X, nodes[i].Y, CardW, height(i)) {
				raw++
			}
		}
	}
	if raw == 0 {
		return
	}

	best := score()
	for pass := 0; pass < 40 && crossings() > 0; pass++ {
		improved := false
		for _, i := range movable {
			// a card that keeps getting shoved is the score chasing noise,
			// cap it so the layout stays recognisable
			if moves[i] >= 4 {
				continue
			}
			// only bother with cards involved in a violation, as the crossed
			// card or as an endpoint of the crossing edge
			involved := false
			for _, s := range segs {
				for j := range nodes {
					if placed[nodes[j].ID] && crossed(s, j) && (j == i || s.a == i || s.b == i) {
						involved = true
					}
				}
			}
			if !involved {
				continue
			}
			// best-of-candidates, not first-improvement: a greedy first pick
			// happily trades one violation for another and stalls
			ox, oy := nodes[i].X, nodes[i].Y
			bx, by, bs := ox, oy, best
			for _, off := range offsets {
				x, y := snap(ox+off[0]), snap(oy+off[1])
				if x < minX || overlapsAny(i, x, y) {
					continue
				}
				nodes[i].X, nodes[i].Y = x, y
				if sc := score(); sc < bs-0.5 {
					bx, by, bs = x, y, sc
				}
			}
			nodes[i].X, nodes[i].Y = bx, by
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

// flowLayout: column = layer depth. System cards took depth 0 (the left
// column), so every workload card starts at 1, or 2 when trigger satellites
// need the first workload column as their left-side parking. Columns settle
// left to right, each card at the average height of the neighbors already
// placed, which is the cheap half of a barycenter pass and kills most
// crossings.
func flowLayout(nodes []Node, connected []int, needLeft bool, layer func(map[string]bool) map[string]int, settle func([]int, float64, float64) float64) float64 {
	members := map[string]bool{}
	for _, i := range connected {
		members[nodes[i].ID] = true
	}
	base := 1
	if needLeft {
		base = 2
	}
	depth := layer(members)
	cols := map[int][]int{}
	for _, i := range connected {
		d := depth[nodes[i].ID] + base
		cols[d] = append(cols[d], i)
	}
	var order []int
	for d := range cols {
		order = append(order, d)
	}
	sort.Ints(order)
	var maxY float64
	for _, d := range order {
		if y := settle(cols[d], colSystemX+float64(d)*colGap, rowStart); y > maxY {
			maxY = y
		}
	}
	return maxY
}

// clusterLayout: islands. Connected components of the workload graph, minus
// hubs, a hub is a card whose removal splits its neighbors into 3+ separate
// groups (a shared instance, a vars card everyone reads). Hubs get their own
// column on the left; each island lays out as a small left-to-right flow in
// its own horizontal band.
func clusterLayout(nodes []Node, spine, full []int, adj map[string][]string, needLeft bool, layer func(map[string]bool) map[string]int, settle func([]int, float64, float64) float64, snap func(float64) float64, attachSats func(map[string]bool) float64) float64 {
	// Two graphs: hubs are detected on the full workload graph (a shared
	// instance welds app+satellite groups, and the satellites are what give
	// those groups their size), islands are built from the spine only.
	fullSet, spineSet := map[string]bool{}, map[string]bool{}
	for _, i := range full {
		fullSet[nodes[i].ID] = true
	}
	for _, i := range spine {
		spineSet[nodes[i].ID] = true
	}

	// components of the given member set with `without` excluded
	components := func(inSet map[string]bool, without map[string]bool) map[string]int {
		comp := map[string]int{}
		next := 0
		var ids []string
		for id := range inSet {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			if without[id] {
				continue
			}
			if _, done := comp[id]; done {
				continue
			}
			queue := []string{id}
			comp[id] = next
			for len(queue) > 0 {
				cur := queue[0]
				queue = queue[1:]
				for _, nb := range adj[cur] {
					if !inSet[nb] || without[nb] {
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

	// hub detection: highest degree first, so the biggest weld is tested
	// before the groups it created are attributed to someone else
	deg := func(id string) int {
		n := 0
		for _, nb := range adj[id] {
			if fullSet[nb] {
				n++
			}
		}
		return n
	}
	var candidates []string
	for _, i := range spine {
		if deg(nodes[i].ID) >= 3 {
			candidates = append(candidates, nodes[i].ID)
		}
	}
	sort.Slice(candidates, func(a, b int) bool {
		if deg(candidates[a]) != deg(candidates[b]) {
			return deg(candidates[a]) > deg(candidates[b])
		}
		return candidates[a] < candidates[b]
	})
	hubs := map[string]bool{}
	for _, id := range candidates {
		without := map[string]bool{id: true}
		for h := range hubs {
			without[h] = true
		}
		comp := components(fullSet, without)
		size := map[int]int{}
		for _, c := range comp {
			size[c]++
		}
		// Only groups of 2+ count: an app with three leaf satellites (db,
		// bucket, cron) is an island's center, not a hub between islands.
		groups := map[int]bool{}
		for _, nb := range adj[id] {
			if c, ok := comp[nb]; ok && size[c] >= 2 {
				groups[c] = true
			}
		}
		if len(groups) >= 3 {
			hubs[id] = true
		}
	}

	comp := components(spineSet, hubs)
	byComp := map[int][]int{}
	for _, i := range spine {
		if c, ok := comp[nodes[i].ID]; ok {
			byComp[c] = append(byComp[c], i)
		}
	}
	// biggest island first, ties broken by their smallest node id
	type cluster struct {
		members []int
		key     string
	}
	var clusters []cluster
	for _, members := range byComp {
		key := nodes[members[0]].ID
		for _, i := range members {
			if nodes[i].ID < key {
				key = nodes[i].ID
			}
		}
		clusters = append(clusters, cluster{members, key})
	}
	sort.Slice(clusters, func(a, b int) bool {
		if len(clusters[a].members) != len(clusters[b].members) {
			return len(clusters[a].members) > len(clusters[b].members)
		}
		return clusters[a].key < clusters[b].key
	})

	// Hubs get the column between the system zone and the islands, trigger
	// satellites the one after that; islands shift right only for columns
	// that actually exist.
	baseX := float64(colSystemX + colGap)
	var hubIdx []int
	if len(hubs) > 0 {
		for _, i := range spine {
			if hubs[nodes[i].ID] {
				hubIdx = append(hubIdx, i)
			}
		}
		baseX += localGap
	}
	if needLeft {
		baseX += localGap
	}

	bandY, maxY := float64(rowStart), float64(rowStart)
	for ci, cl := range clusters {
		// stagger the bands into a staircase: a straight left wall of islands
		// puts every card in the proxy fan's path; each band indents a half
		// step so the chords to lower bands clear the ones above
		indent := float64(ci) * (localGap / 2)
		members := map[string]bool{}
		for _, i := range cl.members {
			members[nodes[i].ID] = true
		}
		depth := layer(members)
		colsOf := map[int][]int{}
		for _, i := range cl.members {
			d := depth[nodes[i].ID]
			colsOf[d] = append(colsOf[d], i)
		}
		var order []int
		for d := range colsOf {
			order = append(order, d)
		}
		sort.Ints(order)
		bandMax := bandY
		for _, d := range order {
			if y := settle(colsOf[d], baseX+indent+float64(d)*localGap, bandY); y > bandMax {
				bandMax = y
			}
		}
		// this island's satellites join it now, so the next band starts below them
		if y := attachSats(members); y > bandMax {
			bandMax = y
		}
		if bandMax > maxY {
			maxY = bandMax
		}
		bandY = snap(bandMax + rowGap*1.5)
	}

	// hubs settle beside the islands they serve, vertically at the average
	// of their neighbors, settle() does exactly that once islands are placed
	if len(hubIdx) > 0 {
		if y := settle(hubIdx, colSystemX+colGap, rowStart); y > maxY {
			maxY = y
		}
		hubSet := map[string]bool{}
		for _, i := range hubIdx {
			hubSet[nodes[i].ID] = true
		}
		if y := attachSats(hubSet); y > maxY {
			maxY = y
		}
	}
	return maxY
}

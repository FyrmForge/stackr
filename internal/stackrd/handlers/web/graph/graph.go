// Package graph derives a project topology (nodes + edges) from apps and
// databases. No HTTP and no DB driver: it maps records to cards, so it is
// trivially unit-testable. It does read the engine registry
// (internal/databases) for per-engine display facts, the noun a slice wears,
// its drawer route, rather than keeping a second copy of them here.
// Edges are inferred the way Railway infers them: a service that
// references another service's address is connected to it. The references
// themselves come from the resolver (refs), which knows exactly what each tile
// reads, the canvas doesn't guess.
package graph

import (
	"database/sql"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/deploy"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/managedtiles"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// runLine formats a cron card's last-run display: "ok · Jul 24 13:16".
func runLine(status string, at sql.NullTime) string {
	if !at.Valid {
		return "never run"
	}
	label := "failed"
	switch status {
	case "ok":
		label = "ok"
	case "stopped":
		// someone hit Stop; not a failure, and the card must not say so
		label = "stopped"
	}
	return label + " · " + at.Time.Local().Format("Jan 2 15:04")
}

// MarkRunning overwrites the last-run line on the cron and function cards
// whose tile has a run in flight. A post-pass rather than another Build
// argument: Build stays a pure function of the rows it is handed, and the
// runs table is not one of them.
func MarkRunning(g *Graph, open []repo.CronRun) {
	if len(open) == 0 {
		return
	}
	byNode := make(map[string]repo.CronRun, len(open))
	for _, r := range open {
		if id, ok := strings.CutPrefix(r.Ref, "app:"); ok {
			byNode[AppNodeID(id)] = r
		}
	}
	for i := range g.Nodes {
		if r, ok := byNode[g.Nodes[i].ID]; ok {
			g.Nodes[i].LastRun = "running · since " + r.StartedAt.Local().Format("15:04")
		}
	}
}

// nextLine formats a cron card's next-run display: "next 13:18".
func nextLine(expr string) string {
	sched, err := cron.ParseStandard(expr)
	if err != nil {
		return ""
	}
	return "next " + sched.Next(time.Now()).Format("15:04")
}

// NodeKind distinguishes cards on the canvas.
type NodeKind string

const (
	KindApp      NodeKind = "app"
	KindManaged  NodeKind = "managed"
	KindProxy    NodeKind = "proxy"    // synthetic: the managed Traefik ingress
	KindHost     NodeKind = "host"     // synthetic: ports published on the host
	KindCron     NodeKind = "cron"     // a tile that runs on a schedule
	KindFunction NodeKind = "function" // run-to-completion tile: manual / on-deploy trigger
	KindVolume   NodeKind = "volume"   // persistent storage, stacked under its service
	KindRef      NodeKind = "ref"      // read-only ghost: a shared db instance living in another env
	// KindResource is one logical slice, a database or bucket provisioned out
	// of an instance. It has no container of its own, but it does have its own
	// credentials and its own consumers, so it is its own card: the instance
	// tile is where it lives, not what an app talks to.
	KindResource NodeKind = "resource"
	// Higher-level canvases: one card per environment on a stack graph, one
	// per stack on an org graph, one per organization at the root. Clicking
	// them navigates a level down.
	KindEnv   NodeKind = "env"
	KindStack NodeKind = "stack"
	KindOrg   NodeKind = "org"
	// KindVars is one card standing for a scope's variables, the canvas shows
	// that they exist and links to the editor, it is not an editor itself.
	KindVars NodeKind = "vars"
	// KindConnector is an external integration owned by an org (a GitHub app
	// install). Its own kind so it doesn't inherit the shared-instance card.
	KindConnector NodeKind = "connector"
	// KindForward is a live port-forward relay: one card per proxyrelay
	// container, listing who is tunnelled in. Ephemeral, it exists exactly as
	// long as the container does.
	KindForward NodeKind = "forward"
)

// Synthetic node IDs (stable so drag positions persist).
const (
	ProxyNodeID = "proxy:traefik"
	HostNodeID  = "host:ports"
)

// Node is one card on the canvas.
type Node struct {
	ID     string   `json:"id"` // "app:<uuid>" | "db:<uuid>"
	Kind   NodeKind `json:"kind"`
	Name   string   `json:"name"`
	Detail string   `json:"detail"` // source type / engine
	Status string   `json:"status"`
	// Waiting names the value a deploy stopped for: the config declares it and
	// nobody has set it. Only ever set alongside Status "waiting".
	Waiting string `json:"waiting,omitempty"`

	// Engine is the technology behind the card: the storage engine of a db /
	// slice / ghost ("postgres", "s3", …) or a connector's provider
	// ("github"). Carried separately from Detail so a card can pick its icon
	// without parsing its own subtitle, which is display copy and gets
	// reworded.
	Engine string `json:"engine,omitempty"`

	Href string  `json:"href"`
	X    float64 `json:"x"`
	Y    float64 `json:"y"`

	// Nav marks a card whose Href is a page to open, not a drawer fragment:
	// drilling down a level (env, stack) or leaving the canvas entirely
	// (connectors, the variables card). Kind cannot decide this, connector
	// cards share KindRef with shared-instance ghosts, which do have drawers.
	Nav bool `json:"nav,omitempty"`

	// Position came from a saved drag, not the auto layout. The client uses
	// this to know when a canvas still has never-dragged cards: the first
	// drag then snapshots every position, so autoLayout's "anchor newcomers
	// beside a dragged neighbor" branch only ever fires for genuinely new
	// cards.
	Saved bool `json:"saved,omitempty"`

	// HostAccess marks a tile running privileged or with host devices, worth
	// a badge, because it is the one setting that pierces the container
	// boundary.
	HostAccess bool `json:"hostAccess,omitempty"`

	// UpdateAvailable: the registry watcher has seen a newer image digest
	// than what runs, the card wears a "new version" chip.
	UpdateAvailable bool `json:"updateAvailable,omitempty"`

	// Outside-world exposure: proxied domains (apps) or a published host
	// port (databases). Empty/zero = internal only.
	Domains      []string `json:"domains,omitempty"`
	ExternalPort int      `json:"externalPort,omitempty"`

	// DomainCount stands in for Domains on the org canvas: hostnames belong to
	// tiles, and a stack card that listed every one of them would be a wall of
	// text. The count says "reachable from outside", the level below says how.
	DomainCount int `json:"domainCount,omitempty"`

	// Named volumes mounted by the tile (Railway-style chip on the card).
	Volumes []string `json:"volumes,omitempty"`

	// Subtiles are drawn as tiles stacked under the card, narrower than it:
	// an attached volume under its service, a db's data volume, the managed
	// instance hosting a slice. Purely visual (not nodes, no edges) each
	// with a click-through to its own drawer.
	Subtiles []Subtile `json:"subtiles,omitempty"`

	// Cron cards: preformatted last-run and next-run display lines.
	LastRun string `json:"lastRun,omitempty"`
	NextRun string `json:"nextRun,omitempty"`

	// Live network rates (bytes/second) from the latest metric sample.
	RxBps float64 `json:"rxBps,omitempty"`
	TxBps float64 `json:"txBps,omitempty"`

	// Resource cards: how busy this logical database is and how big it is.
	// Read from the engine per database, not from the wire, bytes on a wire
	// belong to the instance's container and can't be split between slices.
	TxnRate   float64 `json:"txnRate,omitempty"`
	SizeBytes int64   `json:"sizeBytes,omitempty"`

	// Deck is how many layers to draw behind an env/stack card so it reads as
	// a literal stack of what's inside it. Capped at 2 by the builders, the
	// card says "3 tiles" in words, the layers only need to say "several".
	Deck int `json:"deck,omitempty"`

	// UI-staging marker: "pending" (a staged edit) or "delete" (staged for
	// teardown). Empty = no pending change. Set post-build from staged_changes.
	Staged string `json:"staged,omitempty"`

	// Color is the environment's colour as a CSS value (env cards only).
	Color string `json:"color,omitempty"`

	// Ephemeral marks a card that comes and goes without a page change (a
	// forward relay). graph.js excludes these from its node-count reload
	// check and creates/removes them from the status poll instead.
	Ephemeral bool `json:"ephemeral,omitempty"`

	// Forward cards: one badge per user with a forward open. Empty = the
	// relay is idling out its last 60 seconds.
	Forwards []ForwardUser `json:"forwards,omitempty"`

	// ForwardCount stands in for forward cards on the stack and org canvases,
	// the same way DomainCount stands in for Domains: the chip says someone is
	// tunnelled in here, the level below says who.
	ForwardCount int `json:"forwardCount,omitempty"`

	// Placement (docs/plans/32-multi-node-ui.md, canvas). Node is where this
	// card's first replica runs, shown as a chip only when the swarm has more
	// than one node, a single-node install looks exactly as it did. Home is
	// set on a pinned tile, and the chip then carries a house mark: its
	// volume is on that machine's disk and the tile cannot move without the
	// data moving with it.
	Node string `json:"node,omitempty"`
	Home bool   `json:"home,omitempty"`

	// MovingTo is the node a volume move is copying towards, and MovePct how
	// far it has got. The chip reads old to new for the whole move and a thin
	// bar runs along the bottom of the card; the status chip stays whatever it
	// is, because the tile really is still running until the final sync.
	MovingTo string `json:"movingTo,omitempty"`
	MovePct  int    `json:"movePct,omitempty"`

	// Replicas is the roll-up over a stateless tile's tasks: "running · 3/3",
	// or amber "degraded · 2/3" when any replica is failed or starting. Zero
	// means the tile has one replica and the card is simply the tile.
	Replicas Replicas `json:"replicas,omitzero"`
}

// Replicas is a stateless tile's task roll-up. The card is replica 1; every
// replica above it is dealt underneath as a subtile.
type Replicas struct {
	// Want is the configured count, Running how many are actually up.
	Want, Running int
	// Rows are replicas 2 and up, in slot order, capped by the canvas.
	Rows []ReplicaRow
	// Hidden is how many did not fit, and HiddenWord summarises them in one
	// word ("all running", "1 starting", "2 failed").
	Hidden     int
	HiddenWord string
}

// Degraded reports whether any replica is not running. Amber, not red: the
// tile is up, just not all of it.
func (r Replicas) Degraded() bool { return r.Want > 0 && r.Running < r.Want }

// ReplicaRow is one dealt replica under the card.
type ReplicaRow struct {
	Slot   int    `json:"slot"`
	State  string `json:"state"`
	Age    string `json:"age"`
	Node   string `json:"node"`
	TaskID string `json:"taskId"`
}

// Subtile is one tile stacked under a card: name on the left, detail on the
// right, an icon picked by Kind ("volume" or an engine name), and the drawer
// its click opens. One visual component serves every flavour, a sub-tile is
// a sub-tile, whatever it stands for.
type Subtile struct {
	Kind   string `json:"kind"` // "volume", or the engine of a managed instance
	Name   string `json:"name"`
	Detail string `json:"detail,omitempty"`
	Href   string `json:"href"` // drawer fragment base; the client opens Href+"/panel"
}

// Edge connects two nodes. Kind styles the line: "" (app -> db reference),
// "ingress" (proxy -> app), "port" (host -> db).
type Edge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind,omitempty"`
}

// Graph is the full topology for a project.
type Graph struct {
	Nodes []Node `json:"nodes"`
	Edges []Edge `json:"edges"`
	// Annotations are the canvas's shared notes (text labels, boxes). Loaded by
	// the page handler after Build*, they belong to the canvas, not the
	// topology, so no Build function computes them.
	Annotations []repo.Annotation `json:"annotations,omitempty"`
	// Groups tie cards/annotations together so they move as one. Loaded the
	// same way as Annotations.
	Groups []repo.GraphGroup `json:"groups,omitempty"`
}

// AppNodeID / DBNodeID build the stable node identifiers.
func AppNodeID(id string) string { return "app:" + id }
func DBNodeID(id string) string  { return "db:" + id }

// ResourceNodeID is the stable node id for a provisioned logical slice.
func ResourceNodeID(id string) string { return "resource:" + id }

// ScopeSuffix names a tile's sharing scope for card subtitles. The canvas is
// the one place scope was invisible: an env-only instance and an org-wide one
// drew identical cards.
func ScopeSuffix(scopeKind string) string {
	switch scopeKind {
	case "stack":
		return "stack-scoped"
	case "org":
		return "org-scoped"
	}
	return "env-scoped" // the write-side default (sqlite/tiles.go)
}

// resourceKind names what a slice actually is on its card. Object storage
// hands out buckets rather than databases, and the distinction matters when
// the two sit side by side on one canvas, so the noun comes from the engine
// registry rather than from a switch here.
func resourceKind(engine string) string {
	if n := managedtiles.Engines[engine].SliceNoun; n != "" {
		return n
	}
	return engine
}

// SliceDomains decides which of the hosting instance's domains a slice card
// should wear.
//
// Every slice used to inherit them unconditionally, which said two wrong
// things at once: a private bucket advertised a public hostname, and a
// postgres logical database showed one even though the engine cannot sit
// behind an HTTP router at all. Only a publicly-readable slice of an
// HTTP-speaking engine is actually reachable at the instance's hostname, so
// only it keeps the chip. The published port is separate and stays, that one
// is real for every slice.
func SliceDomains(kind string, public bool, instanceDomains []string) []string {
	if !public || !managedtiles.Engines[kind].PublicSlices {
		return nil
	}
	return instanceDomains
}

// ResourceHref is the drawer a slice card opens: an engine with its own
// per-slice UI (an s3 object browser, a postgres data browser scoped to the
// slice's credentials) gets that; any other slice opens its instance.
func ResourceHref(kind, providerTileID, name string) string {
	if path := managedtiles.Engines[kind].SlicePath; path != "" {
		return "/dbs/" + providerTileID + "/" + path + "/" + name
	}
	return "/dbs/" + providerTileID
}

// Build assembles the graph. positions maps node ID -> (x, y); nodes without a
// saved position fall back to a deterministic auto-layout so the canvas is
// never empty-looking on first view.
// NetRates carries the latest per-tile network sample, keyed by tile ID.
type NetRates map[string][2]float64 // [rx, tx] bytes/second

// proxyNode is the Traefik card, identical at every level, the same reverse
// proxy serves an env, a stack and the whole org, so all three builders place
// this one card rather than three copies that could drift apart. Engine names
// the technology so the card can pick its brand icon, the same way a database
// card does.
func proxyNode(positions map[string][2]float64) Node {
	n := Node{ID: ProxyNodeID, Kind: KindProxy, Name: "Traefik", Detail: "reverse proxy",
		Engine: "traefik", Status: "running"}
	applyPosition(&n, positions)
	return n
}

// ForwardUser is one avatar badge on a forward card. Avatar is a ready URL
// ("" = initials fallback), resolved by the handler, since this package
// doesn't know where uploads are served from.
type ForwardUser struct {
	Name   string `json:"name"`
	Role   string `json:"role,omitempty"`
	Avatar string `json:"avatar,omitempty"`
}

// Forward is one live proxyrelay as the env canvas draws it: a tunnel into
// TileID on Port, with a badge per user who has a forward open.
type Forward struct {
	TileID string
	Port   int
	Users  []ForwardUser // empty = idling
}

// ForwardNodeID is stable per (tile, port) so a poll can diff cards by id.
func ForwardNodeID(tileID string, port int) string {
	return "forward:" + tileID[:8] + ":" + strconv.Itoa(port)
}

// AddForwards appends an ephemeral card per relay, placed left of its target
// tile's card with an edge to it. Runs after Build (and after autoLayout):
// these cards take no node_positions row and are not draggable, so their
// position is always derived from the target's. A forward into a tile with no
// card of its own (a sliced instance) still gets a card, parked at the left
// margin with no edge.
func AddForwards(g *Graph, fwds []Forward) {
	byID := map[string]*Node{}
	for i := range g.Nodes {
		byID[g.Nodes[i].ID] = &g.Nodes[i]
	}
	for i, f := range fwds {
		n := Node{
			ID:        ForwardNodeID(f.TileID, f.Port),
			Kind:      KindForward,
			Detail:    "port-forward",
			Ephemeral: true,
			Forwards:  f.Users,
			X:         colSystemX, Y: float64(rowStart + i*rowGap),
		}
		if len(f.Users) > 0 {
			n.Name = strconv.Itoa(len(f.Users)) + " forwarding"
			n.Status = "running"
		} else {
			n.Name = "port-forward"
			n.Status = "idle"
		}
		for _, target := range []string{AppNodeID(f.TileID), DBNodeID(f.TileID)} {
			if t, ok := byID[target]; ok {
				n.X, n.Y = t.X-300, t.Y
				g.Edges = append(g.Edges, Edge{From: n.ID, To: target, Kind: "forward"})
				break
			}
		}
		g.Nodes = append(g.Nodes, n)
	}
}

// TrafficPair is an observed flow between two graph nodes (conntrack).
type TrafficPair struct {
	From string  `json:"from"` // node ID
	To   string  `json:"to"`
	Bps  float64 `json:"bps"`
}

// MapTile points both sampler spellings of a tile at the same card. The
// sampler keys a tile "app:<id>" or "db:<id>" depending on repo.Tile.IsManaged, and
// a canvas rollup has no reason to re-derive that: claiming both is cheap and
// keeps a reclassified tile from silently losing its lane.
func MapTile(nodeOf map[string]string, tileID, node string) {
	nodeOf[AppNodeID(tileID)] = node
	nodeOf[DBNodeID(tileID)] = node
}

// WorstStatus collapses a group of tiles into the one light its card can show,
// an environment card standing for its tiles, a stack card for its
// environments. The most alarming status wins: a crash is what the operator
// needs to see.
//
// The ranking has to agree with what the card renderers call each status
// (nodeStatus in components/canvas/canvas.templ, statusSpan in
// static/js/graph.js), or the same tile reads one way zoomed out and another
// zoomed in. "done" ranks with "running" for exactly that reason: both render
// as Online, so a stack of finished cron tiles must not roll up to blank and
// draw itself "idle".
//
// Volumes are skipped, they have no lifecycle of their own to report.
func WorstStatus(tiles []repo.Tile) string {
	rank := map[string]int{"error": 4, "building": 3, "queued": 3, "unhealthy": 2, "running": 1, "done": 1}
	worst, out := 0, ""
	for i := range tiles {
		if tiles[i].IsVolume() {
			continue
		}
		if r := rank[tiles[i].Status]; r > worst {
			worst, out = r, tiles[i].Status
		}
	}
	return out
}

// RollupTraffic maps container-level flows onto a coarser canvas. pairs is the
// sampler snapshot ("app:<id>|db:<id>" -> bytes/sec); nodeOf renames each
// endpoint to the card standing for it at this level (a tile to its env card,
// an env's tiles to their stack card). Endpoints with no card drop out, flows
// whose ends land on the same card collapse away (an env talking to itself is
// not a line), and parallel flows sum. The JS side draws a lane only where the
// pair has an edge, so mapped-but-unconnected pairs cost nothing.
func RollupTraffic(pairs map[string]float64, nodeOf map[string]string) []TrafficPair {
	sum := map[[2]string]float64{}
	for k, bps := range pairs {
		from, to, ok := strings.Cut(k, "|")
		if !ok || bps <= 0 {
			continue
		}
		f, t := nodeOf[from], nodeOf[to]
		if f == "" || t == "" || f == t {
			continue
		}
		sum[[2]string{f, t}] += bps
	}
	out := make([]TrafficPair, 0, len(sum))
	for k, bps := range sum {
		out = append(out, TrafficPair{From: k[0], To: k[1], Bps: bps})
	}
	return out
}

// refs maps a consumer tile ID to what it references: tile IDs, and resource
// IDs for provisioned slices. Edges are drawn only for targets that are nodes
// on this canvas.
func Build(apps []repo.Tile, dbs []repo.Tile, resources []repo.ManagedResource, domains map[string][]string, positions map[string][2]float64, rates NetRates, traffic []TrafficPair, refs map[string][]string) Graph {
	g := Graph{}

	// An instance that hosts logical slices is represented by those slices,
	// not by a card of its own: the container is where the data lives, but
	// what an app talks to is one database with one set of credentials. Each
	// slice card carries the instance as a tile stacked underneath, a purely
	// visual strip naming it and its sharing scope, linking to its drawer.
	//
	// An instance that hosts nothing follows the tree instead: env-scoped ones
	// are this canvas's residents and draw a full card; stack- and org-scoped
	// ones are cards on the canvas one or two levels up and appear here only
	// as ghost references, the same dashed card a cross-env instance gets.
	resident, hostsSlices := map[string]bool{}, map[string]bool{}
	for _, d := range dbs {
		resident[d.ID] = d.ScopeKind == "" || d.ScopeKind == "env"
	}
	for _, r := range resources {
		hostsSlices[r.ProviderTileID] = true
	}
	// dbNode names the card standing for an instance on this canvas: its own
	// card when it has one, its ghost when it lives up the tree. A slice
	// host has neither, edges pointed at it simply don't render.
	dbNode := func(id string) string {
		if resident[id] || hostsSlices[id] {
			return DBNodeID(id)
		}
		return RefNodeID(id)
	}

	for _, d := range dbs {
		if !resident[d.ID] && !hostsSlices[d.ID] {
			n := Node{
				ID:           RefNodeID(d.ID),
				Kind:         KindRef,
				Name:         d.Name,
				Detail:       d.Engine + " · " + ScopeSuffix(d.ScopeKind),
				Engine:       d.Engine,
				Href:         "/dbs/" + d.ID,
				ExternalPort: d.ExternalPort,
				Domains:      domains[d.ID],
			}
			applyPosition(&n, positions)
			g.Nodes = append(g.Nodes, n)
			continue
		}
		if hostsSlices[d.ID] {
			continue // its slices stand in for it (and for its data volume)
		}
		n := Node{
			ID:           DBNodeID(d.ID),
			Kind:         KindManaged,
			Name:         d.Name,
			Detail:       d.Engine,
			Engine:       d.Engine,
			Status:       d.Status,
			Href:         "/dbs/" + d.ID,
			ExternalPort: d.ExternalPort,
			Domains:      domains[d.ID],

			UpdateAvailable: d.HasImageUpdate(),
		}
		if r, ok := rates[d.ID]; ok {
			n.RxBps, n.TxBps = r[0], r[1]
		}
		// The db's data volume rides under the card as a sub-tile (mirrors
		// managedtiles.VolumeName).
		n.Subtiles = append(n.Subtiles, Subtile{
			Kind: "volume", Name: "stackr-db-" + d.ID[:8], Detail: "data",
			Href: "/dbs/" + d.ID + "/volume", // opens the volume drawer, not the db's
		})
		applyPosition(&n, positions)
		g.Nodes = append(g.Nodes, n)
	}

	// One card per logical slice. The engine and the instance it lives on read
	// as the card's subtitle rather than as another node.
	instanceName := map[string]string{}
	instanceScope := map[string]string{}
	instancePort := map[string]int{}
	for _, d := range dbs {
		instanceName[d.ID] = d.Name
		instanceScope[d.ID] = ScopeSuffix(d.ScopeKind)
		instancePort[d.ID] = d.ExternalPort
	}
	for _, r := range resources {
		// The card wears the slice's SLUG, not its raw db/bucket name: the slug
		// is the tile's literal name, the config key (site-db) or the derived
		// instance-qualified one, and it is unique per environment, where names
		// are only unique per instance. A consumer named "site" with a db named
		// "site" and a bucket named "site" was three cards all saying "site".
		// The instance hosting it reads as the subtitle, and the sub-tile below
		// repeats the instance with its scope; the subtitle carries the host
		// alone so a slice whose instance is elsewhere still says where it
		// lives.
		detail := instanceName[r.ProviderTileID]
		if detail == "" {
			detail = resourceKind(r.Kind)
		}
		n := Node{
			ID:     ResourceNodeID(r.ID),
			Kind:   KindResource,
			Name:   r.Slug,
			Detail: detail,
			Engine: r.Kind,
			Status: r.Status,
			Href:   ResourceHref(r.Kind, r.ProviderTileID, r.Name),
			// A slice has no container of its own: whatever reaches it does so
			// through the instance hosting it, so it wears the instance's
			// exposure.
			ExternalPort: instancePort[r.ProviderTileID],
			Domains:      SliceDomains(r.Kind, r.Public, domains[r.ProviderTileID]),
		}
		if _, here := resident[r.ProviderTileID]; here {
			// The hosting instance rides under the slice card as a sub-tile:
			// name + scope, click-through to its drawer. Visual only.
			n.Subtiles = append(n.Subtiles, Subtile{
				Kind:   r.Kind,
				Name:   instanceName[r.ProviderTileID],
				Detail: instanceScope[r.ProviderTileID],
				Href:   "/dbs/" + r.ProviderTileID,
			})
		} else {
			// An instance in another environment keeps its ghost card (it is
			// not a tile of this canvas), so slices of it still hang off
			// something.
			g.Edges = append(g.Edges, Edge{From: n.ID, To: RefNodeID(r.ProviderTileID), Kind: "shared"})
		}
		applyPosition(&n, positions)
		g.Nodes = append(g.Nodes, n)
	}

	// Attached volume tiles ride under their service as sub-tiles; only a
	// detached volume (nothing to sit under) stays a card of its own.
	subVolumes := map[string][]Subtile{} // attached tile id -> its volume sub-tiles
	for _, a := range apps {
		if a.Kind == "volume" && a.AttachedTileID != "" {
			subVolumes[a.AttachedTileID] = append(subVolumes[a.AttachedTileID], Subtile{
				Kind: "volume", Name: a.Name, Detail: a.MountPath, Href: "/apps/" + a.ID,
			})
		}
	}
	for _, a := range apps {
		if a.Kind == "volume" && a.AttachedTileID != "" {
			continue // rides under its service as a sub-tile
		}
		n := Node{
			ID:      AppNodeID(a.ID),
			Kind:    KindApp,
			Name:    a.Name,
			Detail:  a.SourceType,
			Status:  a.Status,
			Href:    "/apps/" + a.ID,
			Domains: domains[a.ID],
			Volumes: namedVolumes(a.Volumes),

			HostAccess: a.Privileged || a.Devices != "",

			UpdateAvailable: a.HasImageUpdate(),

			Subtiles: subVolumes[a.ID],
		}
		// The deploy engine parks a tile whose config names a value nobody has
		// set as "waiting:<name>"; the card shows the name, not the raw status.
		if name := deploy.WaitingFor(a.Status); name != "" {
			n.Status, n.Waiting = "waiting", name
		}
		// Storage attachments ride under the consumer as chips, like volumes.
		for _, l := range strings.Split(a.Storage, "\n") {
			l = strings.TrimSpace(l)
			if l == "" {
				continue
			}
			name, detail := l, ""
			if i := strings.IndexByte(l, ':'); i > 0 {
				name, detail = l[:i], l[i+1:]
			}
			n.Subtiles = append(n.Subtiles, Subtile{Kind: "storage", Name: name, Detail: detail, Href: "/servers/local"})
		}
		if r, ok := rates[a.ID]; ok {
			n.RxBps, n.TxBps = r[0], r[1]
		}
		if a.Kind == "cron" {
			// standalone CronJob-style service: cron card, schedule as detail
			n.Kind = KindCron
			n.Detail = a.Cron
			n.Domains = nil
			n.LastRun = runLine(a.LastStatus, a.LastRunAt)
			if a.Status != "paused" {
				n.NextRun = nextLine(a.Cron)
			}
		}
		if a.Kind == "function" {
			n.Kind = KindFunction
			n.Detail = "manual"
			if a.RunOnDeploy {
				n.Detail = "on deploy"
			}
			n.Domains = nil
			n.LastRun = runLine(a.LastStatus, a.LastRunAt)
		}
		if a.Kind == "volume" {
			n.Kind = KindVolume
			n.Status = ""
			n.Domains = nil
			n.Volumes = nil
			n.Detail = "detached"
		}
		applyPosition(&n, positions)
		g.Nodes = append(g.Nodes, n)
		if a.Kind == "volume" {
			continue // no db-reference edges for storage
		}

		// Edges come from what the tile's variables actually reference.
		for _, target := range refs[a.ID] {
			for _, d := range dbs {
				if d.ID == target {
					g.Edges = append(g.Edges, Edge{From: AppNodeID(a.ID), To: dbNode(d.ID)})
				}
			}
			for _, r := range resources {
				if r.ID == target {
					g.Edges = append(g.Edges, Edge{From: AppNodeID(a.ID), To: ResourceNodeID(r.ID)})
				}
			}
			for _, other := range apps {
				if other.ID == target && other.Kind != "volume" {
					g.Edges = append(g.Edges, Edge{From: AppNodeID(a.ID), To: AppNodeID(other.ID)})
				}
			}
		}
		// Startup-order edges: dotted, dependent → dependency, per depends_on.
		// A pair that already has a reference line keeps that one line: a
		// tile nearly always references what it waits for, and two curves
		// between the same cards read as clutter, not as two facts.
		linked := func(from, to string) bool {
			for _, e := range g.Edges {
				if (e.From == from && e.To == to) || (e.From == to && e.To == from) {
					return true
				}
			}
			return false
		}
		for _, line := range strings.Split(a.DependsOn, "\n") {
			slug := strings.TrimSpace(line)
			if i := strings.IndexByte(slug, ':'); i >= 0 {
				slug = slug[:i]
			}
			if slug == "" {
				continue
			}
			for _, other := range apps {
				if other.Slug == slug && other.ID != a.ID && !linked(AppNodeID(a.ID), AppNodeID(other.ID)) {
					g.Edges = append(g.Edges, Edge{From: AppNodeID(a.ID), To: AppNodeID(other.ID), Kind: "startup"})
				}
			}
			for _, d := range dbs {
				if d.Slug == slug && !linked(AppNodeID(a.ID), dbNode(d.ID)) {
					g.Edges = append(g.Edges, Edge{From: AppNodeID(a.ID), To: dbNode(d.ID), Kind: "startup"})
				}
			}
		}
	}

	// Synthetic infrastructure nodes: the Traefik ingress in front of proxied
	// apps, and the host network in front of externally published DB ports.
	hasIngress := false
	for _, a := range apps {
		if len(domains[a.ID]) > 0 {
			hasIngress = true
			g.Edges = append(g.Edges, Edge{From: ProxyNodeID, To: AppNodeID(a.ID), Kind: "ingress"})
		}
	}
	// Managed instances speak HTTP too (RustFS behind a hostname). The proxy
	// points at whichever card stands for the instance: itself, or its slices.
	for _, d := range dbs {
		if len(domains[d.ID]) == 0 {
			continue
		}
		hasIngress = true
		if !hostsSlices[d.ID] {
			g.Edges = append(g.Edges, Edge{From: ProxyNodeID, To: dbNode(d.ID), Kind: "ingress"})
			continue
		}
		for _, r := range resources {
			if r.ProviderTileID == d.ID {
				g.Edges = append(g.Edges, Edge{From: ProxyNodeID, To: ResourceNodeID(r.ID), Kind: "ingress"})
			}
		}
	}
	if hasIngress {
		g.Nodes = append(g.Nodes, proxyNode(positions))
	}
	hasPorts := false
	for _, d := range dbs {
		if d.ExternalPort == 0 {
			continue
		}
		hasPorts = true
		if !hostsSlices[d.ID] {
			g.Edges = append(g.Edges, Edge{From: HostNodeID, To: dbNode(d.ID), Kind: "port"})
			continue
		}
		// The instance has no card of its own (its slices stand in for it) but
		// the port is published all the same, so every slice it hosts hangs off
		// the host node. Skipping it entirely hid a publicly reachable database.
		for _, r := range resources {
			if r.ProviderTileID == d.ID {
				g.Edges = append(g.Edges, Edge{From: HostNodeID, To: ResourceNodeID(r.ID), Kind: "port"})
			}
		}
	}
	if hasPorts {
		n := Node{ID: HostNodeID, Kind: KindHost, Name: "Host network", Detail: "published ports", Status: "running"}
		applyPosition(&n, positions)
		g.Nodes = append(g.Nodes, n)
	}

	// Observed traffic between nodes with no derived edge shows up as its
	// own dashed edge, auto-discovered dependencies from conntrack.
	seen := map[string]bool{}
	for _, e := range g.Edges {
		seen[e.From+"|"+e.To] = true
		seen[e.To+"|"+e.From] = true
	}
	ids := map[string]bool{}
	for _, n := range g.Nodes {
		ids[n.ID] = true
	}
	for _, t := range traffic {
		if !ids[t.From] || !ids[t.To] || seen[t.From+"|"+t.To] {
			continue
		}
		seen[t.From+"|"+t.To] = true
		seen[t.To+"|"+t.From] = true
		g.Edges = append(g.Edges, Edge{From: t.From, To: t.To, Kind: "traffic"})
	}

	// No layout here: the caller runs g.Arrange once every card is on the
	// canvas (references, var cards), so they all take part.
	return g
}

// namedVolumes extracts docker named volumes from "name:/path" mount lines
// (bind-mount paths starting with / or . are not volumes and are skipped).
func namedVolumes(lines string) []string {
	var out []string
	for _, l := range strings.Split(lines, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "/") || strings.HasPrefix(l, ".") {
			continue
		}
		if name, _, ok := strings.Cut(l, ":"); ok && name != "" {
			out = append(out, name)
		}
	}
	return out
}

// Reference is a shared db instance consumed by services in this env but
// living in another env, shown as a read-only ghost card that links to the
// instance's real home.
type Reference struct {
	InstanceID  string
	Name        string
	Detail      string // e.g. "postgres · managed"
	Engine      string // storage engine, for the card's icon
	Href        string
	ConsumerIDs []string // app tile ids in this env provisioning from it

	// The instance's own outside-world exposure, so a ghost card says as much
	// about reachability as the real card in its home environment.
	Domains      []string
	ExternalPort int
}

// RefNodeID is the stable node id for a shared-instance reference card.
func RefNodeID(instanceID string) string { return "ref:" + instanceID }

// AddReferences appends ghost cards for cross-env shared instances plus their
// edges to local consumers. Called after Build so consumer nodes already have
// their laid-out positions; each ghost parks just right of its first consumer
// (saved drag positions win). no force-layout, a secondary card
// beside its consumer reads clearly and stays a tiny, test-free addition.
func (g *Graph) AddReferences(refs []Reference, positions map[string][2]float64) {
	placed := map[string]Node{}
	for _, n := range g.Nodes {
		placed[n.ID] = n
	}
	for _, r := range refs {
		id := RefNodeID(r.InstanceID)
		n := Node{ID: id, Kind: KindRef, Name: r.Name, Detail: r.Detail, Engine: r.Engine, Href: r.Href,
			Domains: r.Domains, ExternalPort: r.ExternalPort}
		if p, ok := positions[id]; ok {
			n.X, n.Y = p[0], p[1]
			n.Saved = true
		} else if len(r.ConsumerIDs) > 0 {
			if c, ok := placed[AppNodeID(r.ConsumerIDs[0])]; ok {
				n.X, n.Y = c.X+260, c.Y
			}
		}
		g.Nodes = append(g.Nodes, n)
		for _, cid := range r.ConsumerIDs {
			g.Edges = append(g.Edges, Edge{From: AppNodeID(cid), To: id, Kind: "shared"})
		}
	}
}

func applyPosition(n *Node, positions map[string][2]float64) {
	if p, ok := positions[n.ID]; ok {
		n.X, n.Y = p[0], p[1]
		n.Saved = true
	}
}

// The canvas's geometry, declared once. Everything that places or measures a
// card reads these: the layout engines here, the card box in
// components/canvas/canvas.templ, and the client (dragging, the no-overlap
// nudge, the system-column wall), which is handed them as data attributes
// rather than repeating the numbers. They used to be spelled out in five
// places, and a Go test was pinned to a JS function by comment to keep two of
// them honest.
const (
	// CardW/CardH are the card box. CardGapX/CardGapY are the clear space a
	// placement keeps around one, so two cards never touch.
	CardW    = 220.0
	CardH    = 96.0
	CardGapX = 40.0
	CardGapY = 30.0
	// GridPx is the snap grid, and the dot spacing of the canvas background.
	GridPx = 22

	colSystemX = -280
	colGap     = 360
	rowStart   = 80
	rowGap     = 140
)

// overlaps reports whether a card at (x, y) would sit too close to one at
// (ox, oy), the one collision rule, shared by both layout engines.
func overlaps(x, y, ox, oy float64) bool {
	return x < ox+CardW+CardGapX && ox < x+CardW+CardGapX &&
		y < oy+CardH+CardGapY && oy < y+CardH+CardGapY
}

// placeColumns is the column sweep the stack and org canvases share: unsaved
// cards fill their kind's column top to bottom, stepping over any row a
// hand-dragged card already occupies. Without that step a new card can land
// exactly under one the user placed there and simply disappear.
func placeColumns(nodes []Node, positions map[string][2]float64, colX func(Node) float64) {
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
	idx := make([]int, 0, len(nodes))
	for i := range nodes {
		if _, saved := positions[nodes[i].ID]; saved {
			taken = append(taken, box{nodes[i].X, nodes[i].Y})
			continue
		}
		idx = append(idx, i)
	}
	sort.SliceStable(idx, func(a, b int) bool { return nodes[idx[a]].ID < nodes[idx[b]].ID })
	next := map[float64]float64{} // per column, not per kind: kinds can share one
	for _, i := range idx {
		n := &nodes[i]
		x := colX(*n)
		y := rowStart + next[x]*rowGap
		for !free(x, y) {
			next[x]++
			y = rowStart + next[x]*rowGap
		}
		n.X, n.Y = x, y
		next[x]++
		taken = append(taken, box{x, y})
	}
}

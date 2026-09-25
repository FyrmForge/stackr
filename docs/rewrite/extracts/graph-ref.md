Source: frontend/static/js/graph.js (1978 lines) plus internal/stackrd/handlers/web/ graph/{graph,layout,placement,stack,org,orgs,vars}.go, components/canvas/{canvas.templ,status.go}, handler/project/{graph.templ,stackgraph.go,stackgraph.templ,placement.go}, handler/org/{graph.go,graph.templ}, handler/annotate/{annotate,groups}.go; the graph handlers in handler/project/handler.go and account/handler.go (GraphPrefs)
Commit: c2423f0
Taken: what the canvas draws, every gesture and what it sends, the data shapes, the layout rule, and a mapping onto the rewrite's element rules
Cut: every line of JS, templ and Go; all CSS; the view-transition animations; the create modal and env-compare pill code that lived in graph.js
Cuts belong to: the new web layer, if the canvas ever comes back. REWRITE.md "Later (not v1)" lists "the canvas", and step 6 dropped the old canvas JS (PROGRESS.md). This file says how it would return, not v1 work.

Reference only. Prose, no code.

## 0. Shape of the old thing

One 1978-line IIFE in graph.js drives four canvas levels, each with its
own base URL `{base}`: home (all the viewer's orgs) `/graph`, org
`/orgs/:slug/graph`, stack `/projects/:id/graph`, env `/envs/:id/graph`.

The page is server-rendered. JS picks up the DOM, then owns geometry,
persistence, polling, the side drawer, the create modal and the env-compare
pill. It adds document-wide listeners for keydown, click, pointerdown,
mouseover and toggle.

## 1. What is drawn

### Node kinds (`graph.NodeKind`)

- **app**: a service tile.
- **cron** / **function**: detail is the schedule, or "manual" / "on
  deploy". Footer: last run ("ok · Jul 24 13:16", "never run", "running ·
  since 13:02"), plus next run for cron.
- **managed**: a managed instance, env-resident. Its data volume rides
  under it as a sub-tile.
- **resource**: one logical slice (a database or a bucket), labelled by
  slug. Its hosting instance rides under it as a sub-tile, and the
  instance's own card is suppressed. Footer shows txn/s and size.
- **ref**: a dashed ghost for an instance that lives up the tree or in
  another env. Footer: "managed tile, open home", or its exposure.
- **volume**: own card (short, 62 px) only when *detached*; attached
  volumes are sub-tiles under their tile.
- **proxy** (`proxy:traefik`), **host** (`host:ports`): dashed "system"
  cards in a left column behind a "server" divider; only when something is
  proxied or published.
- **vars / secrets** (`vars:<scope>`, `secrets:<scope>`): one card per
  class, showing a count only, never values. Opens the variables editor.
- **env** (stack canvas), **stack** (org canvas), **org** (home canvas):
  drill-down cards, drawn as a deck of 0 to 2 layers. The env card wears
  the env colour. Status is the worst-of roll-up over its tiles: error >
  building/queued > unhealthy > running/done.
- **connector** (org canvas): a GitHub install.
- **forward**: ephemeral port-forward relay with user avatars; not
  draggable, no drawer.

### What an ordinary card shows

- icon (engine brand or stroke glyph), name, detail line, kind chip; a
  red "host" chip (privileged or devices); a "new version" chip (image
  watcher saw a newer digest)
- a footer strip:
  - a status light: running/done = Online; error, unhealthy, building,
    queued; "waiting" names the missing param
  - or, when replicas > 1, the replica roll-up "running · 3/3", or amber
    "degraded · 2/3"
  - exposure: first domain as a link plus "+n", or "n domains" on upper
    levels, or the published port
  - a forward-count chip
- a volumes chip (first name plus "+n"); staged markers "edited" and
  "removing"; multi-node only, a node chip (house icon when home-pinned)
  and a move-progress bar
- **sub-tiles** (28 px down, 8 px across each; each opens its own
  drawer): volume, instance, storage share, replicas 2+ (cap 3, then a
  "+n more · 2 failed" row).

### Edge kinds (`data-edge-kind` on SVG paths)

| kind | meaning | look |
|---|---|---|
| ref (unkinded) | app reads a tile/slice via a param ref | strong grey, dashed 4 5; animated `edge-busy` when the target slice has txn/s > 0 |
| ingress | proxy → proxied card | accent, solid |
| port | host → published db/slice | amber, solid |
| shared | slice→ghost, consumer→ghost, vars→reader, stack→org instance | accent, dashed 5 4 |
| startup | depends_on, dependent → dependency; skipped if a ref line already joins the pair | amber, dotted 1 5, faint |
| traffic | conntrack flow with no declared edge | teal, dashed 3 7, faint |
| config / source | connector → stack (config-as-code / build source) | connector hue; solid vs dashed 4 6 |
| forward | forward card → target | CSS-animated teal |
| cron, volume | styled in templ and in the legend | **no builder read here emits them**: dead or leftover |

- One shared arrow marker, painted in the path's own stroke.
- Traffic "lanes" are JS clones of an edge, one per direction. Each is
  offset 4 px along the normal, has an animated pulse and a rate label
  ("→ 12.3 KB/s").
- The legend is built from the edge kinds actually visible.

The server draws plain curves. JS re-paths every edge on load:

- anchor side: left/right if the run is mostly horizontal, else top/bottom
- cubic curve, control offset max(half the run, 60), or straight
- optional fan-out: edges meeting one side of one card spread along it,
  sorted by where the far end sits, gap 14 px, within 60 % of the side

## 2. Interactions

| action | effect | server? |
|---|---|---|
| wheel with ctrl/meta (trackpad pinch) | smooth zoom about the cursor | no |
| mouse-wheel notch (whole deltaY ≥ 100, no deltaX) | zoom ×1.1 per notch | no |
| two-finger trackpad scroll | pan | no |
| drag on empty canvas | pan; also clears the selection and leaves an entered group | no |
| two-finger touch | pinch zoom plus pan about the midpoint; cancels a node drag and puts the cards back | no |
| zoom buttons + / − | ×1.2 about the centre | no |
| reset-view button | back to scale 0.8, fit content, 24 px margin | no |
| window resize | re-clamp pan | no |
| pan/zoom clamp | at least one card's worth of content stays on screen; scale 0.3 to 2.5 | no |
| shift+drag on canvas | marquee; selects any card or annotation it *intersects* | no |
| drag a card | moves the card, or the whole selection if the card was selected, or its group. 3 px slop (10 on touch). Optional snap to the 22 px grid. System cards and workload cards cannot cross the divider. Sub-tiles and edges follow live. | yes: a position save every 250 ms while moving, a full-canvas batch on first move if any card is unsaved, then a batch on drop |
| drop (single card, no-overlap on) | nudges to the nearest free spot, ring search up to 12 rings | as above |
| click a card | env/stack/org card: a real `<a>` navigation, modifier-click works. Other cards: open the right drawer with `Href/panel`. A click right after a drag is swallowed. | yes: drawer GET via htmx.ajax |
| Enter / Space on a focused card | same as click | same |
| click a sub-tile | opens that sub-tile's drawer | yes: GET |
| click a domain on a card | opens https://domain in a new tab | no |
| hover a card | lights its visible edges, lanes and neighbours; dims the rest (off while dragging) | no |
| `?focus=<node id>` in the URL | centres and lights that card (search lands here) | no |
| double-click a grouped card | enters the group so one member moves alone; closes the drawer | no |
| Escape | clears selection, leaves group, hides menu, closes create modal, closes drawers, exits annotation mode | no |
| right-click | menu: Group selection (≥2 selected), Ungroup (on a grouped item), Add label here / Add box here (on empty canvas) | group, ungroup, add: yes |
| Ctrl+G / Ctrl+Shift+G | group selection / ungroup selected | yes |
| Ctrl+Z / Ctrl+Shift+Z | undo/redo positions only; 50 steps, in memory | yes: re-posts the restored positions |
| note / box buttons, then click or drag | text note: placed and edited in place (contentEditable). Box: drag out, minimum 40×30. | yes: annotation save, returns an id |
| annotation drag / resize grip / double-click to edit / × | move (snaps, joins selection or group), resize a box, edit text, delete. An emptied text note is deleted. | yes |
| "View" drawer checkboxes/radios | toggle a view pref, re-apply | yes: prefs PUT |
| "Re-arrange" button | PUTs prefs, POSTs reset, then `location.reload()` | yes |
| drawer backdrop / close | close drawer | no |
| drill-down navigation | cross-document view transition: cards "deal out" from the clicked card; going up "gathers" them back. Uses sessionStorage. | no |

- There is **no collapse** feature. The closest things are group/enter
  and the deck look on env/stack cards.
- The "View" toggles are: snap, system nodes, exposure badges, server
  divider, reference edges, startup edges, legend, traffic, curved or
  straight edges, fan-out, arrows, no-overlap, hover focus, and the arrange
  style (clusters or flow).

## 3. Data in

**No JSON graph load.** The first paint is server HTML:

- card divs placed by a generated `<style>` of per-node left/top/width/height
  rules. JS copies these onto `el.style` at init.
- SVG paths carrying `data-edge-from/to/kind`.
- attributes on the viewport: card width/height, gaps, grid px, save URL,
  CSRF token, ws room.
- two JSON script tags: annotations `{id, kind: text|box, body, x, y, w, h,
  color}` and groups `{id, members: ["node:<id>" | "anno:<id>"]}`.

Node ids are `app:<uuid>`, `db:<uuid>`, `resource:<uuid>`, `ref:<uuid>`,
`env:<slug>`, `stack:<slug>`, `org:<uuid>`, `connector:<id>`, `vars:<scope>`,
`secrets:<scope>`, `forward:<tile8>:<port>`, `proxy:traefik`, `host:ports`.

**Live refresh:** `GET {base}/status`. It is not on a timer. It runs when:

- `live.js` fires `stackr:ws`, from the websocket room `project:<id>` or the
  org room; the extra `flows` room ticks every few seconds for traffic
- the tab becomes visible again

Only one fetch runs at a time, and none while the tab is hidden.

The response is `{nodes: [StatusNode], traffic: [{from, to, bps}]}`.
StatusNode is the full `graph.Node` (id, kind, name, detail, status, x, y,
saved, domains, replicas, …) plus:

- `footer`: pre-rendered HTML for the card's bottom strip, swapped in only
  when it changed
- for ephemeral cards: `html`, `class` and `height`, so JS can create them

Client handling: a non-ephemeral count mismatch or a missing id means a
full `location.reload()`. Otherwise it moves cards to remote positions
(not ones being dragged), resyncs `saved`, diffs forward cards in and out,
and rebuilds traffic lanes.

Server side, every status call rebuilds the whole graph: store reads,
telemetry, swarm `ListNodes` / `ServiceTasksOnNetwork`, Docker
`ListProxyRelays`, and the sampler.

**Prefs:** `GET /account/graph-prefs`, a client-owned JSON blob on the
user row (≤ 4 KB, valid JSON). The server reads back only `arrange`. It
wins over the localStorage copy once it arrives.

## 4. Data out

All JSON, with the `X-CSRF-Token` header. Fire-and-forget; errors are
swallowed.

| URL | method | payload | when |
|---|---|---|---|
| `{base}/positions` | POST | `{node_id, x, y}` | every 250 ms during a drag |
| `{base}/positions` | POST | `{nodes: [{node_id, x, y}…]}` | first move on a canvas with unsaved cards (all cards); drop; undo/redo; annotation-group drags |
| `{base}/positions/reset` | POST | none | Re-arrange, followed by reload |
| `{base}/annotations` | POST | `{id?, kind, body, x, y, w, h}` → `{id}` | create (server mints the id), move, resize, edit |
| `{base}/annotations/delete` | POST | `{id}` | × button, or text emptied |
| `{base}/groups` | POST | `{id?, members}` → `{id}` | group, or membership change |
| `{base}/groups/delete` | POST | `{id}` | ungroup, or group thinned below 2 |
| `/account/graph-prefs` | PUT | whole prefs object | every view toggle; before reset |
| `Href/panel` (e.g. `/apps/:id/panel`, `/dbs/:id/volume/panel`, `…?class=secret`) | GET via `htmx.ajax` into `#drawer-body` | none | card or sub-tile click |

- Position saves are chained so they arrive in order.
- Annotation and group saves share a second chain, so a group save never
  lands before the save that mints a member's id.
- Every write handler calls `notifier.Project` (or the org room), which
  nudges every open canvas to re-poll. That is the "live-shared" drag:
  viewers see the card jump every 250 ms.
- Auth verbs: org `VerbOrgGraphWrite`, stack `VerbStackWrite`, env
  `VerbEnvWrite`, reads `VerbOrgRead`, home only `RequireAuth`.

## 5. State kept client-side

- **localStorage**: `stackr.graph.settings` (prefs, first paint only);
  `stackr.envcompare.open` and `stackr.envcompare.tab` (the stack page's
  env-compare pill).
- **sessionStorage**: `stackr.graph.deck` (clicked card's rect, for the
  deal-in) and `stackr.graph.gather` (departing cards' rects).
- **In memory**: viewport panX/panY/scale (start 0.8, not persisted);
  selection set; entered group; undo/redo stacks; hover focus; traffic
  lane registry; a drawer generation counter so a close beats an
  in-flight open.
- **In the DOM**: `data-saved` (position persisted), `data-suppressClick`
  (swallow the click after a drag), `data-face` / `data-rendered` (last
  swapped HTML).

## 6. Layout algorithm (server, `graph.Arrange`)

- **Saved positions always win**; only unsaved cards are laid out,
  deterministically. Traffic edges never drive placement.
- **Newcomer beside a hand-placed neighbour.** A card with a saved
  neighbour tries four slots in order: left (one card plus 80 px), right,
  above, below. It takes the first free one, otherwise steps down in rows.
  These slots, satellites and row steps snap to the 22 px grid; column x
  positions do not (the proxy sits at -280, columns at 80 and 380).
  - This is why the first drag must save *every* card. Otherwise all
    unsaved neighbours become newcomers and jump next to the dragged card.
- **Engines**, chosen by the `arrange` pref:
  - **clusters** (default): each connected component is an island; islands
    are placed biggest first. Cards welding several islands together
    (shared instances) become hubs in their own column. Inside an island,
    columns follow directed depth.
  - **flow**: layers left to right by longest path along the edge
    direction (cycle-capped).
- **Satellites.** A card with exactly one link, whose host is a workload
  with 2+ links, sits right beside the host. Satellites cycle right,
  below-right, under, above-right.
- **Orphans**: a row below all. **System cards**: left column, re-centred.
- **Polish pass.** Greedy single-card nudges until no edge passes under a
  card it does not touch. One crossing is scored at about 400 px of extra
  edge length.
- **Collision box.** Card 220×96 plus 30 px per sub-tile; gaps 40 × 30.
- **Org, stack and env canvases all run `Arrange`** (org/graph.go,
  stackgraph.go, project/handler.go). The column sweep (`placeColumns`,
  rows 140 px apart from y 80, skipping rows a dragged card holds) is only
  **home's**: `layoutOrgs` fills columns of 4 orgs by id, 360 px apart.
  v0's comment on `placeColumns` still says stack and org use it; stale.
- **Rewrite port (step 6e)**: `internal/service/internal/flow/graph/arrange.go`
  is this, pinned by v0's exact coordinates in `arrange_test.go`. Not
  ported: flow and the `arrange` pref (clusters only). Egress edges (the
  live traffic sample, v0's "traffic") never drive placement. Replica
  sub-tiles are left out of the card height; volume and instance subs
  count.
- **Late additions**, no layout of their own: ghost refs park 260 px
  right of their first consumer, forward cards 300 px left of their
  target.
- **Persistence.** One `node_positions` row per (owner, node id), owner =
  `GraphOwner(scope, id)`. Env canvases store tiles by *slug* (the handler
  maps to and from UUID) so a rebuilt tile keeps its place; org cards by
  UUID so a rename does. Reset deletes the owner's rows.
- **Client no-overlap.** Opt-in; single-card drop; moves only that card.

## 7. Rewrite mapping (if the canvas returns)

The rule: `<env-graph>` is one custom element. Its children are
templ-rendered node divs carrying htmx attributes. The JS does pan, zoom,
drag and edges only. It fires `node-moved` and `node-selected` and never
fetches.

### Stays in the JS element (geometry)

- wheel, pinch, trackpad pan, zoom buttons, pan clamp, fit/reset view
- node drag, slop, snap, divider wall, no-overlap nudge; `node-moved` on
  drop only
- edge re-path (sides, curve/straight, fan-out); sub-tiles follow
- hover focus (class toggles); `?focus=` centring from a templ attribute
- marquee fires `node-selected`; Escape clears it

### Becomes an htmx attribute on the templ node

- card click: `hx-get` on `…/panel` into the drawer (DECIDE 2 below)
- drill-down cards: plain `<a href>` with `hx-push-url`
- `node-moved`: `hx-post` to positions with `hx-trigger="node-moved"`,
  the confirm-dialog pattern, drop only (DECIDE 3 below)
- Re-arrange: `hx-post` reset; server re-renders the canvas fragment
- view toggles and arrange style: what changes the server's drawing
  (system nodes, ref/startup edges) becomes a query param; pure look
  (arrows, straight, fan) becomes a templ-set element attribute
- create modal, source switch, repo pick: an ordinary templ form, not the
  graph's job

### Becomes an SSE swap

- card footer (status, replica roll-up, exposure): `sse-swap` per node
- node added or removed: swap the whole canvas fragment, replacing the
  count check plus `location.reload()`
- remote position changes: dropped; others see them on the next swap

### Dropped in v1 (plan Later)

- **metrics**: traffic lanes, rx/tx, teal traffic edge, busy-slice pulse,
  txn/s and size footer
- **port forwarding**: forward cards, edges, chips
- **cron tiles and functions**: their cards and run footers
- **storage shares**: storage sub-tiles
- **multi-node, volume moves**: node chip, move bar
- **the canvas** itself

Also dropped: the "edited"/"removing" markers (staging is not coming
over).

### Dropped in v1, never asked for (same verdict as templ-ref)

Groups (enter, Ctrl+G, endpoints); annotations (notes, boxes, endpoints);
right-click menu; undo/redo; live-shared 250 ms drag; deal/gather view
transitions and their sessionStorage keys; the JS legend builder (static
templ legend if kept); the server divider; the home all-orgs canvas.

### Kept as server-rendered facts (v1 has these)

Replica roll-up text (single node), "new version" chip (image watch),
"host" chip, exposure, ghost refs, vars/secrets count cards.

### Belongs in services, not templ or handlers

Worst-status ranking, `edgeBusy`, `SliceDomains`, "waiting for <param>"
parsing, `isEmpty`, `kindLabel`, and Build/Arrange as a whole: a `graph`
service fed view structs. The old handlers called the store, telemetry,
swarm and Docker directly.

### Decisions

- DECIDE 1: REWRITE.md says the canvas "gets its own island then". Is that
  island `<env-graph>` under the 300-line element cap? Pan, zoom, drag and
  edge re-path alone were about 500 lines here. Options: (a) split into
  `<graph-viewport>` (pan/zoom) plus `<graph-edges>` (re-path) plus
  node-level drag, each under 150 lines; (b) grant the canvas a written
  exception to the cap; (c) keep the canvas Later.
- DECIDE 2: drag must suppress the next click, or every drag also fires the
  card's `hx-get`. Either the element calls `preventDefault` or
  `stopImmediatePropagation` on that click (touching only its own
  subtree, which is allowed), or the drawer trigger is a custom
  `node-selected` event instead of `click`.
- DECIDE 3: how `node-moved` carries x/y to `hx-post`; no precedent in
  internal/ui (confirm-dialog's `confirmed` carries no data). (a) templ
  renders hidden `x`/`y` inputs in each node, the element writes them on
  drop, the node carries `hx-post hx-trigger="node-moved"
  hx-include="this"`; (b) `hx-vals="js:…"`, inline JS, forbidden; (c) one
  form per canvas whose fields the element sets. (a) fits the rules.

Size: source 9280 lines (graph.js 1978 + 16 server files 7302; graph sections of handler/project/handler.go and account/handler.go read, not counted), extract 349 lines (prose only; nothing copied)

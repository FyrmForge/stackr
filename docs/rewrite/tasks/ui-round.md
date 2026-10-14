# UI round: map, drawers, egress model

Status: planned 2026-10-14 on `multi-node` (head fb6f843). 13 items, 5 lanes.

## Rules

- **Never hand-edit generated files:** `*_templ.go`, `ui/static/js/elements/*`, `ui/static/css/output.css`, `docs/openapi.json` and `internal/ui/components/staticmanifest.go`. Regenerate them with `templ generate`, `cd ui && npm run css:build`, `cd ui && npm run ts:build`, `hamr gen static` and `go run ./cmd/stackrd --dump-openapi > docs/openapi.json`, then commit the result. CI checks that it matches.
- **Generated-file merge conflicts:** never merge them by hand. Take either side, regenerate, and commit. Tailwind scans every `.templ`, so any lane that adds a class changes `output.css` and the manifest hash. That is expected.
- **`ui/css/input.css` has one owner, lane C.** Other lanes style with Tailwind classes in their own templ files. Arbitrary variants are fine, e.g. `[side-drawer[full]_&]:max-w-5xl`.
- **Test first.** For Go, write the failing test, then the code.
  - TS has no test harness, so don't add vitest.
  - For TS items, "test first" means two things: a templ-render Go test that pins the markup the TS relies on, and a Playwright check on `hamr dev` (the fake workers give a multi-node map). Note that check in the commit body.
- **Finish every lane with:**
  - `go build ./... && go vet ./...`
  - the tests of the touched packages
  - `golangci-lint run` (2.14) clean
  - `cd ui && npm run ts:check` if TS changed
  - `make templint` if templ changed
- **Commits:**
  - `fix:` for UI fixes and polish, `feat:` for new capability.
  - One line, no AI attribution.
  - Add files by name (`git add path ...`), never `-A`.
- **Never print secrets.** That covers keys, tokens and join commands in logs or test output.
- **UI copy:** no em dashes, terse, one short sentence per hint.
- **Style:** match the neighbouring code. Mark a deliberate shortcut with a `ponytail:` comment naming its ceiling.
- **Layering stays as AGENTS.md says:**
  - Handlers bind input, call the orchestrator and render.
  - Decisions (who dials, own vs others' egress, interface groups) are made in `internal/service`.
  - `.templ` files only display them.

## Lanes and file ownership

A file belongs to exactly one lane. Where a later lane needs a file, the order section says when.

| Lane | Model | Items | Owns |
|---|---|---|---|
| **W facts** | Sonnet | 1 (data), 6 (worker data), 7, 8 (data) | `internal/worker/extras.go`, `internal/worker/worker.go`, `cmd/stackrd/worker.go`, `internal/wgapply/{wgapply.go,linux.go,fake.go}`, `internal/service/internal/docker/{docker.go,containers.go}`, `internal/service/internal/dockerfake/fake.go`, `internal/service/multinode.go`, `internal/service/clustermemory.go`, `internal/service/nodeapi.go` (first), new `internal/service/nodelive.go` |
| **E egress** | Opus | 10 (backend, CLI, API) | `internal/exits/{resolve.go,plan.go}`, new `internal/db/migrations/024_node_exits.{up,down}.sql`, `internal/db/*_test.go`, `internal/service/internal/store/exit_ladders.go`, `internal/service/{defaults.go,placement.go,mesh.go,join.go,nodeadmin.go}`, new `internal/service/nodeexit.go`, `internal/worker/mesh.go`, `internal/api/routes.go`, `internal/api/handler/v1/nodes.go`, `cmd/stackr/node.go`, `docs/openapi.json`, `internal/service/nodeapi.go` (after W) |
| **C canvas** | Sonnet | 2, 3, 12 (graph cards), the graph mechanics for 4, 6 and 9 | `ui/ts/graph-canvas.ts`, `ui/css/input.css`, `internal/ui/graph/{view.go,graph.templ,graph_test.go}`, `internal/ui/graph/cards/cards.templ`, `internal/service/internal/flow/graph/graph.go` (CardH only) |
| **M map** | Sonnet | 1 (display), 4, 5, 6, 7 (wiring), 9, 12 (map card), 8 (tunnel data) | `internal/service/cluster.go`, new `internal/service/tunnels.go`, `internal/ui/cluster/*` (plus new `drawers.templ`), `internal/web/handler/cluster/*` |
| **D drawers** | Sonnet | 8, 10 (UI), 11, 13 | `internal/ui/components/{drawer_view.templ,drawer.templ,layout.templ}`, `ui/ts/side-drawer.ts` (only if needed), `internal/ui/drawer/admin/nodes.templ`, `internal/web/handler/admin/nodes.go` and its tests |

### Order

1. **Phase 1, in parallel:** W (everything), E (everything except `nodeapi.go`), C (everything), and D's items 11 and 13.
2. **Phase 2:**
   - M starts once W and C have merged. It needs the Facts fields, `nodeMemory.Arch`, `controllerFacts`, and `graph.Edge` `Dir`/`Drawer`/`Push` plus the new edge kinds.
   - E does its `nodeapi.go` part once W has merged.
3. **Phase 3:**
   - M item 9 waits for E's `nodeexit.go` resolver.
   - D item 8 waits for W (`NodeLive`) and M (`NodeTunnels` and the tunnel drawer URL).
   - D item 10 (UI) waits for E (`NodeExits`, `SaveNodeTags`).

Items 1 and 7 share the fact path: W adds the fields and `controllerFacts`, and M wires them into `cluster.go`. Item 6 comes before item 8's tunnel rows, and item 10 before item 9.

---

## Lane W: facts

### Item 1 (data): CPU arch in facts
- **`internal/worker/extras.go`, `Facts`:**
  - Add `Arch string `json:"arch,omitempty"``, set in `(*Server).Facts` from `runtime.GOARCH`.
  - Add the comment `// ponytail: the worker binary's arch, not Docker's Architecture; service.dockerArch can't be imported here (cycle). Same thing on every real install.`
- **`internal/service/clustermemory.go`, `nodeMemory`:** add `Arch string `json:"arch,omitempty"`` so an offline node keeps its arch. `remember` fills it from the new `ClusterNode.Arch`, which M adds. Until M lands, pass it through `clusterNodeIn.Facts`.
  - Simplest order: W adds the field and the `remember` line, M adds `ClusterNode.Arch`. Coordinate by committing W first; if `ClusterNode.Arch` doesn't exist yet, W leaves the `remember` line for M.
- **`internal/service/nodeapi.go`, `NodeOut`:** add `Arch string `json:"arch"``, filled in `nodeOut` from the facts.
- **Test first:**
  - `internal/worker/worker_test.go`: GET `/v1/facts` returns `arch == runtime.GOARCH`.
  - `internal/service/nodeapi_test.go` `TestListAndShowNodes`: arch present.
- **Gap:** none.

### Item 7: the controller's facts
- **What exists:** on a real multi-node install, `o.mesh.factsOf(ctlID)` already holds the controller's full facts. `meshSvc.refreshAll` reads the controller through `localMesh` (`multinode.go`), which is the same worker code with Docker, Ping, Seen and Rx.
- **The bug:** `cluster.go` `clusterFacts` returns only `Interfaces` for the controller (line 700), and `nodeapi.go` `reportedFacts` returns nil for it (`n.Role != node.Worker`).
- **Change:**
  - New `func (o *Orchestrator) controllerFacts(ctx, n store.Node) *worker.Facts` in `nodelive.go`:
    - Use the mesh facts when present.
    - Otherwise fall back (single node, fake host) to `worker.Facts{Interfaces: worker.Interfaces(), Arch: runtime.GOARCH, CPUs, MemBytes, Docker}`, read from `o.docker.HostInfo(docker.WithNode(ctx, n.ID))`.
  - Add `Version string` to `docker.HostInfo`: set from `info.ServerVersion` in `containers.go` `HostInfo`, and add the field in `dockerfake`.
  - `reportedFacts` calls `controllerFacts` for the controller.
  - M replaces the controller branch in `clusterFacts` with `return o.controllerFacts(ctx, n), true, 0`.
- **Test first:** in `nodeapi_test.go`, with a fake Docker whose `Host.Version = "29.9.0"`, `ListNodes` shows the controller with `docker == "29.9.0"` and a non-empty arch.
- **Gap:** none.

### Item 6 (worker data): handshake and bytes sent
- **What exists:**
  - `Facts.Rx` (bytes received per peer key).
  - `Facts.Seen` (endpoint, fresh handshakes only).
  - `Facts.Ping` (RTT by transit address).
  - `wgapply.PeerStat.Handshake` exists but is not reported.
  - Bytes sent are not read at all.
- **Change:**
  - `wgapply.PeerStat` gets `Tx int64`. `linux.go` `Stats` sets `Tx: p.TransmitBytes`. `fake.go` gets `SetTx(key, n)` mirroring the existing rx setter.
  - `Facts` gets `Tx map[string]int64 `json:"tx,omitempty"`` and `Handshake map[string]int64 `json:"handshake,omitempty"`` (unix seconds, zero left out), both filled in the existing `Stats` loop in `(*Server).Facts`.
- **Test first:** `internal/worker` test with `wgapply.Recorder`: `SetHandshake` and `SetTx` show up in `/v1/facts`.
- **Gap:** workers must be upgraded to report these. Older workers show "not reported".

### Item 8 (data): version, last seen, interface groups
- **Stackr version** is not reported today.
  - Add `Version string` to `worker.Config` and `Facts.Version` (`json:"version,omitempty"`).
  - `cmd/stackrd/worker.go` passes `main.version`.
  - `multinode.go` `startMesh` passes `o.cfg.Version` to the local worker.
- **New file `nodelive.go`:**
  ```go
  type NodeLive struct {
      Facts    *worker.Facts // controller via controllerFacts, worker via reportedFacts
      LastSeen time.Time     // meshSvc.at[id] under m.mu; the controller: now
      Ifaces   []NodeIface
  }
  type NodeIface struct{ Group, Name, Addr, Label string }
  ```
  - `Group` is one of `public`, `lan`, `cluster`, `docker` or `hidden`.
  - Add `func (o *Orchestrator) NodeLive(ctx, id string) (NodeLive, error)`.
- **Classify per address, not per interface** (today fe80 on a public NIC is called "public", and `stackr0` is called "private network" because the handler checks for the `wg` prefix). Use a pure `groupIfaces(f worker.Facts, envs map[string]string /*subnet -> org/stack/env*/) []NodeIface`:

  | Address | Group | Label |
  |---|---|---|
  | `lo`, loopback addresses | hidden | "loopback" |
  | fe80::/10 and 169.254/16 | hidden | "link-local" |
  | `veth*` | hidden | "veth" |
  | `stackr0` (mesh.Iface) | cluster | "tunnel" |
  | `br-*` whose prefix masks to an env subnet (`environments.subnet` of envs on this node) | cluster | the env path |
  | other `br-*` inside `mesh.Supernet` | cluster | "stackr network" |
  | `docker0` and other `br-*` | docker | (none) |
  | any address `isPublic` | public | (none) |
  | other private addresses | lan | (none) |

  - Add `// ponytail: bridges named by subnet, not by Docker network; a job's network shows as "stackr network".`
- **Test first:** table test of `groupIfaces`. eth0 with a public v4 and fe80 gives one public row and one hidden "link-local" row. A `br-1a2b` whose prefix is `10.208.3.1/24` with env subnet `10.208.3.0/24` gives cluster "acme/shop/prod". `lo` gives hidden "loopback".
- **Gap:** cert expiry already exists (`NodeDetail.Certs`).

**W commits:**
- `feat: nodes report their CPU arch and stackr version`
- `fix: controller shows its docker version and arch`
- `feat: workers report wg handshake and bytes sent per peer`
- `feat: node interfaces grouped by kind`

---

## Lane E: egress model (feat)

### What exists
- `exits.Resolve(Levels{Tile,Env,Stack,Org}, def)`. `def` is the cluster-wide default exit setting (`defaults.go` `defaultExitIDs`, setting `default_exit`). `SourceNone` means the own uplink.
- The resolution is built three times:
  - `mesh.go` `gather` (the real routing),
  - `cluster.go` `exitNode` (the map),
  - `placement.go` `ResolveExits`.
- An exhausted ladder already blackholes (`exits.Subject.Blackhole`, `Ladder.Tick` with zero rungs gives Down, "no exits listed").
- `buildMesh` skips a subject whose filtered ladder is empty (`if len(l) == 0 { continue }`). That is the leak to close.
- The CHECK constraint on `exit_ladders` allows org, stack, env and tile only (`014_exit_ladders.up.sql`).
- The removal check to mirror is the ingress-pick refusal in `SetNodeTags` (`IngressPicks.ListByNode`, "…; pick another ingress there first."). `RemoveNode` checks only envs today.

### Changes

1. **Migration `024_node_exits`**, following the table-rebuild precedent in `010_env_tiers.up.sql`:
   - Rebuild `exit_ladders` with `scope_kind IN ('org','stack','env','tile','node')`, copy the rows, recreate the index and the four cascade triggers.
   - Add `exit_ladders_node_cascade AFTER DELETE ON nodes` (scope_kind 'node').
   - Tag every node egress:
     ```sql
     UPDATE nodes SET tags = json_insert(tags, '$[#]', 'egress')
     WHERE NOT EXISTS (SELECT 1 FROM json_each(nodes.tags) WHERE value = 'egress')
     ```
   - The down migration rebuilds without 'node' (dropping those rows) and leaves the tags (comment why).
   - **Test first:** in `internal/db`, after up, a node with tags `["ingress"]` has egress, a 'node' rung inserts, and a down then up round trip works (pattern: `nodes_test.go` `TestNodesMigrationRoundTrip`).
2. **`exits/resolve.go`:**
   - Add `Node []string` to `Levels` and `SourceNode`.
   - Order: Tile, Env, Stack, Org, Node, Default. The cluster default comes last; see open question 2.
   - **Test first:** extend `TestResolve`: the node level beats the default and loses to org.
3. **New `nodeexit.go`, one resolver for all three callers:**
   ```go
   // envExits is the env's exit ladder as routing uses it: env, stack, org,
   // the node's default exit (only when the node lacks egress), the cluster
   // default. A node without egress never lists itself; nothing left = no
   // internet. Nil ladder with own=true = the node's own uplink.
   func envExits(ctx, st *store.Store, e store.Environment, orgID string, n store.Node) (ladder []string, own bool, err error)
   ```
   - `OwnRung` maps to `n.ID`.
   - Rungs whose node lacks egress are dropped. That covers the own rung on a non-egress node, and it is what the tag now means.
   - `own` is true only when the node is egress-tagged and the ladder is empty or only itself.
   - Replace the inline code in `mesh.go` `gather`, `cluster.go` `exitNode` (M wires this in item 9) and `placement.go` `ResolveExits`.
   - **Test first:** table test with a real store (`servicetest`):
     - egress node, nothing set: own.
     - non-egress node with default `[h4]`: `[h4]`.
     - non-egress with env ladder `[own]`: empty, not own.
     - non-egress with org ladder `[h4]`: org wins.
4. **Firewall drop for non-egress nodes:**
   - **(a) Routing (no worker upgrade needed):**
     - `meshNodeIn` gets `NoUplink bool`.
     - `gather` adds a `meshEnvIn` for every env with a subnet on a non-egress node, not only when `res.Routed`.
     - `buildMesh` keeps a subject with an empty ladder when `NoUplink`, so the worker ladder blackholes it.
   - **(b) The firewall rule:**
     - `MeshSpec.NoUplink []string` (env subnets).
     - New `exits.Fence{Sources []string; Uplink string}` in `plan.go` with `Up()`/`Down()` steps.
     - A `STACKR-FENCE` filter chain, flushed and refilled:
       - for each source: `-s src -d r -j RETURN` for each `exits.Refused` range (LAN and private traffic still flows),
       - then `-s src -o uplink -j DROP`,
       - and one `once` jump from `DOCKER-USER`.
     - `worker/mesh.go` `meshExits` applies it, using the existing `s.uplink(ctx)`. `reassert` re-checks the jump.
     - Exit-link and stackr0 traffic never leaves by the uplink, so it is untouched.
   - **Test first:**
     - `mesh_internal_test.go` `TestBuildMeshNoUplink`: a non-egress node gets a zero-rung ladder and `NoUplink`.
     - `exits_test.go` `TestFence`: the argv.
   - Add `// ponytail: IPv4 only; env networks have no IPv6 today.`
5. **Default exit verbs in `nodeexit.go`:**
   - **`SetNodeExit(ctx, by, ref string, refs []string) (NodeOut, error)`** (names in, names out, like `ExitSet`):
     - every rung must be egress-tagged and not the node itself, no duplicates, at most 3 (`errs.Invalidf("exits", …)`);
     - stored as scope 'node';
     - audit `node.exit`;
     - `kickMesh`.
   - **`NodeExits(ctx, id) (NodeExitView, error)`:** `{Ladder []string; Options []PlacementNode; Suggested string}`. `Suggested` is the egress node with the lowest `Facts.Ping[transitOf(x)].RTTms > 0` as this node measured it.
   - **`SaveNodeTags(ctx, by, id, tags, exits []string) error`:** sets the ladder, then calls `SetNodeTags`, so the web form is one call.
6. **`nodeadmin.go`:**
   - `SetNodeTags` refuses removing egress when the node has no default exit: `errs.Conflictf("Pick a default exit for %s first.", n.Name)`. It also refuses removing it from the controller while it has no default exit (same check).
   - `RemoveNode` refuses when `ExitLadders.ListByNode(id)` has a 'node' rung: `errs.Conflictf("%s is the default exit of %s; point it elsewhere first.", …)`. Forget stays allowed (the node is gone; those nodes lose that rung and fall to their next one).
   - `nodeapi.go` `scopePath` learns kind "node" (the node's name), so `ExitFor` reads right.
   - **Test first** in `nodeadmin_test.go`:
     - untag egress without exits: conflict;
     - with exits: ok;
     - remove a node that is someone's default exit: conflict.
7. **`join.go` `CreateJoinToken`** always adds egress. A default exit can't be picked before the node exists. **Test first:** in `join_test.go`, a token minted with `["ingress"]` carries egress.
8. **API, CLI, docs:**
   - `PUT /admin/nodes/:node/exit` (op `admin.node-exit`, `node.manage`), body `{"exits":["h4"]}`, answers `NodeOut`.
   - `NodeOut` gets `DefaultExit []string `json:"default_exit"`` (names; empty for an egress node). This is the `nodeapi.go` part that waits for W.
   - CLI: `stackr node exit <node> [<exit>...]` next to `node tag`. No exits clears, and the service refuses a clear on a non-egress node.
   - `node show` prints `default exit`. `node untag … egress` surfaces the service's refusal.
   - Regenerate `docs/openapi.json`.
   - **Test first:** `cmd/stackr/node_test.go` checks the request JSON. `TestEveryRouteHasAVerb` covers the new op.

**E commits:**
- `feat: egress tag means traffic may leave from the node's IP; every node tagged egress`
- `feat: per-node default exit, required to drop egress`
- `feat: nodes without egress drop direct internet from env ranges`
- `feat: stackr node exit`

---

## Lane C: canvas, cards, CSS

### Item 2: middle-button pan (`ui/ts/graph-canvas.ts`)
- **`onDown`:** before the `e.button !== 0` return, handle `e.button === 1`:
  - When the target is in `input, textarea, select, [contenteditable]`, return so native middle-click still works.
  - Otherwise `preventDefault()`, set `this.pan`, and record the start point for a moved flag. Don't call `select()`, and don't check for a card.
  - `graph-node.ts` already ignores `button !== 0`, so cards don't drag.
- **Autoscroll:** add a `mousedown` listener in `wire()` that calls `preventDefault()` for `button === 1` outside text fields. Firefox starts autoscroll on mousedown.
- **Link after a drag:** add an `auxclick` capture listener that swallows the event (preventDefault) only when the middle pan moved past 3 px. A middle click on a link without a drag still opens it.
- Also use the moved flag for left-button pans: a capture-phase `click` listener swallows the click that ends a pan which moved. Item 6's clickable lines need this.
- **Test:** Playwright on the stack and cluster graphs:
  - middle-drag on a card pans, and the selection and card x/y are unchanged;
  - middle-click on an env row link opens a tab;
  - middle-click in a note textarea is untouched.

### Item 3: notes above everything (`input.css`, about line 690)
- Today notes and boxes share z-index 0, under the edges (1) and cards (2). Dragging is z-index 10.
- Add `graph-canvas > graph-node[note-kind="note"] { z-index: 3; }`, with `note-kind={ n.Kind }` emitted on the note's `graph-node` in `graph.templ` `note`.
- Box annotations stay at z-index 0 (open question 1).
- **Test first:** in `graph_test.go`, a note renders `note-kind="note"` and a box renders `note-kind="box"`.

### Mechanics for items 4, 6 and 9 (`internal/ui/graph/view.go`, `graph.templ`, CSS, TS)
- **`graph.Edge` fields:**
  ```go
  type Edge struct {
      Kind, From, To, Label string
      Dir          bool   // tunnel: an arrow From -> To
      Drawer, Push string // a click on the line opens this drawer; "" = not clickable
  }
  ```
- **Template:**
  - `Dir` emits `data-dir`.
  - `Drawer` emits, after the label, `<path data-edge-hit d="M0,0" hx-get hx-target="#drawer-body" hx-push-url>`.
  - The hit path must not carry `data-from` or `data-edge-kind`. `paths()`, fan-out and `light()` treat every `path[data-from]` as an edge.
- **Arrow marker:** a new `<marker id="tunnel-arrow" markerUnits="userSpaceOnUse" markerWidth="12" markerHeight="12" orient="auto" fill="context-stroke">` in the defs. The default strokeWidth units would draw a 30 px head on a 6 px pipe.
- **TS `repath`:** walk the siblings after each edge path. Copy `d` onto `[data-edge-core]` and `[data-edge-hit]`; the label is still placed from the edge path.
- **CSS (C owns):**
  - `path[data-edge-kind^="tunnel"][data-dir] { marker-end: url(#tunnel-arrow) }`.
  - `path[data-edge-hit] { stroke: transparent; stroke-width: 16; fill: none; pointer-events: stroke; cursor: pointer }`. The edges SVG keeps `pointer-events: none`, and the child opts back in.
- **Edge looks:**
  - `edgeLooks["egress-own"]`: amber, stroke-width 1, stroke-opacity 0.35.
  - `edgeLooks["egress-via"]`: amber, `stroke-dasharray: "3 4"`, stroke-opacity 0.5.
  - Legend rows: "egress, own apps" and "egress through another node".
- **Test first:** in `graph_test.go`, an Edge with `Dir` and `Drawer` renders `data-dir` and a `data-edge-hit` path with `hx-get` and no `data-from`. Then a Playwright check that a 1 px `overflow: visible` SVG still hit-tests a line far from its origin, and that the pipe arrow looks right (the core cuts a hollow line through it, which is fine).

### Item 12 (graph cards): footer on the bottom edge
- `cards.templ` `Card` and `graph.templ` `body`: the inner `p-3.5` becomes `flex h-full flex-col p-3.5`.
- `Footer`: `mt-3` becomes `mt-auto` (it is sent by SSE too, so the class lives in `Footer`).
- `CardH = 96` in `flow/graph/graph.go` is too short. Estimated: 14 + about 38 (head with a chip row) + 1 + 12 + 16 + 14 is about 95, with no gap left.
  - Measure in the browser first.
  - If the footer still touches, raise `CardH` to 104 and run `go test ./internal/service/internal/flow/graph/...` (arrange uses the constant).
- **Test:** Playwright screenshots of tile, slice, managed and proxy cards: the footer sits 14 px off the bottom edge.

**C commits:**
- `feat: middle-button drag pans every graph`
- `fix: notes draw above cards and lines`
- `feat: graph edges can carry an arrow and open a drawer`
- `fix: card footers sit on the bottom edge with padding`

---

## Lane M: cluster map

### Item 1 (display)
- `ClusterNode.Arch` (from facts, else `mem.Arch`).
- `mapMap` in `handler/cluster/handler.go` and `cluster.Node.Arch`.
- `nodeCard` footer: `docker 29.9.0 · amd64`. When docker is unknown, the arch alone.
- **Test first:** in `internal/ui/cluster/cluster_test.go`, the rendered card contains `docker 29.9.0 · amd64`.

### Item 7 (wiring)
- `clusterFacts` controller branch becomes `return o.controllerFacts(ctx, n), true, 0` (W's func).
- **Test:** `TestBuildClusterMap` with controller facts carrying Docker.

### Item 4: tunnel arrows
- **Finding:** the mesh knows who dials. `mesh.Candidates(self, other)` gives `other`'s LAN addresses when they share a subnet, then its public ones, and each node's last spec keeps them (`meshSvc.specs`, read through `o.mesh.Spec(id)`). `spec(a).Peers[b].Candidates` non-empty means a has an endpoint for b and dials it. Empty means a waits. Nothing new is needed from workers.
- **New `tunnels.go`:**
  ```go
  type TunnelEnd struct {
      Dials     bool      // has an endpoint for the other side
      Endpoint  string    // Candidates[0], "" = waits to be dialled
      SeenFrom  string    // Facts.Seen[peer key]: where the other side's packets arrive from
      Handshake time.Time // Facts.Handshake, zero = not reported
      Rx, Tx    int64     // Facts.Rx/Tx[peer key], -1 = not reported
  }
  // ClusterLink gets Ends [2]TunnelEnd: [0] is A's side of the pair.
  ```
  - `fillEnds(m *ClusterMap, in []clusterNodeIn, spec func(string) (worker.MeshSpec, bool))` runs in `ClusterMap` where `o.mesh != nil`, on `m.Links` and `m.Pairs`.
  - The peer's public key comes from the spec's `Peer.PublicKey`, which also covers the controller's keyless row.
- **View:** if exactly one end dials, the edge runs From the dialer To the listener with `Dir: true`. If both dial, or nothing is known, there is no arrow (open question 3 picks none over double).
- **Test first:** a pure test of `fillEnds` on the rig topology:
  - 109, 110 and 106 share a LAN, so all of them dial each other: no arrow.
  - 109 dials h4, and h4 has no endpoint for 109: arrow 109 to h4.
  - h3 and h4 dial each other over the widened /32 LAN: no arrow.
  - h3 and 109: neither dials, the pair is routed via h4 and draws no line.

### Item 5: Internet drawer
- Refactor `Build`: the `public(...)` closure's `inetInfo` per card becomes an exported `Internet(m Map, from string) (Inet, bool)`, which `Build` also uses.
- `inetCard` loses the `<button popovertarget>` and the `[popover]` panel. It becomes the `card-face` with:
  - `hx-get={ InternetDrawer(from) }`, where `InternetDrawer` returns `Base + "/internet?from=" + url.QueryEscape(from)`. Ids contain `:` and `/`, so it's a query parameter, not a path parameter.
  - `hx-target="#drawer-body"`.
  - `hx-push-url={ InternetPush(from) }`, i.e. `?drawer=` + escaped `internet:` + from.
  - `role="button"` and `tabindex="0"`.
- **Handler:**
  - `GET /-/cluster/internet` renders `components.Drawer` with the new `drawers.templ` `internetBody`.
  - `Page` also handles `drawer=internet:` and `drawer=tunnel:` through `DrawerLoad`.
- **Content:**
  - Router (NAT cards only): "Ports 80 and 443 forward to X.", the unconfirmed form, or the warning "No port forward. The internet does not reach ingress here."
  - Public addresses.
  - Ingress: host and env link.
  - Egress: "Own apps" and "Exit for other nodes" (item 9's split).
- **Test first:**
  - `handler_test.go`: `GET /-/cluster/internet?from=node:<id>` returns 200 with the ingress host and env.
  - `cluster_test.go`: no `popover` in the map.

### Item 6: tunnel drawer
- Tunnel edges get `Drawer: TunnelDrawer(a, b)`, which returns `Base + "/tunnel?a=..&b=.."` with the pair in `mesh.PairKey` order, and `Push: "?drawer=tunnel:a:b"`.
- `GET /-/cluster/tunnel` finds the pair in `ClusterMap().Pairs` (else `Links`) and renders:
  - both ends, linked to `NodeDrawer`;
  - kind (LAN, private, internet, or "via <node>");
  - who dials;
  - each end's endpoint and seen-from address;
  - ping;
  - last handshake per end (as "2 min ago");
  - bytes sent and received per end;
  - via node;
  - a "Ping history" section reading "Comes with observability."
- `NodeTunnels(ctx, id) ([]NodeTunnel, error)` in `tunnels.go`: `{Peer, PeerName string; Link ClusterLink}` from `ClusterMap().Pairs`, for D. Add `// ponytail: builds the whole map; cached facts make it cheap.`
- **Test first:** `handler_test.go`: the tunnel drawer for a seeded pair shows both names and "Ping history". `TestClusterDrawerLoad` covers `?drawer=tunnel:a:b`.

### Item 9: egress arrows (after E)
- `exitNode` uses E's `envExits`. The first rung is the exit; no rung means no internet and no arrow.
- `ClusterNode.ExitEnvs` splits into `ExitOwn` (this node's envs leaving here) and `ExitOthers` (other nodes' envs leaving here).
- New `ClusterMap.Egress []ClusterEgress{From, To string; Envs []string}`: env node to exit node, when they differ.
- **View:**
  - A node or NAT internet edge is `egress` (full) when there are `ExitOthers`. With only `ExitOwn`, it is `egress-own` (thin, faded).
  - Each `Egress` pair is an `egress-via` edge From to To. A non-egress node draws its arrow to its default exit this way.
  - A node with no public IP in a network that is not `Router` (h3 in the Hetzner private network) draws its own egress to one shared Internet card per network, `internet:net:<id>`, placed above the box like the NAT's, with the drawer note "Leaves through the network's gateway." It no longer gets its own Internet card.
- **Test first:**
  - Service test: an env on h3 with exit h4 gives h4 `ExitOthers` and `Egress{h3, h4}`.
  - `cluster_test.go`: own-only gives `egress-own`. A public-less node in a non-router network gets an edge to `internet:net:<id>` and no `internet:node:` card.

### Item 12 (map card)
- `nodeCard`'s face `div.p-3.5` becomes a flex column with the footer `mt-auto`.
- `faceH = 156` in `view.go` is about 2 px short (head, tags, IP row and footer come to about 158); measure and raise it to 164.
- The footer stays above the env rows. The rows are a list under the face, and the card ends with their own padding.
- **Test:** `TestMapLayout` still passes; Playwright screenshot.

**M commits:**
- `feat: map node cards show CPU arch`
- `fix: controller card shows docker version`
- `feat: tunnel lines point from the dialling side`
- `feat: internet drawer replaces the popover`
- `feat: tunnel drawer`
- `feat: egress arrows split own traffic from exits for others`
- `fix: map node card footer padding`

---

## Lane D: drawers

### Item 11: sticky drawer head, centred full screen (`drawer_view.templ`)
- In `Drawer`, wrap `panelHeader`, `Strip`, the phone `<select>` and the `tab-strip` in `<div class="sticky top-0 z-20 bg-rw-surface">`. The body `div.p-6` stays the last child of `#drawer-view`: `side-drawer.ts` `onError` prepends the error banner into `lastElementChild`.
- `DrawerHead` gets the same sticky wrapper.
- Full screen: `#drawer-view` gets `[side-drawer[full]_&]:max-w-5xl [side-drawer[full]_&]:mx-auto` (64rem) and `[side-drawer[full]_&]:has-[[data-wide]]:max-w-none`. Wide tables (`vars/grid.templ`) keep the width.
- No TS change expected.
- **Test first:** `components/drawer_test.go`: the header and tabs render inside the sticky wrapper, and the body is the last child. Then Playwright: scroll a long tile drawer, and the tabs stay put.

### Item 13: env stripe at the bottom edge (`layout.templ` `topBar`)
- Drop the `env-band` div.
- When `envClass(s.EnvColor) != ""`, the bar `div` gets the env class plus `shadow-[inset_0_-3px_0_var(--env-c)]` and loses `border-b`. Otherwise it keeps `border-b border-rw-border`. Height is unchanged.
- Lane C deletes the dead `.env-band` rule from `input.css` afterwards (one line, in a C commit or a follow-up).
- **Test first:** a layout render test: the env colour sets the shadow class and drops `env-band`.

### Item 8: node drawer (after W and M)
- **Handler:** `nodeAnswer` also calls `NodeLive` and `NodeTunnels`. `nodeView` fills the new fields, and its inline interface classification goes.
- **Overview, 4 `c.Section` cards with `panel` dl:**
  - **Node:**
    - the status dot and word, mapping `statusTone` success/warn/danger to `bg-rw-success`/`-warn`/`-danger`, consistent with the map's `statusTone`;
    - role, tags;
    - stackr version (`Facts.Version`, "not reported" when empty);
    - last seen (`NodeLive.LastSeen`, "just now" or "5 min ago");
    - cert expiry (the newest valid cert).
  - **Addresses:** one labelled row each for public IPs, LAN IPs, the tunnel address (`WGAddress`) and the env range (`Range`), from `NodeLive.Ifaces`.
  - **Machine:** arch, cores (`plural(n, "core")`, which fixes "1 cores"), memory, disk free, kernel, docker, uptime.
  - **Tunnels:** one row per peer with name, ping, and who dials ("this node dials", "h4 dials", "both dial", "via h4").
    - Each row has `hx-get` of `cluster.TunnelDrawer(a, b)`, `hx-target="#drawer-body"` and `hx-push-url="false"`. A pushed `?drawer=tunnel:` would only restore on `/-/cluster`.
    - These rows are also the keyboard path to the tunnel drawer, since the SVG is aria-hidden.
- **Network tab:** sections Public, LAN, Cluster (label = env path or "tunnel"), Docker. Hidden rows sit in a native `<details><summary>Show all</summary>` with the labels "loopback", "link-local" and "veth".
- **Test first:** in `internal/web/handler/admin/nodes_test.go`, the overview shows "1 core", "amd64" and a tunnel row linking `/-/cluster/tunnel?`. The network tab shows "link-local" inside `details` and a cluster row with the env path.

### Item 10 (UI, after E)
- **Tags tab:** egress hint `tagHelp("egress")` becomes "Internet traffic may leave from this node's IP."
- **Default exit picker** in the same form:
  - up to 3 `<select name="exit">`, pre-filled from `NodeExits().Ladder`, else the first one with `Suggested`;
  - label "Default exit", hint "Used when this node has no egress tag.";
  - options are `NodeExits().Options`.
- The tags POST calls `SaveNodeTags(tags, exits)`. A refusal shows in the drawer's error banner as today.
- **Add node step 1:** the egress box is checked and disabled with the hint "Every node starts with egress." The service adds the tag.
- **Test first:** `nodes_test.go`: POST tags without egress and without an exit gives 422 with "Pick a default exit". With `exit=<h4>` it gives 200, and the tab shows the picker pre-filled.

**D commits:**
- `fix: drawer header and tabs stay put while the body scrolls`
- `fix: full-screen drawer content centred at 64rem`
- `fix: env colour runs along the top bar's bottom edge`
- `fix: node drawer overview in four cards, network tab grouped and labelled`
- `feat: default exit picker on the node's tags tab`

---

## Rig proof (after release)

The rig has 5 nodes:
- 109: the controller, behind a home NAT.
- ct110 and vm106: on the same LAN as 109.
- h4: public.
- h3: Hetzner private network, behind h4.

1. **Upgrade and migration:**
   - `stackr node ls` shows every node tagged egress.
   - `stackr node show <n>` shows `arch`, and the controller row has docker and arch.
   - No env lost internet: `curl -s ifconfig.me` from a tile on each node gives the same IP as before.
2. **Map node cards:** every card, 109 included, reads `docker <ver> · amd64` with padding under the footer, and the stack graph cards too.
3. **Tunnel arrows:**
   - 109 to h4: an arrow pointing at h4.
   - 109 to ct110, 109 to vm106, ct110 to vm106, h3 to h4: no arrow.
   - h3 has no line to 109, ct110 or vm106 ("reached via h4").
4. **Tunnel drawer:**
   - Click the 109 to h4 line. The drawer shows "109 dials", h4's public endpoint, ping, the last handshake on both ends, bytes both ways, and the Ping history placeholder.
   - A reload with `?drawer=tunnel:…` reopens it.
   - A pan that starts on a line doesn't open it.
5. **Internet drawer:**
   - Click the NAT internet card. It shows port forwards and their target node (or the "No port forward" warning), domains with envs, and own vs exit-for-others egress.
   - h4's card shows its public IP.
   - No popover remains.
6. **Egress arrows:**
   - h3's own egress goes to its network's shared Internet card, not its own.
   - Untag h3 egress in its drawer. The picker is pre-filled with h4 (lowest ping). Save. The map shows h3 to h4 `egress-via`, and h4's internet arrow becomes full.
   - From an h3 tile, `curl ifconfig.me` gives h4's IP.
   - Stop h4's exit (or block it): the h3 tile has no internet.
   - On h3, `iptables -S STACKR-FENCE` lists the drop.
   - Put the tag back afterwards.
7. **Refusals:**
   - `stackr node untag h3 egress` with no default exit is refused.
   - `stackr node rm h4` while h4 is h3's default exit is refused.
8. **Node drawer:**
   - Overview shows the four cards. The controller shows its version and last seen "just now".
   - The Tunnels rows open the tunnel drawer.
   - The Network tab groups interfaces; fe80 and lo sit under Show all as link-local and loopback; `br-…` rows are named by env.
9. **Canvas:**
   - Middle-drag on a card pans without selecting or moving it.
   - Middle-click on an env row opens a new tab.
   - Notes sit above cards and lines; a dragged card goes above the notes.
10. **Drawers and top bar:**
    - Long drawers keep the title and tabs pinned.
    - Full screen centres the content.
    - `mn/edge/prod` shows the pink stripe on the top bar's bottom edge with no height change.

## Open questions (each with a recommendation)

1. **Item 3, box annotations.** Box frames share the `note:` id prefix. Raised above the cards, they would cover the cards they frame and block clicks. **Recommend:** raise only kind=note (the `note-kind` attribute); boxes stay behind.
2. **Item 10, the cluster-wide `default_exit` setting.** The item's precedence list leaves it out. Removing it would change routing on upgrade for any cluster that has one set. **Recommend:** keep it, after the node level: env, stack, org, node default (non-egress nodes only), cluster default, own uplink (egress nodes only). Upgrade then changes nothing, and a non-egress node's own picker still wins. The Nodes tab control stays as is; retiring it can be a later decision.
3. **Item 4, both sides dial.** **Recommend:** no arrow, not a double arrow. Every LAN pair dials both ways, so double arrows would cover most lines and add noise.
4. **Item 10, `own` rung in an env, stack or org ladder on a non-egress node.** **Recommend:** strip it during resolution, so that level resolves to its remaining rungs or to no internet. Don't refuse it in `SetExits`: org and stack ladders span nodes with and without egress.
5. **Item 10, join default.** "By default" could mean a pre-ticked box. A node that joins without egress has no default exit and would have no internet. **Recommend:** `CreateJoinToken` always adds egress, and the UI box is shown checked and disabled.

### Critical files for implementation
- /home/darthvader/FyrmForge/stackr/internal/service/cluster.go
- /home/darthvader/FyrmForge/stackr/internal/service/mesh.go
- /home/darthvader/FyrmForge/stackr/internal/ui/cluster/view.go
- /home/darthvader/FyrmForge/stackr/ui/ts/graph-canvas.ts
- /home/darthvader/FyrmForge/stackr/internal/ui/drawer/admin/nodes.templ

## Decided (darthvader 2026-10-14)

All five open questions go with the recommendation: (1) only kind=note rises, boxes stay behind;
(2) cluster default_exit kept after the node level; (3) no arrow when both sides dial;
(4) an own rung on a non-egress node is stripped at resolution, not refused;
(5) CreateJoinToken always adds egress, the UI box is checked and disabled.
All lanes run on Sonnet.

# Step 6: UI

Read first: `docs/rewrite/PROGRESS.md`, then REWRITE.md "UI stack", "Build
order" row 6, "Auth" (nested URLs), "Volumes" (the card rule). Extract:
`docs/rewrite/extracts/templ-ref.md` (what each old screen shows, never
how). `docs/rewrite/verbs.md` lists the verbs the screens call.

templ + htmx, server-rendered, the URL is the state, SSE for live data,
view structs only. Web handlers are as dumb as the API's. Shared components
first; every screen is built from them.

## Custom-element whitelist (the only JS that may exist)

One file, one class, one tag each, under `ui/static/js/`, TypeScript
checked by `tsc --noEmit` in `make lint`, no imports, no fetch, DOM events
up, htmx attributes only in the templ wrapper, no shadow DOM, ~150 lines.
Adding a tag is a plan item, not a build decision.

| Tag | Does | Wrapper |
|-----|------|---------|
| `<log-pane>` | follow-scroll, level filter, search, timestamps toggle, wrap toggle over lines the SSE swap appends | `@LogPane(view)` |
| `<confirm-dialog>` | native `<dialog>` open/close, returns the answer as a DOM event the parent's `hx-trigger` listens to | `@Confirm(view)` |
| `<flash-toast>` | shows htmx error responses and flashes, auto-dismiss | `@Flash()` in the layout |
| `<theme-toggle>` | light/dark, `localStorage` only | `@ThemeToggle()` |

**Superseded 2026-09-24 for tasks 5 to 13:** the graph is v1. Read
`docs/rewrite/ui-plan.md`; it adds `<graph-canvas>`, `<graph-node>` and
`<side-drawer>` to this whitelist and replaces the page lists below with
four canvases plus drawers and dialogs. Tasks 1 to 4 (session A) stand.

Not in v1 (old JS dropped): metrics charts, terminal (xterm + websocket),
the YAML code editor with autocomplete (raw Caddy snippets and any file
view use a plain `<textarea>` / `<pre>`).

## Tasks

1. **Vendor htmx + SSE extension**, pinned, under `ui/static/js/vendor/`.
   `tsc` config, `make lint` runs `tsc --noEmit`, `templint` allowlist gets
   the four tags.
   Done when: lint passes with an empty element file for each tag.

2. **Layout + helper.** `ui/components/layout.templ` (shell, nav by URL,
   flash slot), one helper that reads `HX-Request` and renders full page or
   fragment from the same handler, `hx-push-url` on navigation. Web router
   mounts the step 1 middleware on `/:org/:stack/:env/:tile`.
   Done when: one page renders both ways in a test.

3. **Component set** in `ui/components/`, one templ file each: form (field,
   error, submit), table, status badge (tile state, job state, env colour),
   card (tile card, volume card stacked under its mounting tile or alone),
   dialog wrapper, log pane wrapper, job status (SSE), release diff/plan
   view, param editor (masked secret, merge semantics), settings form built
   from the catalogue, empty state, pagination.
   Done when: a components gallery page renders every one.

4. **Custom elements**, the four above, TypeScript.
   Done when: each under budget, no fetch, no import, events only.

## Tasks 5 to 13 (rewritten 2026-09-24, see `docs/rewrite/ui-plan.md`)

Everything below is a canvas, a drawer or a dialog. No page lists a
thing a drawer also shows. Session B and C run in parallel worktrees;
the element contract in task 5 is the only thing they share, so it is
written and committed before either starts.

### Element contract (the three graph tags)

`<graph-canvas>` wraps one canvas. Children: one `<svg data-edges>` with
a `<path data-edge-kind="…" data-from="<id>" data-to="<id>">` per edge
(server-drawn straight lines; the element re-paths), then one
`<graph-node>` per card. Attributes, all set by templ: `scale` (initial),
`snap`, `straight`, `fan-out`, `arrows`, `focus="<id>"`; the look ones
are overridden from `localStorage` by the element. Events it fires:
`selection-changed` with `detail.ids`. It never fetches, never touches
anything outside its subtree.

`<graph-node>` wraps one card. Attributes: `node-id`, `x`, `y`, `w`, `h`,
`system` (lives in the column behind the divider, cannot cross it),
`static` (not draggable: vars cards, ghost refs). Its light DOM holds the
templ card plus `<input type="hidden" name="x">` and `name="y"`, which the
element writes on drop, then fires `node-moved` (bubbles). The templ
wrapper carries `hx-post="…/positions" hx-trigger="node-moved"
hx-include="this" hx-swap="none"`. Click after a drag is swallowed
(`stopImmediatePropagation` inside its own subtree). Card click is
`hx-get="…/drawer/<kind>/<id>?tab=…" hx-target="#drawer-body"
hx-push-url="?drawer=<id>&tab=…"`; drill-down cards are `<a href>`.
Multi-select drag: the canvas moves every selected node and each fires
its own `node-moved`; the server takes them one by one.

`<side-drawer>` is in the layout once. Attributes: `open`, `tab`. Its
light DOM has `#drawer-body` (the htmx target) and a close button.
Escape and backdrop close it and fire `drawer-closed`; the wrapper's
`hx-on::drawer-closed` is not allowed (inline), so the element itself
removes `?drawer` and `?tab` from the URL with `history.replaceState`,
the one place an element touches the URL. Tab links inside the body
are plain `hx-get` with `hx-push-url`.

Sub-tiles (attached volume, hosting instance, replicas 2+) are ordinary
`<graph-node static>` children of their parent node, offset by CSS;
they move with it because they are inside it.

**Added by task 5 (session B, 2026-09-24)** — names the templ must render
or may style; `elements_test.go` checks each one is in the source:

- `<graph-canvas divider="<x>">`: world x of the system-column wall.
  `system` cards keep `x + w <= divider`, the rest `x >= divider`. No
  attribute = no wall. The divider line itself is the server's (an SVG
  `<line>` in `svg[data-edges]`).
- Canvas children besides the SVG and the nodes are overlay chrome, not
  transformed: `input[type=checkbox][data-look="snap|straight|fan-out|arrows"]`
  (the element syncs them to its attributes and to `localStorage`
  `graph.<look>` = `"1"`/`"0"`) and `button[data-zoom="in|out|fit"]`.
- Arrows: the SVG carries `<marker id="graph-arrow">` (fill
  `context-stroke`); CSS `graph-canvas[arrows] path[data-edge-kind]`
  sets `marker-end`. The element only flips the attribute.
- Set by the elements, for CSS only: `selected` and `lit` on top-level
  nodes, `focusing` on the canvas while a card is hovered or focused,
  `data-lit` on lit paths, `dragging` on the node being dragged, a
  transient `div[data-marquee]` child during shift+drag. The canvas
  writes `--px`, `--py`, `--s`; a node writes `--x`, `--y` and its
  `width`/`height` from `w`/`h` (so `w`/`h` are required on top-level
  cards). The transforms live in `ui/css/input.css`.
- Multi-drag is run by the dragged `<graph-node>`: when it is
  `selected` it moves every `graph-node[selected]` child of its canvas.
  Each still fires its own `node-moved`.
- The position POST learns the card from a third hidden input,
  `<input type="hidden" name="node_id">`, next to `x` and `y` (all three
  direct children of the `<graph-node>`; `hx-include="this"` sends them).
- The `<graph-node>` also carries `hx-disinherit="*"`: without it the
  card's drawer `hx-get` inherits `hx-swap="none"` (the drawer opens
  empty) and `hx-include="this"` (x/y/node_id ride on the GET).
- `<side-drawer>` light DOM: `[data-backdrop]`, `[data-close]`,
  `#drawer-body`. It opens on any `htmx:afterSwap` whose target is inside
  `#drawer-body` and copies `?tab=` into `tab`. Wrapper:
  `components.SideDrawer()`, in `Layout` after `#main`.

**`graph.View` (task 6, session B)**: what `service.Canvas` returns and
the card templates read (`internal/service/internal/flow/graph`).

- Node ids: `org:<id>`, `stack:<id>`, `env:<id>`, `connector:<id>`,
  `vars` (one card per canvas, counts only). On the env canvas, row ids
  as task 9 asks (the Traffic verb's lane ends): the tile id, the
  provision id (slice), the instance tile id (ghost of an instance in
  another scope), the volume id (detached only; attached ones are
  sub-tiles), plus `ref:stack.<slug>` / `ref:org.<slug>` ghosts
  (`Static`), `proxy`, `internet` (`System`). `note:<id>` (annotations)
  on every canvas.
- `Node`: `ID Kind Name Slug Detail Status X Y W H Saved System Static
  Color Deck Subs`; tile facts `Replicas Running Domains Volumes Host
  LastRun Waiting`; vars counts `Params Secrets`. `Kind` is the tile kind
  for tiles (`service image managed cron function`), else `org stack env
  connector vars slice ref volume proxy internet`. `H` already includes
  30 px per sub-tile; a detached volume is 62 tall, the rest 96.
- `Status` is one word, worst-of on drill-down cards: `error > building |
  queued | waiting > unhealthy > degraded > stopped > running > done >
  none`; "" = nothing to roll up.
- `Sub`: `ID Kind Name Status` (volume, the hosting instance under a
  slice as its tile id, replicas 2+ as `replica:<tile id>:<n>`, cap 3).
- `Edge`: `Kind From To`, kinds `ref ingress egress shared startup config
  source`. A startup edge is dropped when a ref already joins the pair.
- `View`: `Nodes Edges Notes Divider Compare`; `Divider` 0 = no system
  column. `Compare` (stack canvas) is the ladder in order with each env's
  release number and `Behind` (the rung below runs a newer one).
- `Show{System, Refs, Startup, Traffic}` is what the query params turn
  off; `Traffic` off drops the egress edges and the internet card.

**Canvas pages (task 7, session B)**: `internal/ui/graph` (`Page`,
`Canvas`, `Footer`, view structs) and `internal/web/handler/canvas`, one
handler for the four levels.

- Pages: `/`, `/:org`, `/:org/:stack`, `/:org/:stack/:env`. Helper
  routes under `<page>/-/` (`-` is never a slug): `GET events?n=<sig>`, `POST positions` (node_id, x, y), `POST reset`,
  `POST notes` (id?, kind, text, x, y, w, h), `POST notes/delete` (id).
  Reads need `org.read` (home: signed in); writes `org.graph.write`,
  `stack.write`, `env.write`.
- Show params `system|refs|startup|traffic=0` ride on every helper route.
- `#graph` wraps the canvas with `sse-connect` and `sse-swap="graph"`; the
  stream sends `graph` (whole `Canvas`) when the node, edge or note set
  moves off the page's `n`, and `footer:<id>` (every footer at connect,
  then on change). `canvas.(*handler).Poll` is that producer; the env
  stream (`/:org/:stack/:env/events`) folds it in at the merge.
- `ui.Node.Card` / `ui.Node.Footer` replace the generic body and footer:
  the env mapping sets them from `cards.Card`/`cards.Subs`/`cards.Footer`.
- A fresh load of `?drawer=<node id>&tab=` renders `<side-drawer open
  tab>` with the tab inside (`render.PageWith`, `components.Shell.Drawer`):
  the node is looked up in the canvas just drawn, its scope resolved from
  its slug, then the same render as the drawer route (task 8). An id not
  on the canvas, or one the viewer may not open, leaves it closed.

**Drawers and dialogs (task 8, session B)**: a card's drawer lives at the
card's own path, so the access middleware resolves and checks it; tabs
are `?tab=`, each tab's own verb is asked in the handler. Every answer is
`components.Drawer` (`#drawer-view`, header, tab strip pushing
`?drawer=<node id>&tab=`, error or note banner) around one tab templ from
`internal/ui/drawer/{org,stack,env,connector,vars}`; an action is one
verb, then its tab again (422 with the refusal over it).

- `GET /:org/-/drawer` org: settings (rename, delete) `org.write`,
  members + invites (`member.list`; role, remove, invite `member.manage`),
  keys (the viewer's own, mint and revoke), params, backups
  (`destination.write`).
- `GET /:org/:stack/-/drawer` stack: settings (rename, config repo,
  delete) `stack.write`, params, releases (list).
- `GET /:org/:stack/:env/-/drawer` env: settings (rename, from/auto,
  colour, delete), params, order (the ladder, one move per post), logs
  (placeholder); `env.write`.
- `GET /:org/-/connectors/:connector`: settings (rename, delete)
  `connector.write`, repos (placeholder).
- `GET <level>/-/vars` at org, stack and env: the vars card, the same
  editor as every params tab; `POST <level>/-/vars` and `/-/vars/delete`
  (`variable.write`) answer `#vars-editor` via `HX-Retarget`. Secrets are
  read only with `variable.write` and only "set" reaches the view.
- Create dialogs open in the drawer from buttons beside the compare pill
  (`graph.View.Create`, only the ones the viewer's verbs pass):
  `/-/new-org` (`org.create`: draft, rename, finish), `/:org/-/new-stack`
  (`stack.create`), `/:org/:stack/-/new-env` (`env.write`),
  `/:org/-/new-connector` (`connector.write`: begin, then a plain form
  posting the manifest to GitHub). Create answers `HX-Redirect` to the new
  card's page, a refusal re-renders the form (422). Delete confirms
  (`dialog.DeleteOrg/Stack/Env/Connector`) redirect to the canvas above.
- `<graph-canvas>` keeps pan/zoom per path in `sessionStorage`
  (`graph.view.<path>`), so a `graph` swap or a reload keeps the view, and
  re-paths on `childList` too (a swapped-in lanes svg).

**Added by task 9 (session C, 2026-09-24)** — what task 7's node wrapper
and stream must match:

- Cards live in `internal/ui/graph/cards` with their own views
  (`CardView`, `FooterView`, `SubView`, `Lane`). The task 7 wrapper
  renders `<graph-node node-id={v.ID} system?={cards.System(v.Kind)}>`,
  its three hidden inputs, then `@cards.Card(v)` and `@cards.Subs(v)` as
  siblings (the gallery's `envNode` is the model).
- Node ids: tile id (service, cron, function, managed, ref), provision
  id (slice), volume id, and the words `proxy`, `internet`, `vars`,
  `secrets`. These are the ends the Traffic verb names, so a lane needs
  no mapping.
- Card click: `hx-get={v.Drawer}` into `#drawer-body`, pushing
  `?drawer=<id>&tab=<v.Tab>`. Sub-tiles are `<graph-node static>` with
  their own drawer GET.
- Footer: one `div[sse-swap="footer:<id>"][hx-target=this]` swapped
  outerHTML; the stream sends `cards.Footer(id, f)` rendered.
- Lanes: `<svg data-edges data-lanes id="graph-lanes">` swapped whole by
  event `traffic`; paths `data-edge-kind="traffic"`, labels on a
  `<textPath>`. Stream: `GET /:org/:stack/:env/events`
  (`internal/web/handler/env`), which task 7 folds its `graph` and
  `footer:<id>` events into. SSE bodies go out as `stream.HTML` via
  `render.Event`, never raw text.
- **Merge need:** `<graph-canvas>` repaths only on node attribute
  changes; it must also watch `childList` on itself so a swapped-in lanes
  svg gets its `d` (DECIDE 100).

**Added by task 10 (session C, 2026-09-24)** — the env-level drawer URLs
task 7's cards and the env page point at (all under `/:org/:stack/:env`):

- `drawer/tile/:tile?tab=` (tabs by kind: `tile.Tabs`),
  `drawer/instance/:tile`, `drawer/slice/:provision?tab=bindings`,
  `drawer/volume/:volume?tab=backups`, `drawer/proxy?tab=routes`,
  `drawer/new-tile` (the create form, opened into the drawer),
  `drawer/rollback/:release` (POST, answers a live `JobStatus`),
  `drawer/jobs/:job/events` (every drawer's job stream). The vars and
  secrets cards open session B's `drawer/vars`.
- Every drawer answer is one `<div id="drawer-view">`; tabs, actions
  and confirms replace it outerHTML, so nothing ever targets
  `#drawer-body` but a card click.
- The env page must open `?drawer=&tab=` on a full load itself (a
  create redirects to `…?drawer=<tile id>&tab=status`).

### Session B (`../stackr-step-6b`)

5. **Contract + elements.** Write the contract above into
   `internal/ui/components/elements_test.go` (seven tags), build
   `<graph-canvas>`, `<graph-node>`, `<side-drawer>` in `ui/ts/`.
   Done when: gallery shows a fake canvas with three cards, drag saves
   into the hidden inputs, marquee selects, drawer opens and closes.

6. **`graph` service** (`internal/service/graph.go` + `internal/service/
   internal/flow/graph`): `Canvas(scope) graph.View` for the four levels.
   Cards, sub-tiles, edges (ref, ingress, egress, shared, startup,
   config, source), worst-status roll-up, ghost refs, env-compare pill,
   arrange for unsaved cards. Positions: `positions(scope, node_id, x, y)`
   table, shared per canvas. Annotations: `annotations(scope, id, kind
   note|box, x, y, w, h, text)`. Verbs `Canvas`, `SetPosition`,
   `ResetPositions`, `Annotations`, `SetAnnotation`, `DeleteAnnotation`.
   Done when: view tests per level with a fixture tree.

7. **Canvas templ + handlers.** `internal/ui/graph/` (canvas, node,
   edge, legend, note, box from `graph.View`); handlers in
   `internal/web/handler/<level>/`: `GET` canvas (full page or fragment),
   `POST positions`, `POST reset`, annotations CRUD, `GET events` SSE
   (per-card footer swaps, a `graph` event that swaps the whole
   fragment on add/remove). Query params for what the server draws
   (system cards, ref/startup edges, traffic).
   Done when: all four levels render from the fixture tree; SSE swaps a
   footer in a test.

8. **Home, org, stack drawers + dialogs** under `internal/ui/drawer/
   {org,stack,env,connector,vars}` and `internal/ui/dialog/`: the rows
   for home, org and stack in ui-plan §5. Every tab is one templ, one
   handler, one verb call.
   Done when: each tab renders from a view struct in a test; create
   dialogs round-trip through the API in a handler test.

### Session C (`../stackr-step-6c`, needs task 5 merged into its base)

9. **Env canvas cards.** Card templates for service, cron, function
   (schedule / "manual" / "on deploy" detail, last and next run footer),
   managed instance, slice, ghost ref, detached volume, proxy, internet,
   vars/secrets; chips: host, new version, volumes, replica roll-up,
   exposure; traffic lanes from the `traffic` SSE event (step 3c).
   Done when: every card kind in the gallery; lanes repaint in a test.

10. **Env and tile drawers + dialogs**: the env row of ui-plan §5.
    Tile drawer tabs: status + last job (SSE), logs, domains (+ admin raw
    snippet), env block, settings (catalogue), jobs, image watch, runs
    (cron/function: list, run now, pause), backups. Instance, slice,
    volume, proxy, vars drawers. Create-tile dialog (source switch, repo
    pick), confirm for restart/stop/delete/rollback.
    Done when: same as task 8.

### Session D (after B and C are stacked)

11. **Full pages + admin drawer.** Login, setup, invite accept, CLI
    authorize (DECIDE 44), account; `GET /settings/github/callback`;
    admin drawer from the nav: server settings, users, update
    (check/run/badge, the upgrade route step 5 left), raw Caddy, panel
    backup.

12. **Handler audit + Playwright smoke** on the VM: login → create org →
    stack → env → deploy an image tile → drag a card and reload (position
    kept) → open its drawer → promote dry run → promote → rollback →
    run a function tile and read its log.

13. **Done gate.** build/lint/test/templint; every element under budget;
    audit empty; smoke green; PROGRESS.md ticked; stacked PRs.

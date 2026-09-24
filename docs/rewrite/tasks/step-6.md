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

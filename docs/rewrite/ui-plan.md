# UI plan (step 6, sessions B to D)

Agreed with darhvader 2026-09-24, one point at a time. Supersedes the
"Not in v1" line of `docs/rewrite/tasks/step-6.md` and REWRITE.md's
"Later" for the canvas. Reference for look and behaviour: v0
(`docs/rewrite/extracts/graph-ref.md`, `templ-ref.md`). Reference for
code: none. v0's front end is spaghetti; nothing is copied.

## 1. The rule

**Everything is a graph.** Each level of the tree is one canvas. Anything
that is not a card on a canvas is a **drawer** (edit, inspect, lists) or
a **dialog** (confirm, create). There is no third surface: a thing has a
drawer *or* a page, never both. Full pages exist only where a canvas
makes no sense: login, setup, invite accept, CLI authorize, account.

## 2. Four levels, one element

| URL | Cards | Edges |
|---|---|---|
| `/` | orgs (deck look, worst-status) | none |
| `/:org` | stacks (deck), connectors, org vars/secrets count card | config, source (connector → stack), shared (vars → stack) |
| `/:org/:stack` | envs (env colour, deck, worst-status), stack vars/secrets | shared |
| `/:org/:stack/:env` | tiles: service, cron, function, managed instance, slice (`resource`), ghost `ref`, detached volume; system column behind the divider: proxy, internet; env vars/secrets | ref, ingress (proxy → tile), egress (tile → internet), shared, startup, traffic lanes |

Sub-tiles under a card: attached volumes, hosting instance under a slice,
replicas 2+. `host:ports` card and `port` edges are out (port forwarding
and published ports are Later).

One canvas element for all four levels. The server decides what cards
exist (a `graph` service fed view structs: worst-status ranking, ghost
refs, arrange). The element never knows which level it is on.

## 3. Three custom elements (whitelist additions)

| Tag | Does (geometry only, never fetches) | Budget |
|---|---|---|
| `<graph-canvas>` | pan, wheel/pinch zoom, clamp, fit/reset, marquee, SVG edge re-path (sides, curve, fan-out), hover focus, `?focus=`, fires `selection-changed` | <300 |
| `<graph-node>` | drag with slop, snap to 22 px grid, no-overlap nudge, sub-tiles follow, writes `x`/`y` into its own hidden inputs, fires `node-moved` on drop, swallows the click that ends a drag | <150 |
| `<side-drawer>` | open/close, tabs by URL param, Escape, backdrop; content arrives by htmx swap | <150 |

Plus the four from session A (`log-pane`, `confirm-dialog`,
`flash-toast`, `theme-toggle`). Seven tags total; `elements_test.go`
enforces the list.

## 4. htmx on the templ node

- Card click: `hx-get="…/drawer"` into `<side-drawer>`, `hx-push-url`
  with `?drawer=<id>&tab=<name>` so the URL is the state.
- Drill-down cards (org, stack, env): plain `<a href>`, `hx-boost`.
- `node-moved`: `hx-post="…/positions" hx-trigger="node-moved"
  hx-include="this"`, hidden `x`/`y` inputs (graph-ref DECIDE 3, option
  a). Positions table: `(scope, node_id, x, y)`, per user? No: per canvas,
  shared, like v0.
- Re-arrange: `hx-post` reset, server re-renders the canvas fragment.
- View toggles that change what the server draws (system cards, ref and
  startup edges, traffic) are query params; pure look (arrows, straight,
  fan-out, snap) are element attributes set from `localStorage` by the
  element itself.
- Live data: one SSE stream per canvas (`…/events`): `sse-swap` per card
  footer (status, replica roll-up, last run, new-version chip), a
  `graph` event that swaps the whole canvas fragment when a card is
  added or removed, a `traffic` event that repaints lanes.

## 5. Drawers and dialogs per level

Drawer = one per card kind, tabs by `?tab=`. Dialog = confirm or create.

| Level | Drawers (tabs) | Dialogs |
|---|---|---|
| home | org: settings (rename, delete verdict), members + invites, keys, params, backup dests | create org, confirm delete |
| org | stack: settings (defaults, config repo), params, releases (list, diff, promote plan, rollback); connector: rename, repos; vars/secrets: editor | create stack, install connector (redirect), confirm |
| stack | env: settings (`from`/`auto`, colour, PR badge), params, logs, order | create env, confirm delete |
| env | tile: status + last job (SSE), logs, domains (+ admin raw snippet), env block, settings (catalogue), jobs, image watch, runs (cron/function: list, run now, pause), backups; managed instance: provisions/slices/bindings; slice: bindings; volume: backups (schedules, runs, restore); proxy: routes; vars/secrets: editor | create tile (source switch, repo pick), confirm restart/stop/delete/rollback |
| any | admin (nav, not a card): server settings, users, update, raw Caddy, panel backup | confirm upgrade |

Auth pages stay full pages. The admin surface is a drawer opened from
the nav, not a card, because it belongs to no level.

## 6. Code shape (so it does not grow arms and legs)

- `internal/ui/graph/`: `graph.templ` (canvas, node, edge, legend from
  view structs), `view.go` (the structs), nothing else. Every level's
  handler builds the same `graph.View` from the `graph` service.
- `internal/ui/drawer/<kind>/`: one package per card kind, one templ per
  tab, each handler = verb call + view struct + render. No drawer knows
  another drawer.
- `internal/ui/dialog/`: create forms and the confirm wrapper.
- Handlers under `internal/web/handler/<level>/`: canvas GET, events
  SSE, positions POST, and the drawer routes for that level's kinds.
- No JS outside the seven elements. No inline scripts. No
  `hx-vals="js:"`.
- `graph` service (`internal/service/graph.go`) computes worst-status,
  ghost refs, edges, arrange. Handlers only map to view structs.

## 7. Sessions

- **B** (after 3b lands, worktree): `graph` service, `<graph-canvas>`,
  `<graph-node>`, `<side-drawer>`, home + org + stack canvases and
  their drawers, positions, SSE events.
- **C** (parallel to B, worktree): env canvas cards and edges (needs B's
  element API: agree the event names and attributes in
  `docs/rewrite/tasks/step-6.md` first), every env drawer, create-tile
  dialog, runs tab, traffic lanes (3c).
- **D**: auth full pages, admin drawer, CLI authorize page, GitHub
  callback, upgrade button, handler audit, Playwright smoke on the VM
  (login → org → stack → env → deploy → drag a card → open drawer →
  promote → rollback), done gate.

## 8. Decided (darthvader, 2026-09-24)

1. Positions shared per canvas, like v0.
2. Traffic lanes are v1 (step 3c). Shape: `leaf/traffic` pure sampler,
   one `flow/schedule` entry, one `Traffic(env)` verb. Consumer → managed
   instance traffic is attributed to the slice through the consumer's
   binding row and drawn on the ref edge. Egress (destination outside
   the map) rolls up to an `internet` system card; ingress comes from
   the `proxy` card. Port forwards join the system column later.
   Transactions per second is a Postgres stat, later/metrics.
3. Marquee select and multi-drag: keep.
4. Notes and boxes: yes, one `annotations` table, drawn as cards.
   Groups and undo: no.
5. Env-compare pill and startup edges: yes.

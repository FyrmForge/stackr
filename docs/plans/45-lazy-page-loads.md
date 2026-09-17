# Plan: pages that wait on something slow

Status: proposed 2026-09-16, reviewed against the code the same day.
Implemented and rig tested 2026-09-17 (two nodes, worker agent paused,
registry stopped). Not tested: GitHub unreachable (no sudo on the manager to
block outbound). Where the build differs from the text below:

- Item 1 is one file. The `HaveBehind` line is in `commitLog`, not `Graph`.
  Also added a mutex on the older map: every request shares it until the head
  moves, and concurrent writes crash the process.
- Item 3 keeps the settings tab's `volumeView` call (the attach dropdown
  reads its services) and only drops the disk scan. The warning moved inside
  the size cell.
- Item 4: the websocket room and the 120s poll sit on a wrapper. The poll
  re-fetches the page with `hx-select`, so a node that joins or leaves shows
  up without a reload. The wrapper carries `hx-disinherit="*"`, or the
  sections inherit the select and swap in nothing. Each section URL carries
  the hostname, so a section is one agent call and no node lookup.
- Item 5 renders "node not answering" only for dial and deadline errors,
  logged as a warning. Docker's "No such container" is a 404, and anything
  else (a bad node id, a stale agent, a bad key) is the error itself.
- A shared `components.PageCtx` is the 5s bound, and `components.Loading` the
  placeholder, at every site.
- Item 6: a failed info call now says "No answer from this node" instead of
  showing nothing.
- Item 7 is one fragment, `.../registry/images?image=`. Image links stay full
  page links; image names contain slashes, so a path parameter was awkward.
- Item 8 is `?body=1` on the step route, not a new route.
- Item 9: org uses an inline timeout, it has only the one call.
- The node fragments (containers section, server host and volumes) are
  bounded at 5 seconds too. Found on the rig: unbounded, a hung agent held
  them for the 30 second request timeout, which answered 500 and the "no
  answer" line never showed.

Working rules: discuss first, one point at a time, no code without a go, no
git writes, no edits to `*_templ.go` or `output.css`, terse UI copy.

## Where this came from

Reviewing the panel upgrade (plan 43). The update page called GitHub inline
and stalled for its whole timeout on an offline host. Fixed by splitting the
check into `/admin/update/check`, loaded with `hx-get` on page load. A sweep
of the other full-page GET handlers found the same shape in nine places.
This plan lists them, ranked, with the fix that actually holds after reading
the templates and callers.

## The rule

A full page renders from the store. Anything that talks to docker, a node
agent, GitHub or the registry either:

- **loads after** the page, as a fragment with `hx-get` and
  `hx-trigger="load"` (the pattern in `app/panel.templ` for vars,
  provisions and backups), or
- **is bounded** with a short context timeout when the result has to be in
  the page itself, and a miss renders as *unknown*, never as a wrong value
  and never as no page.

Lazy fragments for lists and drawers. Timeouts for chips inside a rendered
structure. A bounded call must have an unknown state to land in; a timeout
that leaves a zero behind is a lie, not a fix (see items 1 and 2).

The timeout helper. No handler package shares code today and there is no
timeout convention in the tree (the only `context.WithTimeout` calls are
background work in `infra/runtime`). So: a two line `pageCtx(ctx)` with a
5 second deadline, copied into each handler package that needs it. Five
copies of two lines beat a package for two lines. The agent transport keeps
its own limits (5 second dial, 30 second response header); the page must not
inherit the 30.

## Ranked list

### 1. Env and stack canvas: older commit lookups (hot)

`handler/project/commitlog.go`, `olderCommit`. Two GitHub calls per SHA
outside the 10 commit window, each under the GitHub client's own 15 second
timeout, so up to 30 seconds per SHA. A failed lookup is not remembered, so
it runs again on every render of the env canvas, stack canvas, releases
page and promote dialogue. GitHub rate limited means every render pays.

The commit list itself is fine: `fetchCommits` caches for 60 seconds and
caches failures too.

The trap. A failed lookup today returns `olderInfo{onBranch: true, behind:
0}`, and `Graph` copies that into `row.HaveBehind = o.onBranch`. So a miss
already reads as "on branch, at head" to `behindOf`, `promoteSpan`,
`planWaiting` and the runs chip. Caching that as is would make the wrong
answer stick for a minute.

Fix:

- `olderInfo` gains `known bool` and `checked time.Time`. A miss is stored
  with `known: false` and retried only after the same 60 seconds
  `fetchCommits` uses.
- `Graph` sets `row.HaveBehind = o.known && o.onBranch`. Unknown rows stay in
  `Older` (not `OffBranch`, which renders as a force push) with no behind
  chip. `behindOf` already returns "not known" for a row without
  `HaveBehind`, so the releases page and promote dialogue fall back to their
  existing unknown wording without a change.
- The lookups run under `pageCtx`.

Not lazy loading the log rail. The card chips read `behindOf`, so the log
has to be there when the graph renders. Bounded and cached is enough.

Files: `project/commitlog.go`, `project/handler.go` (the one line in
`Graph`). Two.

### 2. Env canvas: placement chips (hot)

`handler/project/placement.go`, `markPlacement`. Per tile: three store
reads for the network, then `ServiceTasksOnNetwork`, which is a network
inspect plus a task list on the manager socket. No timeout. The comment at
the top says a slow swarm call must cost a chip, not the page. Today it
costs the page. And it runs on a single node too: `multiNode` only decides
whether node names are shown, the loop runs regardless, and the replica
roll-up applies on one node as well.

The trap. A failed `ServiceTasksOnNetwork` does not skip the tile. The
`err == nil` guard wraps only the append, so the tile enters `byTile` with
`Want: 3, Tasks: nil`, and `graph/placement.go` turns that into `Replicas{
Want: 3, Running: 0}`, which the canvas paints amber as "degraded 0/3". A
timeout as the only change would paint every healthy replicated tile as
degraded, and tiles after the deadline would fail at the store read and get
no chip at all. Two contradictory states on one canvas.

Fix:

- `TileTasks` gains `Unknown bool`. A failed task list sets it instead of
  leaving an empty task list, and `graph/placement.go` skips the replica
  roll-up and node chip for an unknown tile. Nothing painted, which is what
  the comment promises.
- `pageCtx` around the whole of `markPlacement`.
- Early return when there is one node and no tile wants more than one
  replica. That is the common case and it then costs nothing.

Files: `project/placement.go`, `graph/placement.go`. Two.

### 3. Volume tile page: size on disk (hot)

`handler/app/handler.go`, `volumeView`. `InspectVolume` is a host wide
docker disk usage scan plus a full container list, not a walk of one
volume. It runs inline for the overview tab. Three spots in
`app/panel.templ` read the result: the size line, the `OverLimit` border
class on the panel, and the "past its expected size" paragraph.

Also: the settings tab calls `volumeView` too, and `volumeSettings` never
reads the size. That scan is paid for nothing.

Fix:

- `GET /apps/:id/size` returns the size line and the warning paragraph as
  one fragment. The over limit border moves off the panel and onto the
  fragment's own box, so all three readers live in one swap.
- `volumeView` stops calling `InspectVolume`. The fragment handler does.
- The settings tab's call is deleted, not moved.

Files: `app/handler.go`, `app/panel.templ`, `web/server.go`. Three.

### 4. Containers list: one call per node, in series (admin)

`handler/container/handler.go`, `List`. On a multi node swarm it calls
`ListAll` on each node one after another. A halted node costs the 5 second
dial per node; a node that connects and hangs costs 30. Two bad nodes,
double that, before the page shows anything.

What the page carries that a split has to keep, from
`container/container.templ`: the table is `#containers-table` with
websocket refresh (`data-ws-room`, `data-ws-refresh`, `hx-trigger="refresh,
every 120s"`, `hx-select` on itself), fed by `notifier.Containers()` after
every start, stop and remove. The "N stopped, not listed" line above the
table is computed over all nodes. The unreachable banner is already inside
the swapped region.

Fix:

- The page renders one section per node from `ListNodes`, each section a
  fragment, `GET /containers/node/:id?stopped=`. The browser fetches them in
  parallel, so a bad node delays only its own section, and the unreachable
  banner becomes that section's content.
- Refresh wiring moves onto each section: same room, same trigger,
  `hx-select` on the section. One websocket event refreshes every section.
- The stopped counter becomes per section, under its own heading. Cheaper
  than a second round trip to total it, and the operator reads one node at a
  time anyway.
- The single node path stays inline. The local socket does not stall.

Files: `container/handler.go`, `container/container.templ`,
`web/server.go`. Three.

### 5. Container detail, logs and terminal pages (admin)

`container/handler.go` `Detail`, `LogsPage`; `container/terminal.go`
`TermPage`. Each inspects the container on its node before rendering. All
three templates take the inspect result and read the name from it, and the
URL only carries the id. All three return 404 on any inspect error, so a
dead node and a deleted container look the same today.

Fix:

- `pageCtx` on the inspect and, in `Detail`, the stats sample.
- On a deadline (`context.DeadlineExceeded`), render the page with the
  short id in place of the name and a "node not answering" line. Any other
  error stays a 404: a deleted container must not get a page.
- Logs and terminal are shells for a stream, so they render fine on the
  shortened header. Detail renders its actions disabled.

Files: `container/handler.go`, `container/terminal.go`,
`container/container.templ`, `container/terminal.templ`. Four.

### 6. Server detail and the remove dialogue (admin)

`handler/server/handler.go` `Detail`: docker info and the volume list go
through the node's agent. They land in two regions of `server/server.templ`:
the stat tiles near the top (docker version, host, CPU, containers) and the
volumes section further down (count, total bytes, rows). One fragment
cannot fill both.

`handler/server/nodes.go` `RemoveForm`: the volume list goes through the
agent of the node being removed, which is often the node that is already
gone. Display only; removal is gated by the pinned check and the typed
name.

Fix:

- Detail: two fragments, `GET /servers/:id/host` for the stat tiles and
  `GET /servers/:id/volumes` for the volumes section. The metrics charts and
  settings render from the store at once.
- RemoveForm: `pageCtx` on the volume list. On a deadline the existing
  `volErr` path says the node did not answer and lists nothing. The remove
  still runs.

Files: `server/handler.go`, `server/nodes.go`, `server/server.templ`,
`web/server.go`. Four.

### 7. Org registry settings (admin)

`handler/org/registry.go`, `renderRegistry`. Lists the catalog, then with
`?image=` open reads one manifest per tag, in series, 15 seconds each on a
dead registry. The credentials table on the same page is what you need when
the registry is down, and it waits behind the catalog call.

`DeleteRegistryTag` redirects back to `?image=<name>` twice, to keep the
image open after a delete. Nothing else in the tree uses `?image=`.

Fix:

- The image list becomes a fragment, `GET
  /orgs/:slug/settings/registry/images`. Opening an image is an `hx-get` to
  `.../images/:name` that returns the tag rows. The credentials table
  renders from the store at once.
- `?image=` stays as a page parameter that tells the image list fragment
  which image to open on load, so the two delete redirects keep working
  unchanged. Later the delete can target the tag rows fragment directly;
  not in this pass.
- Tag rows stay serial inside their fragment. ponytail: fine up to a few
  dozen tags per image; parallelise inside `tagRows` if an image grows past
  that.

Files: `org/registry.go`, `org/registry.templ`, `web/server.go`. Three.

### 8. Setup wizard, connector and config steps (admin, onboarding)

`handler/org/setup.go`, `connectorRepos`. Lists GitHub repos per connector
inline, no cache. Called for step 2 and again for step 3. In
`org/setup.templ` the repo list is not a list inside a form: step 2's whole
body is one of three branches keyed on whether there are connectors and
whether they list any repos, and step 3's repo picker is filled from the
same call. There is no server side gate on the list; it only decides which
body renders.

Fix:

- Steps 2 and 3 render their frame at once with the step body as a
  fragment, `GET /orgs/:slug/setup/:step/body`, that does the GitHub call
  and picks the branch. "Reading repositories" in its place while it loads.
- The "installed nowhere" warning is one of those branches and moves with
  them. Step 3 never shows a bindable form before the list is in, because
  the form is inside the fragment.

Files: `org/setup.go`, `org/setup.templ`, `web/server.go`. Three.

### 9. Forward chips: proxy relay list (hot, low risk)

`handler/project/handler.go` (two call sites) and `handler/org/graph.go`
call `rt.ListProxyRelays` inline on canvas and org graph render, for the
forward chips and the forwards roll-up. Manager socket, one call, so it
does not stall on a dead node, but it is the same shape as item 2 and the
same page.

Fix: `pageCtx` on the call. A deadline drops the chips. Same unknown rule
as item 2: no chip, not a zero.

Files: `project/handler.go`, `org/graph.go`. Two.

## Order

1. Items 1 and 2 first. Hot path, four files, and both need the unknown
   state that the rest of the plan relies on.
2. Item 3, the volume tile. Hot, three files, plus one deleted call.
3. Items 4 and 5 together, they share the container handler.
4. Items 6, 7, 8, 9 in any order.

Each item is its own go. Each ships with the rig check below.

## Not doing

- A general async job model for page data. The two patterns above cover
  every case found.
- Parallelising agent calls inside a single handler. Where several nodes
  are involved the browser already fans out over fragments.
- Lazy loading the commit log rail on the canvas. See item 1.
- A shared handler package for the timeout helper. Two lines, copied.
- The database volume drawers (`db/handler.go`, volume and bucket panels).
  They open on click, not on page load, and the disk scan is what the
  drawer is for.
- The servers list. `nodes.Sync` is manager socket only.

## Verification

On the rig, two nodes, one worker halted:

- Env canvas renders inside a second. Replicated tiles show no placement
  chip, not "degraded 0/N". On the single node rig the canvas makes no
  swarm calls at all.
- Containers page renders at once. The dead node's section shows
  "unreachable" on its own, and a stop on a live node refreshes every
  section.
- Server detail for the dead node renders the charts; the host tiles and
  the volumes section each say it did not answer.
- Remove dialogue for the dead node opens and removes it.
- A container page on the dead node shows the short id and "node not
  answering". A deleted container's URL still 404s.

With outbound network blocked on the manager:

- Env canvas, releases page and promote dialogue render at once. Older
  commits show with no behind chip and are not listed as force pushed. A
  second render inside a minute does not wait again.
- Setup steps 2 and 3 render their frame, the body says it could not read
  GitHub.
- Registry settings shows the credentials table with the registry stopped.
  Deleting a tag still lands back on the open image.
- Volume tile settings tab opens with no docker disk scan in the request
  log.

# QA loop runbook

Reusable. Run it again after any release that touches the surface below.

A temporary findings document is written per round; this file is the recipe.
Fixed findings belong in code and open findings belong in `docs/notes.md`.

## How the loop runs

One area at a time, in the order below. Per area:

1. Run every check in the area, recording pass / fail / not testable.
2. Stop and write the failures into the round doc before fixing anything —
   a fix made mid-sweep hides which check caught what.
3. Fix, then re-run only the failed checks plus anything that shares the
   root cause (grep the callers, not just the reported path).
4. `make test`, `make lint`, `make templint` before moving to the next area.

Never fix a symptom found in one area without checking the sibling callers.
Most of the bugs in round 0 were one root cause showing up in four places.

## Where each area runs

| Where | Areas |
|-------|-------|
| Local `hamr dev` | C, D, E, G, J, K's grep and calc rules |
| The manager alone | A0 |
| A disposable manager and worker | A, B, F, H, I, K's eyes rules and journeys |

The remote rig is deliberately not recorded in the repository. Supply its
manager through `STACKR_DEPLOY_HOST`; pass worker addresses to `--nodes`.

```bash
STACKR_DEPLOY_HOST=manager.example.test scripts/dev/wipe-test.sh \
    --nodes worker.example.test
STACKR_DEPLOY_HOST=manager.example.test scripts/dev/deploy-test.sh
```

Rejoin workers from the panel after deployment. Keep certificates unless the
test is specifically about issuance; duplicate-certificate limits are external
state even when every machine is disposable. `scripts/dev/vm.sh` is a separate
single-node local fixture.

## Areas

### A0. Single machine, no workers

Most installs. Run the whole of B through K on a fresh single-node install
first and confirm nothing that worked before multi-node got worse. Any
"node" UI that appears with one node is a finding.

### A. Node agent and multi-node

Highest risk: newest code, and every check here is a place where a wrong
answer is silent rather than an error.

1. Add node → join script installs docker, joins the swarm, agent reaches 2/2
2. Volume move between nodes — verify by checksum on the target, and verify
   the source volume is still whole
3. Worker tile volume: browse, upload, download, delete
4. Worker tile: logs, terminal, live stats, start, stop, remove
5. Backup and restore of a worker tile
6. DB tile on a worker: exec, dump, restore, scheduled jobs
7. Port forward to a worker tile
8. Agent rolls on a panel upgrade (digest pin actually changes the spec)
9. Placement (`internal/stackrd/infra/placement/`): a tile with a volume
   redeploys onto its home node and nowhere else, and a tile with a node
   group lands only on a node carrying that label. Swarm calls a task on
   the wrong node healthy while its volume is empty, so verify by reading
   the data, not the task state
10. Worker pulls from the manager's registry: deploy a tile whose image is
    only in the registry (`010_registry_always_on`) to a worker
11. Node host stats (`internal/stackrd/infra/metrics/host.go`): the servers
    page shows CPU, memory and disk for the worker, and the numbers move
12. Node lifecycle (`handlers/web/handler/server/nodes.go`): drain, activate,
    remove, and rejoin. For each, what happens to a pinned tile on that node
    and what the UI says about it
13. Worker goes offline (power off the VM): containers page, servers page,
    and the pinned tile's card all say so; nothing hangs; it comes back when
    the VM does
14. Agent security (`internal/stackrd/infra/agent/server.go`): a request
    to the agent port without the runtime key is refused on every route.
    Rotate the join key from the servers page and confirm the old key no
    longer joins

### B. Containers page

15. Node column correct, unreachable node named in a flash rather than
    dropped, panel and agent rows show a badge instead of stop / remove

### C. Stacks and config as code

16. Plan, apply, drift; serialize round-trip; varref
17. Promotion ladder and the releases page

### D. Onboarding and settings

18. Fresh install wizard, registry, domain and TLS, the servers page
    Fresh install, no org yet: the owning account can still POST under
    `/servers` (add node, drain, save a volume). The middleware used to
    answer "read-only access" here

### E. Canvas, graph, staging

19. Staged tile creates, apply, canvas render

### F. Install and deploy scripts

20. `install.sh` on a clean VM, `deploy-test.sh`, the join script

### G. Migrations and the upgrade path

Do not ship without this one. The migrations have only ever run on a fresh
database.

21. Upgrade a populated install, not a fresh one. `008_drop_compose_tiles`
    drops two **columns** on `tiles` and converts every compose tile to a git
    tile. It used to destroy the compose definition outright and its
    `.down.sql` could not bring it back; it now archives each definition to
    `compose_tiles_archive` first and the down migration restores from it.
    Test against a copy of a real database, both directions, and check the
    rows rather than only the schema — `TestMigrationsUpDownUp` runs on an
    empty database and so proves nothing about data.
22. `009_network_pool`, `010_registry_always_on`, `011_multi_node` backfill
    correctly on an existing org rather than only on an empty one

### H. Overlay networking

New packages, no coverage at all.

23. Overlays carry no `encrypted` option: `docker network inspect stkr` and any
    `stkr-env-NN` show no such key. Encryption was removed on 2026-09-11
    (docs/plans/31-node-agent-open-questions.md, overlay encryption), so a
    network that has one came from an install predating the removal and never
    got recreated
24. `netpool`: allocate, exhaust, release; no subnet collides with the host
    LAN the VMs sit on

### I. Traefik across nodes

25. Route a worker tile through the proxy, issue its cert, and confirm
    `proxy/probe.go` reports healthy from the manager

### J. CLI and API contract

26. `docs/openapi.json` matches the handlers; `internal/cli/cmd/tile.go`
    against a tile running on a worker

### K. Design and UX

Rubric and journeys: the second half of this file. Runs last, because flow problems change layouts and polishing before that is
wasted work.

27. The grep and calc rules — colour tokens, contrast, copy. No browser needed,
    so run these before anything else in the area
28. Per-screen states: empty, loading, error, too much data, both themes
29. The five journeys, recording every step where you have to guess what to
    do next

## Round doc template

```
# QA round N — <what changed>

Swept <date>, <where>, commit <hash>. Loop: ../qa-loop-runbook.md.

## Matrix
| # | Check | Result |

## Bugs
### BUG-n — <one line> (Pn) — FIXED / OPEN
Repro:
Root cause:      (name the file and the wider flow, not just the function)
Fix:
Sibling callers checked:
```

---

# Part two: design principles and the UX rubric

The rubric behind area K.

Written to be checkable by someone who is not a designer. Every rule below is
one of three kinds, and each rule says which:

- **[grep]** — a command finds the violations. No judgement, no eyes.
- **[calc]** — computed from the tokens. A number either passes or it does not.
- **[eyes]** — needs a person or a browser. Kept short and specific on purpose.

If a rule cannot be written as one of those three, it is taste, and taste does
not belong in a QA sweep.

## What is already covered elsewhere

Do not re-litigate these, `make templint` fails the build on them:

- `<img>` without `alt`, `<a>` without `href`
- single-line control flow in a `.templ`, which templ silently drops

## The design system

`frontend/css/input.css` defines the palette as RGB triplets on `:root`, with
`html.light` overriding the same names. `frontend/tailwind.config.js` maps
each to an `rw-*` utility. One class on `<html>` swaps the whole look, written
by `layout.templ` from the user's saved theme.

This is the thing to protect. Every rule in the next section exists because a
value written outside that system breaks one of the two themes silently.

## Rules

### 1. Color comes from a token [grep]

Any color literal in a template is a bug. It is correct in whichever theme it
was written against and wrong in the other, and nothing reports it.

```
grep -rE '#[0-9a-fA-F]{6}\b' --include='*.templ' internal/
grep -rE '\b(text|bg|border)-(gray|slate|zinc|red|green|blue|amber|violet|sky|rose|teal)-[0-9]{2,3}' --include='*.templ' internal/
```

Baseline 2026-09-10: 5 hex literals, 0 raw palette classes. Each of the five
needs a reason in a comment or a token.

### 2. Text meets contrast on the surface behind it [calc]

WCAG AA is 4.5:1 for body text, 3:1 for text at 18pt or 14pt bold. Ratios
computed from the tokens, both themes, every text colour against every
surface:

**Dark**

| on | bg | surface | raised | inset |
|----|----|---------|--------|-------|
| text | 16.84 | 15.78 | 14.76 | 14.12 |
| muted | 7.02 | 6.57 | 6.15 | 5.88 |
| **faint** | **3.65** | **3.42** | **3.20** | **3.06** |
| accent | 5.26 | 4.92 | 4.61 | **4.41** |
| accentHi | 6.66 | 6.24 | 5.84 | 5.58 |
| success | 7.50 | 7.03 | 6.58 | 6.29 |
| danger | 6.77 | 6.34 | 5.93 | 5.67 |
| warn | 10.07 | 9.44 | 8.83 | 8.44 |

**Light**

| on | bg | surface | raised | inset |
|----|----|---------|--------|-------|
| text | 15.90 | 17.25 | 15.22 | 14.56 |
| muted | 6.06 | 6.58 | 5.80 | 5.55 |
| **faint** | 4.70 | 5.09 | **4.50** | **4.30** |
| accent | 5.04 | 5.46 | 4.82 | 4.61 |
| accentHi | 6.38 | 6.92 | 6.11 | 5.84 |
| **success** | **3.90** | **4.23** | **3.74** | **3.57** |
| **danger** | **4.36** | 4.73 | **4.18** | **4.00** |
| **warn** | **4.16** | 4.51 | **3.98** | **3.81** |

Findings that fall out of this without opening a browser:

- `rw-faint` fails AA on every dark surface. It is legal for a label at 14pt
  bold or larger and illegal for anything smaller. Either it is only ever used
  for large labels, which needs proving, or it needs lifting.
- In light, `rw-success`, `rw-danger` and `rw-warn` all fail AA on `bg`,
  `raised` and `inset`. Status text is exactly the text a user must not
  misread, and red on a light card is the worst of the three.
- `rw-accent` on `rw-inset` in dark is 4.41, marginal. Fine for a border, not
  for body text.

Recalculate these values whenever a token moves. Every body-text pair must be
at least 4.5:1; large or bold text must be at least 3:1.

Every pair above passes as of round 4, and the table it prints has a second
half. A chip paints a 15% wash of its own colour behind its own text, which is
not one of the four flat surfaces — so the first table said the tokens passed
while every status chip in the product failed. The chip table is the one to
read for anything that is not plain text on a plain panel.

The token table still cannot see one thing: text on an accent **fill**, such
as white on a primary button. That wants the fill darker while the same token
used as text wants it lighter, which is why `accentDeep` and `accentPress`
exist separately from `accent`.

### 3. Both themes, every screen [eyes]

`15-theming` is marked implemented. This proves it. Toggle the theme on each
page and look for text that vanishes, borders that disappear into their
surface, and any element that keeps its dark background in light.

### 4. An action that can lock the operator out does not render [grep + eyes]

Not "warns". Does not render.

This is a rule because it already happened: the containers page offered a
**stop** button on the panel's own container, because the self check compared
the container id to the hostname and the panel runs under `--hostname
stkr-panel`. One click ends the session that would have to undo it.

Sweep: every destructive control (stop, remove, drain, delete, wipe, move)
against the objects the panel itself depends on — its own container, the node
agent, the last manager, the registry, the network a running tile is on.

### 5. A destructive confirm names the object and what does not come back [eyes]

"Are you sure?" is not a confirm. The dialog states the name of the thing, and
states plainly what is unrecoverable. Where the operation destroys data with
no undo, the confirm requires typing the name.

Anchor: `008_drop_compose_tiles` drops a table whose `.down.sql` cannot restore
the rows. Anything with that shape needs this treatment in the UI too.

### 6. Every long operation says what state the system is in now [eyes]

Not "failed". A user reading a failure needs to know whether to retry, clean
up, or do nothing.

The exemplar already in the product is the volume move failure: "back on the
node it started on and its data was not touched". Every deploy, move, backup,
restore, promote and node join gets a failure line of that shape.

### 7. Internal errors never reach a user-facing stream [grep + eyes]

Raw daemon and API text in a log viewer or a flash reads as "the product is
broken" even when the product is fine.

Anchor: the combined environment log stream emitted `E no such task or
service` into the viewer for every volume tile, because volume tiles have no
service to stream.

Sweep: open every log and event stream with a tile of each kind present,
including the kinds that have nothing to stream.

### 8. Four states per screen [eyes]

The state matrix is where the 63 templ files actually get swept. Per screen:

| State | What to check |
|-------|---------------|
| Empty | says what this is and the one action that fills it, not a blank card |
| Loading | something occupies the space before data lands, no layout jump |
| Error | what failed, and what to do about it |
| Too much data | 200 rows, a 400 character name, a 50 MB log — wraps or scrolls in its own container, the page never scrolls sideways |

Too much data is the one that gets skipped and the one that breaks in
production. A node name from a real hostname is longer than any name typed in
a test.

### 9. Copy is written from the user's side [grep + eyes]

- Name things by what a person recognises, not by how the system is built.
  A user manages a **server**, not a swarm node object.
- A control says exactly what happens. "Publish" then "Published".
- One short sentence per hint. No mechanics in the hint.
- An error says what went wrong and how to fix it. No apologies.
- No em dashes in user-facing strings [grep]:
  `grep -rn '—' --include='*.templ' internal/` — 46 files carry one today,
  needs a pass to separate copy from Go comments.

### 10. Keyboard and focus [eyes]

Tab through each page. Focus is always visible, order follows the layout, and
nothing is reachable only by mouse. Modals trap focus and close on Escape.

## Journeys

Screen-by-screen rules miss dead ends between screens. Walk these end to end,
recording every step where you have to guess what to do next. A guess is a
finding even when the step works.

1. **First run to first app online.** Fresh install, no org. Stop at the first
   moment you would have to read documentation.
2. **My app is down, find out why.** Break a tile on purpose. Time how many
   clicks from the canvas to the log line that explains it.
3. **Add a second server.** From the servers page to a tile running on the
   worker.
4. **Move a volume between nodes.** Including watching it fail: pull the
   network mid-move and read what the UI says happened.
5. **Undo a destructive action.** Remove something recoverable and get it
   back. If it cannot come back, the finding is whether the UI said so first.

## Method

Headed playwright, never headless. Screenshots to `.playwright-mcp` only,
cleaned up after the round. Findings go in the round doc, not here.

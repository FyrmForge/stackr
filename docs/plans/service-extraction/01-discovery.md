# Service extraction discovery

Status: agreed, not started.

Instructions for the agents doing the sweep. Read-only. No code, no fixes,
no "should". Output is `02-findings.md`. The refactor is `03-refactor.md`,
written only after the findings are agreed.

## Why

Business logic lives in handlers. Validation, gates, side effects and
orchestration are written once for the web panel and again for the API, and
the copies have drifted. `config/stackconf` `apply.go` writing rows directly
is the same problem.

Target: MVC-style separation. A handler parses the request, calls a service,
renders. A service owns the rule, defined once. Consumers on top, none
reaching past it:

    handlers/web · handlers/api/v1 · internal/cli (via the API) · config/stackconf

Services are per business concept, not per surface, and they compose. A tile
service creating a tile calls the proxy service to ensure the route and TLS;
the settings page calls the same proxy service for the same job. A service is
the only layer that touches store and `infra/` together. Nothing above it
orchestrates.

Three jobs, same pass:

1. **Inventory** every piece of business logic sitting in a handler. That is
   what moves into a service.
2. **Hunt drift** between the copies. Those are the rows already broken.
3. **Map the cross-cutting reach.** Every handler that calls an `infra/` or
   `config/` package directly is a consumer that should be going through a
   service. Count them per package. That is the composition graph.

This sweep only finds what is there. It decides nothing about the refactor.

## Paths

Everything is under `internal/stackrd/`:

- web: `handlers/web/server.go` is the route list, bodies in `handlers/web/handler/<area>/`
- api: `handlers/api/v1/v1.go` registers every op via `op[...]`; bodies in the sibling files
- cli: `internal/cli/cmd/` (repo root), calls the API
- config: `config/stackconf/apply.go` and `config/orgconf`
- engine line: `infra/` is below it. Nothing above `infra/` may import docker or swarm types.

`docs/surface-parity.md` lists which surface has which feature. Use its rows
as the operation list. It does not say whether the implementations agree.
That is this sweep.

## What to record

One row per **business operation**, not per route. "Update tile settings" is
one row even though it is an API `PATCH`, a web `POST`, a CLI verb and a
config key.

| column | meaning |
|---|---|
| operation | the verb in plain words |
| web / api / cli / config | `file:line` of the owning func, or `--` |
| shared | one impl called by all, or separate copies |
| gate | `editGate`, `rejectManaged`, or none, per surface |
| logic in handler | per surface: what the func does that is not HTTP glue. Validation, gate, side effects, orchestration across store/proxy/cluster. This is what moves. |
| side effects | per surface: store · proxy write · scheduler reload · notifier · cluster/runtime · staging · workqueue |
| return shape | do the surfaces build different view structs from the same rows |
| class | A / B / C / D or blank |

**Logic in handler and side effects matter most.** Follow the call chain
through helpers; a handler that calls `syncProxy` writes the proxy. Two
surfaces that agree still get a row: agreement is not a reason to skip it.

Classes, exactly one per finding:

| class | definition |
|---|---|
| A — rule drift | same operation, different validation between surfaces |
| B — missing side effect | one surface skips an effect the other performs. A live bug. |
| C — identity mismatch | the surfaces address different sets with no join key. An engine package does not fix this; it needs a table or resolver first. Do not report C as A. |
| D — driver leak | docker or swarm import above `infra/` |

Rules:

- `file:line` on every claim. No ref, no finding.
- Say "not checked" instead of guessing.
- Record, do not fix. Not even a one-liner.

## Shards

Two lenses. Both run. Scope is the whole repo, no area left unswept.

**Lens 1, concept shards.** One agent per business concept, owning **every
surface** for its slice. Rows are operations:

tiles · volumes+storage · orgs+members+auth · domains · backups · databases ·
envs+variables+secrets · stacks+deploys+releases · registry+github+prhooks ·
settings+notifications

Sharding by surface cannot see A or B, so never shard that way.

**Lens 2, cross-cutting shards.** One agent per infra or config package that
handlers reach into directly. Rows are **call sites**, not operations: who
calls it, from which surface, with what rule around the call. This is how a
"traefik is written from six handlers with four rule sets" shows up.

proxy · plan+staging (`config/stackconf`, `config/staging`, `envops`) ·
deploy orchestration (`infra/deploy`, `runtime`, `managedtiles`, `envnet`,
`jobs`, `workqueue`) · notifier+mail+scheduler reload

Direct-import counts from `handlers/` and `config/stackconf` on 2026-09-18,
as a starting map (highest first): `runtime` 27, `managedtiles` 27,
`stackconf` 26, `envops` 15, `deploy` 14, `envnet` 13, `registry` 11,
`proxy` 11, `githubapp` 10, `cluster` 10, `jobs` 9, `backup` 9,
`workqueue` 6, `storagetiles` 6, `forward` 6. Anything a lens 2 shard finds
that lens 1 missed gets a row in the concept table too.

One file per shard, same columns, merged into `02-findings.md` at the end.

`00-calibration.md` is what an earlier pass already found. Read it first,
do not re-derive it, do extend it.

## Output

`02-findings.md`:

1. The operation table. Every operation, flagged or not.
2. **The service inventory.** Every row maps to exactly one candidate
   service. Per service: its operations, a sketched method signature from
   the "logic in handler" column, the call sites, and **which other services
   it calls**. Ranked by bugs found and call sites. This is the main
   deliverable.
3. Drift findings, one per flagged row, with the concrete divergence and
   line refs. Annotations on the inventory, not a separate report.

## Decisions already taken

- Operations are **services**, not controllers ("controller" is reserved
  for the k8s sense). Package name and layout are open for `03`:
  `internal/stackrd/service` already exists, an `engine` package may sit
  inside it or beside it.
- Engine takes a full tile struct, not a sparse patch. The API's merge stays
  at its edge.
- Scope: everything. Reads and writes, every surface.
- No installs exist. Breaking API and CLI shape is free.

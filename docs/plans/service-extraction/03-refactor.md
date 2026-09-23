# Refactor plan: service extraction

Status: proposed, not agreed, not started.

Working rules: discuss first, one point at a time, no code without a go, no
git writes, no edits to `*_templ.go` or `output.css`, terse UI copy.

Input is `02-findings.md`: 267 operations, 117 flagged, 17 confirmed
security findings. This file does not restate them. It cites row numbers
(`1.1`, `2.6`) and service names from that inventory.

## Why

Business logic lives in handlers, written once for the web panel and again
for the API, and the copies have drifted. That is not a tidiness complaint:
37 of the flagged rows are class B, a side effect one surface performs and
the other skips. Those are live bugs and they are not individually fixable,
because the next feature adds the next pair of copies.

The findings also say the refactor cannot be done as one move. 66k LOC,
about 40 candidate services, cycles in the composition graph. So this plan
commits to the decisions, specifies the first two extractions to buildable
depth, and ranks the rest as a backlog.

## Decisions

These are taken, with reasons. Push back on any of them before point 1
starts, because they are expensive to reverse once three services exist.

### D1. Flat core, leaves in their own packages

Not one package per service. The composition graph (`2.16`) has real cycles:
`TileService → SliceService` and `SliceService → ManagedInstanceService →
SliceService`. Go forbids import cycles across packages, so a package per
service would need an interface at every cycle edge — a dozen interfaces
with one implementation each, which is the thing we are trying to stop
writing.

So, hybrid:

- **Core**, flat, `internal/stackrd/service`, one file per concept
  (`service/tile.go`, `service/slice.go`, `service/environment.go`). Every
  service with an outgoing edge to another service lives here. Cycles cost
  nothing inside a package. `service/auth.go` and `service/admin.go` already
  live there and already say "business logic that outlives any one HTTP
  handler"; the five handler import sites do not change.
- **Leaves**, own packages under `service/`. The six services `2.16` lists
  as leaves have no outgoing service edges, so they cannot be in a cycle:
  `service/proxy`, `service/scheduler`, `service/notify`, `service/mail`,
  `service/audit`, `service/volumeown`. Core imports them; they import
  store and `infra/` only.

Points 1 and 2 are both leaves, so the first two extractions ship as small
self-contained packages before the core package exists at all.

Risk: a leaf stops being one, because some later point gives it an outgoing
edge. Cost is a one-file move into core, or one interface if the move is
awkward. Contained, and cheaper than pre-emptively flattening everything.

The core package will become large. That is the price, and it is cheaper
than the interfaces.

### D2. Both surfaces switch together, per concept

When a concept is extracted, web, API, CLI-via-API and `config/stackconf`
all move to the service in the same change. Migrating web now and the API
later leaves the drift in place for the gap, which is the entire problem.

A concept is not done until no handler for it computes a rule.

### D3. Behaviour is chosen, not inherited

Where two surfaces disagree, the service picks one rule and this plan, or
the point that extracts it, says which and why. The default is the correct
rule, not the web one and not the one with more callers.

No installs exist outside the test rig, so breaking API and CLI shape is
free. No compat shims, no dual paths, no deprecation window.

### D4. One error vocabulary, translated at the edge

Services return typed errors. Handlers map them. Decided once, per `2.5`
item 8:

- not yours → 404
- yours, wrong level → 403
- draft org or config-managed → 409
- validation → 400 with the field

Today web answers 404 where the API answers 403 for the same refusal
(`§4`, access). After this, both answer what the vocabulary says.

### D5. Handlers parse and render, nothing else

A handler binds the request, calls one service method, renders the result or
maps the error. No store calls, no `infra/` calls, no gate computation. The
graph builders and canvas handlers carry no rule and import nothing below
`handlers/` (`§4`, leftovers) — they stay where they are.

### D6. Services take full structs

From `01`: the engine takes a full tile struct, not a sparse patch. The
API's merge stays at its edge. This is what kills the eleven copies of the
tile field list (`02` header) — the service has one input shape.

### D7. `infra/` stays below the line

A service is the only layer that touches store and `infra/` together.
`infra/` packages do not import `handlers/`; four do today
(`infra/deploy/engine.go:533`, `infra/jobs/jobs.go:421`,
`infra/imagewatch/watcher.go:142`, `infra/cigate/cigate.go:117`, all for
`handlers/notify`). NotificationService moving to `service/notify` (`2.13`)
fixes that inversion as a side effect.

The one class D hit, `service/admin.go:16` importing
`docker/api/types/swarm`, is fixed when AdminService is touched (`2.11`).

### D8. Each extraction names the rows it closes

An extraction point is done when the flagged rows it claimed are no longer
flagged. "Extract ProxyService" has no done condition; "closes `1.1` Update
tile settings B, `1.1` Rename tile B, `2.6` R1-R7" does.

## Settle before touching

Short list. Each is a read or a one-shot runtime check, not a build. None
of them should be folded into an extraction point — a service that encodes
a guess is worse than a handler that does.

1. The three unverified security items (`§4` Unverified, `§5` Unverified):
   orphan deployment rows on web, and the profile-email-case lockout.
   One runtime check each. If the email one reproduces, it is a confirmed
   [SEC] and goes into the AuthService point.
2. The load-bearing "not checked" cells: whether service names embed the org
   slug in `MoveStack` (`1.5`), the node-less volume inspect for a moved
   volume (`1.2`), and whether `Applier.Promote` running inline on the
   request context has ever failed in the way `stackconf/job.go:52-57`
   exists to avoid (`2.4`).
   Two items left this list during the point write-up: `inv.MailFailed` is
   `db:"-"` by design (point 14), and the deployment-claim question is
   answered by the work item's claim (point 7).
3. Which plaintext-at-rest fields get encrypted and which are fine
   (`§4`, small config packages): `registries.password`, `Tile.DBPassword`,
   `BasicAuthPassword`, `WebhookToken`, `Settings.ProtectPassword`.
   `secrets.Encrypt` is applied only in `store/repo/sqlite`, so this is a
   store decision, not a service one, and it should be decided before
   RegistryAdminService moves.

## Points 1 and 2, specified

Ordered. Each point is a separate go.

### 1. SchedulerService

First because it is a true leaf (`2.16`: store and infra only), it is two
methods, and it closes two confirmed class B rows. It proves D1, D2 and D8
at a size where getting the pattern wrong costs an afternoon.

```go
package scheduler // internal/stackrd/service/scheduler

type Service struct{ jobs *jobs.Service; backups *backup.Service }

func (s *Service) ReloadCron(ctx context.Context) error
func (s *Service) ReloadBackups(ctx context.Context) error
```

Naming, per D1: a leaf package's type is `Service`, not `<Name>Service`, so
callers read `scheduler.Service` and `proxy.Service`. Core services keep the
`<Name>Service` spelling, because in one flat package `service.Service`
means nothing. The inventory names in `02` (`SchedulerService`,
`ProxyService`) stay as the concept names; they are not literal type names
for leaves.

One collision to watch: `service/proxy` and `infra/proxy` both want the
identifier `proxy`. Files importing both alias the lower one as
`infraproxy`. After point 2 that should be one file — the wiring in
`cmd/stackrd/main.go` — because nothing above the service holds a
`*proxy.Proxy`.

One nil guard inside, one error policy inside. Today the nil guard is
copied into `h.reload` and `a.reloadBackupSchedules`, and the error policy
is three-way: `api/apps.go:163` returns 500, fourteen sites discard,
`config/stackconf/apply.go:988,1100` warn. The service logs and returns nil
for a reload failure — a stale cron entry is not worth failing a delete that
already committed — except on the boot path, which returns the error.

Call sites replaced: `jobs.LoadSchedules` 18 (api 7, web 9, config 1,
boot 1), `backup.LoadSchedules` 10 (api 3, web 5, config 1, boot 1). Both
counts are through the existing `h.reload` / `a.reloadBackupSchedules`
wrappers, which the service replaces.

Rows closed:

- `2.12` [B] stack delete reloads no cron on either surface.
  Verified: `handlers/web/handler/project/handler.go:460` and
  `handlers/api/v1/stacks.go:64` both run `TeardownStack` then `DeleteStack`
  and return. Stale entries tick and fail at `infra/jobs/jobs.go:340-342`.
- `2.12` [B] web direct-path tile delete reloads no cron.
  Verified: `handlers/web/handler/app/handler.go:1630-1642` tears down,
  removes the route, detaches provisions, deletes the row, returns. The API
  twin does reload (`api/apps.go:475`).
- `2.10` [B] destination delete cascades every schedule
  (`001_initial.up.sql:52`) and neither surface reloads backups.
  Verified: `api/v1/backups.go:101-104` deletes and returns;
  `web/handler/settings/handler.go:351-357` likewise. Orphaned entries fire
  `Start` on missing rows every tick
  (`infra/backup/backup.go:100-102`, `infra/backup/work.go:109-112`).
- `2.10` [B] the six tile-delete paths that cascade `backups` rows reload
  nothing.

Recorded in `2.12` and worth doing here: reloading inside
`envops.Teardown` / `CloneTiles` / `TeardownStack` and `Applier.CopyTile`
would remove nine of the eighteen cron sites and close the first two rows
without touching any handler. Decide which way when the point starts —
callers calling the service explicitly is more obvious, the ops reloading
themselves is a smaller diff.

Files: new package `service/scheduler`; `cmd/stackrd/main.go` wiring; the 28 call
sites; delete `h.reload` (`web/handler/backups/handler.go:326-331`) and
`a.reloadBackupSchedules` (`api/v1/backups.go:402-407`).

### 2. ProxyService

Second because it is the other leaf with real damage behind it: 53 call
sites, 44 of them writes, and seven different rule sets around one write
surface (`2.6` R1-R7). "Add a domain" crosses four of them.

Methods as sketched in `2.6`. The rule that makes it work: nothing above the
service holds a `*proxy.Proxy`. `envops.EnsureAutoDomain` takes the service,
not the proxy.

Rows closed:

- `1.1` Update tile settings [B], the calibration finding: web calls
  `syncProxy` after save (`web/handler/app/handler.go:1597`), the API patch
  never does. Verified: `api/v1/apps.go:452-460` writes the row, reloads
  cron for a cron tile, returns. `basic_auth_*`, `sec_headers` and
  `traefik_override` set over the API do not take effect until a redeploy,
  and `basic_auth_password` is stored unhashed and unused because hashing
  happens at route-write time (`config/stackconf/stackconf.go:397`).
- `1.1` Rename tile [B], confirmed during the review of `02`:
  `config/stackconf/moved.go:306-311` removes the route and never rewrites
  it. `restartTile` (`apply.go:594-615`) redeploys a running service or a
  managed instance and returns for everything else; no branch writes the
  proxy, and the deploy engine never calls `WriteApp` (`1.11`). A `moved:`
  rename serves nothing until a domain edit or a `Resync`.
  `TileService.Rename` calls `ProxyService.SyncTile`; until TileService
  exists, `moved.go` calls it directly.
- `2.1` [B] web managed-db delete never calls `RemoveApp`
  (`web/handler/db/handler.go:464-467`); the route survives until `Resync`
  prunes it (`infra/proxy/proxy.go:717`).
- `2.8` [B] API `patchRegistry` writes the route without
  `registry.EnsureManaged` (`api/v1/registry.go:298-324`) where web runs
  both (`web/handler/settings/handler.go:616-624`).
- `2.6` [B] boot `EnsureManaged` never renders `registry.yml` and `Resync`
  skips it (`infra/proxy/proxy.go:577-624,715`): a wiped data dir loses the
  registry route until the domain is re-saved. `Resync` renders it, or boot
  calls `EnsureRegistryRoute`. D3 pick: `Resync` renders it, because "the
  route exists" should not depend on which code path last ran.
- `2.6` [A] trusted-proxy CIDRs validated two ways — the seed skips a bad
  line with a warning (`cmd/stackrd/seed.go:54-57`), the handler refuses the
  whole save (`web/handler/settings/proxy.go:74-77`), and the installer is a
  third caller of the same parser (`internal/installer/answers.go:565`).
  D3 pick: refuse the save. A silently dropped CIDR is a trusted proxy that
  is not trusted, which shows up as every client IP being the proxy's.

Behaviour picks this point must also make, all class A in `2.6`:

- The seven managed-gate rule sets collapse to the two real ones from
  `00-calibration.md`: `editGate` for a config-owned field edit (may stage),
  `rejectManaged` for a structural write (fails closed). They are not one
  function; collapsing them would let structural writes stage. The third and
  fourth spellings (`managedErr`, inline `ConfigManaged()`) go.
- `RemoveApp` errors are ignored at every site today. Pick: log, do not
  fail. A route file that will not delete must not block a tile delete that
  has already torn down the service.
- `EnsureTraefik` rewrites the static config and restarts the traefik
  container. Today: web ×3 goroutine + logged
  (`web/handler/settings/handler.go:380`, `proxy.go:99,123`),
  `api/v1/proxycfg.go:93` goroutine + discarded,
  `api/v1/domainresources.go:224` **synchronous + discarded**, boot
  `cmd/stackrd/main.go:362` synchronous + returned.
  Pick: async and logged at every call site except boot, which stays
  synchronous and returns — nothing is serving yet, and a traefik that will
  not start should fail the boot. `domainresources.go:224` is the one that
  changes: it restarts a container inline on the request context, so a
  client disconnect can cancel a restart halfway.
  Pick: the service serializes it. Nothing today stops two saves racing two
  goroutines through the same static-file rewrite and container restart. One
  mutex on the service; concurrent calls coalesce to a single run after the
  one in flight.

Files: new package `service/proxy`; 53 call sites across
`handlers/web`, `handlers/api/v1`, `config/stackconf`, `config/envops`,
`cmd/stackrd`.

## Points 3 to 16

Every point below uses one template:

- **Depends on** — which earlier point must land first.
- **Methods** — promoted from the `02` inventory, not re-derived.
- **Rows closed** — the acceptance criterion (D8). `[verified]` means the
  code was read during the review of `02`; `[from 02]` means the finding is
  the sweep's, trusted but not re-read.
- **Picks** — D3 decisions in three buckets: **decided** (determinable from
  what `02` already states), **resolved by reading** (the read was done, the
  answer and its evidence are in the point), and, where a standing pick was
  wrong, a named exception. Every pick in this document is now made. Three
  are marked as reversible product calls where a reasonable person could
  choose the other way — they are called out in the tally.
- **Files** — call sites, from the `02` lens 2 counts.

Per D1, everything below lands in the flat core except four leaves that get
their own package: NotificationService (`service/notify`, point 12),
MailService (`service/mail`, point 14), AuditService (`service/audit`,
inside point 16) and `VolumeOwnership` (`service/volumeown`, point 9 — a
leaf even though the VolumeService it serves is core).

### Three standing picks

These resolve most class A rows without reading anything. Stated once here,
not repeated per point. A point only lists a pick that these do not settle.

- **SP1. Validate, do not coerce.** Where one surface 400s on a bad value
  and the other silently coerces it to a default, the service 400s. A
  coerced value is a write the user did not ask for, and it is the pattern
  behind a large share of the A rows (bad port → 0, bad role → `member`,
  bad policy → `off`, unknown scope → env, bad range → 1h).
- **SP2. Perform the effect.** Where one surface performs a side effect and
  the other skips it, the service performs it. That is the definition of
  class B: the skipping surface is the bug.
- **SP3. Keep the guard.** Where one surface has a check and the other does
  not, the service keeps the check. Applies to managed gates, ownership
  checks, kind checks, last-owner and last-admin guards, held-slices and
  attached-volume refusals.

Where a standing pick is wrong for a specific row, the point says so and
gives the reason. There are two such exceptions below (points 6 and 9).

### 3. TileService + TileLifecycleService + TileTelemetry

Depends on: points 1 and 2. Everything routes through this one, so it is
first of the core services despite being the largest.

Methods: `2.3`, as written, plus `Rename` added during the review.

Rows closed: `1.1` create, update settings, scale, rename, delete, stop,
restart, toggle cron, run now, metrics — 9 flagged rows (7 A, 5 B,
overlapping). Named:

- [B] config apply redeploys on any non-proxy field (`apply:1487-1495`);
  web `SaveSettings` and API `patchApp` only write the row, so port, limits,
  healthcheck, command and replicas edits take effect on the next manual
  deploy [from 02].
- [B] delete tile: API skips provision detach and the volume guard, web
  skips `jobs.LoadSchedules`, all six sites skip the backup reload [from 02,
  the reload half verified in point 1].
- [B] stop/restart/toggle cron notify on web, never on the API [from 02].
- [A] the eleven tile field-list copies collapse to one input shape (D6).
  `stackconf.TileConfOf` already exists at `config/stackconf/serialize.go:72`
  and `settingsPatch` (`web/handler/app/handler.go:1366`) does not use it
  [verified] — that is the seam.
- [A] kind checks one-sided: web `Deploy` refuses cron, API does not; API
  `toggleCron`/`runApp` require cron/function, web accepts any tile and
  `RunNow` nil-derefs `h.jobs` [from 02].
- [A] `tiles.status`: 14 writers, one observer. `TileLifecycleService`
  becomes the single owner of `UpdateTileStatus`, with `Observe` for the
  reconciler (`2.15`) [from 02].

Picks — decided:

- SP1 covers limits clamping, timeout range, healthcheck, metrics range.
- SP2 covers the proxy rewrite, the redeploy, the notifier, the two reloads.
- SP3 covers the kind checks, the volume guard and the provision detach.
- `RunNow` nil-deref: guard and 503, matching the API. Not a pick, a bug.
- Trigger strings unify: one vocabulary, source as a field, not baked into
  the string (`"manual web"` / `"manual api"` / `"manual"` / `"api"` today).

Picks — resolved by reading:

- **D6 does not mean the store writes everything.** Read
  `store/repo/sqlite/tiles.go:160-217`: `UpdateTile` excludes `status`,
  `slug`, `shared_net` and `home_node` deliberately, and the comment names
  the bug behind each exclusion — a settings save carrying a stale rendered
  value used to revert the tile. So: the *service* takes a full struct, the
  *store* keeps its exclusions, and identity moves only through
  `RenameTile`, `UpdateTileStatus`, `SetTileHomeNode`, `SetTileSharedNet`.
  D6 is about the service's input shape, not the UPDATE statement. Worth
  writing into the service doc comment, because a full-struct write that
  "helpfully" included these would silently revert four columns.
- **The limits/build pair: fix the merge, not the pair.** Read
  `internal/cli/cmd/tile.go:247-263` — the CLI already reads both halves
  together and its comment says why. The bug is that an *unset* flag reads
  as `""`, not as the current value, so `--cpu` alone genuinely sends
  `memory_mb: 0` and `api/v1/apps.go:248-253` assigns both halves whole.
  Pick: nested objects merge per field on the API side (a nil field means
  unchanged), and the CLI sends only the halves that changed. That kills the
  class for `limits` and `build` together rather than special-casing either.

Picks — decided (were dev calls):

- **Scale is exposed to API and CLI.** Config already declares `replicas`
  and `node_group`, and `appPatch` already parses the keys — `patchApp` just
  drops them. An API that accepts a key and silently ignores it is worse
  than one that refuses it, and worse again than one that works.
- **Stop on a cron sets `paused`, not `stopped`.** `paused` already exists
  and is what `ToggleCron` writes. The reconciler skips run-to-completion
  kinds (`infra/metrics/reconcile.go:26`), so a cron parked `stopped` is
  parked for ever and still ticks (`infra/jobs/jobs.go:311`). `paused` is
  the state the scheduler already honours.

Files: `service/tile.go`, `service/tilelifecycle.go`; `deploy.Engine.Enqueue`
14 handler sites; `jobs.LoadSchedules` 18 (already routed via point 1); the
eleven field-list copies.

### 4. DomainService + DomainResourceService

Depends on: point 2 (every method ends in a proxy write), point 3 for the
tile-ownership walk.

Methods: `2.7`, as written.

Rows closed: `1.6` — 7 flagged rows (5 A, 2 B). Named:

- [SEC, A] web `ToggleDomainHTTPS` (`web/handler/app/handler.go:1810`) takes
  `:domainID` with no `d.TileID == a.ID`, and `DeleteDomain` checks
  ownership on the staged branch (`:1892`) and not the live branch (`:1908`).
  Cross-tenant flip or delete by id [verified].
- [A] attach domain, three rule sets: the API skips squat, wildcard-DNS,
  `checkMiddlewares` and the kind/`SpeaksHTTP` check, cannot set
  rule/priority/middlewares, and leaves redirect port 0 — rendered as
  `http://alias:0` (`infra/proxy/proxy.go:447`) [from 02].
- [A] cert and HTTPS gates disagree; `force_https` is API-only [from 02].
- [B] stack-file `acme_email` applies on create only; the org file diffs and
  applies [from 02].
- [B] API patch skips `rejectManagedOwner` and discards the `EnsureTraefik`
  error — the latter already fixed by point 2 [verified].

Picks — decided:

- SP3 gives the ownership check, the squat check, wildcard-DNS, middleware
  refs and `SpeaksHTTP`. The API gains all of them.
- Redirect port 0: default to 80, as web does. `http://alias:0` is not a
  URL anyone meant.

Picks — decided (was a dev call):

- **HTTPS defaults:** `nil` means both on, explicit means explicit, and the
  CLI's `--no-https` sets both off. A bare `POST /domains` should produce a
  working HTTPS domain, because that is what anyone adding a domain wants;
  and `--no-https` meaning "https off, force-redirect to https on" is
  incoherent, so that one is simply wrong today.

Files: `service/domain.go`, `service/domainresource.go`; web app ×5, web db
×2, api ×5, `syncDomains`, `EnsureAutoDomain`; resources web ×7, api ×4,
`applyDomainRes`, `orgconf.applyDomains`, `cmd/stackrd/seed.go`.

### 5. ManagedInstanceService + SliceService

Depends on: points 2, 3.

Methods: `2.1`, as written.

Rows closed: `1.3` — 15 flagged rows (9 A, 6 B), the most of any group.
Named:

- [B] delete instance is four teardowns and none is complete: web releases
  netpool but never `RemoveApp`; API removes the route but never releases
  netpool and with `force=true` leaves provision rows; config has no
  held-slices gate and no netpool; orgconf is netpool only [from 02].
- [B] API `deleteApp` never detaches consumer provisions, leaving rows
  `active` with a dangling `ConsumerTileID` [from 02].
- [B] s3 wiring: API injects the whole `AutoInjectVars` set, web injects
  only `S3_ENDPOINT` [from 02].
- [B] set slice public: web and API flip every row sharing `DBName`, config
  flips one; the API sets `p.Public` in memory only [from 02].
- [A] gate drift `rFU` vs `rejectManaged`, two gates inside one handler
  file; unknown scope coerced on web, 400 on API [from 02].
- [A] web `Start` heals provisions, API `restartApp` on the same id does not
  [from 02].

Picks — decided:

- SP1: unknown scope 400s, bad port 400s, bad policy 400s.
- SP2: one `Delete` does all four effects — held-slices gate, `TearDown`,
  `Proxy.DropTile`, `netpool.ReleaseDB`, drop-or-orphan slices, `DeleteTile`.
  The union, because each surface's omission is a leak.
- SP2: `SetPublic` persists. The API's in-memory flip is a no-op bug.
- SP3: the held-slices refusal survives, and it takes the caller's flag.
  The CLI sending `force=true` unconditionally
  (`internal/cli/cmd/infra.go:1559`) is what makes the refusal unreachable
  today; the CLI stops doing that.
- s3 wiring: inject the full set, as the API does. Web's single var is the
  narrower and less useful of the two.

Picks — resolved by reading:

- **`needsRedeploy` takes config's field list.** Read all four: the API
  (`api/v1/databases.go:119-158`) triggers on external_port, cpu, mem,
  image, shm; config's `needsDBRedeploy` (`apply:1527-1539`) adds `env`,
  `node_group` and `replicas`, and its comment records the bug that put the
  last two in — a group pin on a managed instance wrote the row, touched
  nothing, and the plan read "applied" with the database still scaled to
  zero on the node it was meant to leave. Config's list is the union and the
  one with evidence behind it.
- **`Cut` gets config's rule plus `ServesEnv`.** Confirmed: `02`'s hedge was
  right — `createInstanceProvision` (`api/v1/resolve.go:81-119`) calls
  `managedtiles.ServesEnv` and nothing else. No `Eligible`, no readiness
  wait, no `on_remove`. Config's `createSlice` has `Eligible`,
  `WaitReady 90s` and adopt-or-create but not `ServesEnv`. The service takes
  all four.

Files: `service/managedinstance.go`, `service/slice.go`; `managedtiles` 38
sites (web 17, api 12, config 9); three per-request `NewService`
constructions (`api/v1/envs.go:195`, `web/handler/project/handler.go:2641`,
`web/handler/app/handler.go:1010`).

### 6. EnvironmentService + VariableService

Depends on: points 2, 3, 5.

Methods: `2.2`, as written, plus `ShareLinkService` and `EnvCompareService`.

Rows closed: `1.4` — 16 flagged rows (10 A, 6 B). Named:

- [B] variables over the API never replan and never redeploy
  (`api/v1/variables.go:116-197`) where web does both. `stackr vars set` on
  a running tile or a managed stack changes nothing visible [from 02]. The
  single most user-visible bug in the findings.
- [B] `resetEnv` over the API does not replan; web does [from 02].
- [B] drop-link submit writes vars with no `ClearWaiting` and no replan
  [from 02].
- [B] config apply mints generated secrets with no `audit.Record` [from 02].
- [B] env delete leaves `staged_changes` and env `node_positions` on every
  surface [from 02].
- [A] env create has four rule sets plus a fifth creator in the PR hook with
  no checks at all [from 02].
- [A] variable name rule: web regex, API any non-empty [from 02].

Picks — decided:

- SP1: the name regex applies everywhere. API "any non-empty" is the bug.
- SP2: replan, redeploy, `ClearWaiting*` and audit happen in the service, so
  every caller gets them, including the boot backfill and the drop link.
- SP3: env create gets one rule set — reserved names including `HomeSlug`,
  non-empty, unique per stack. The PR hook calls it like everyone else.
- Env delete clears `staged_changes` and `node_positions` itself.

Picks — exception to SP1:

- **Generate-secret overwrite.** Web overwrites a live value, API and config
  skip if set. SP1 does not apply; neither is a coercion. Pick: skip if set,
  the API behaviour. Overwriting a live secret from a UI button is a
  silent credential rotation that breaks running tiles, and the web panel
  can offer an explicit "rotate" if that is wanted.

Picks — decided (was a dev call):

- **Config only writes what it declares.** `apply:958-962` overwrites
  `ApplyPolicy` and `Color` on every apply even when the file is silent, so
  a panel or API write silently reverts on the next plan. Undeclared must
  mean unmanaged — that is what `ui_edits` and the whole gate vocabulary
  already assume everywhere else. The alternative (file-owned, panel
  refuses) is consistent too, but it takes a working panel control away to
  fix a config bug.

Files: `service/environment.go`, `service/variable.go`, `service/sharelink.go`,
`service/envcompare.go`; `envops` 15 sites, `envnet` 11,
`deploy.ClearWaiting*` 9; five `envops.Ops{}` literals become one
constructor.

### 7. DeployService + ReleaseService + PlanService + StagingService + ApplyEngine

Depends on: points 2, 3, 5, 6. The largest single lift — it is what turns
`config/stackconf/apply.go` from a second implementation into a caller.

Methods: `2.4`, as written.

Rows closed: `1.5` — 11 flagged rows (9 A, 3 B). Named:

- [SEC, A] cancel deployment runs on a read gate: `loadDeployment` calls
  `requireTile(c, d.TileID, false)` (`api/v1/apps.go:570-579`), so a
  read-only member with a deploy-scoped key can cancel [verified].
- [A] promote with a plan: both surfaces hand-build `ApplyJob` under the
  stack id, bypassing `EnqueueApply`'s plan-id dedupe; neither checks
  pending; plain promote runs inline on the request context [from 02].
- [A] the deployment live-state set is copied four times, and `waiting_ci`
  ends the web poll and SSE [from 02].
- [A] stack create pre-checks neither empty nor duplicate slug on either
  surface; raw UNIQUE 500 [from 02].
- [B] `moved:` rename writes Name and Slug only, so the display name becomes
  the slug [from 02]; the proxy half is closed by point 2 [verified].
- [B] bind config drops staged rows on web, not in orgconf [from 02].
- [B] stack delete reloads no cron — closed by point 1 [verified].

Picks — decided:

- SP3: cancel requires write. The read gate is the bug.
- SP2: one `EnqueueApply` with the plan-id dedupe, for both surfaces and
  both promote paths. No hand-built `ApplyJob`.
- SP2: promote never runs inline on a request context. `stackconf/job.go:52-57`
  exists precisely because that fails.
- One `IsLive`/terminal predicate, exported, replacing four copies. The CLI
  stops hard-coding it (`internal/cli/cmd/deploy.go:919`).
- Stack create pre-checks the slug and returns 409, not a 500.

Picks — exception to SP3:

- **`waiting_ci` in the live set.** SP3 would say keep both behaviours'
  guards, but here the surfaces are not guarding, they are disagreeing about
  a vocabulary. Pick: `waiting_ci` is live. A parked deploy is still going
  to happen, so the poll and the SSE must not end on it. This changes web
  and CLI behaviour, deliberately.

Picks — resolved by reading, and one downgrade:

- **The two "did not finish" vocabularies are two meanings, not drift.**
  Read all three: `SweepStaleRuns`
  (`store/repo/sqlite/deployments.go:55-69`) writes `error` +
  `InterruptedMsg` for queued/running, which is what an interrupted deploy
  is; `engine.go:313` writes `cancelled`, which is what a user or a
  supersede does. Those are different events and both spellings are right.
  `waiting_ci` is swept by neither *on purpose* — `cigate.Run`
  (`infra/cigate/cigate.go:40`) lists exactly that status on boot and
  re-adopts it. `RecoverInterrupted` keeps both words and leaves
  `waiting_ci` alone. **Downgrade this from a finding in `02` to a
  documented invariant.**
- **Deployments do not need their own atomic claim.** A deployment's
  lifecycle is driven by a work item, and work items claim atomically by
  rows-affected (`sqlite/workitems.go:48-76`). Two workers cannot hold the
  same item, so the read-modify-write on the deployment row is already
  guarded one level up. Not a bug; write the invariant into
  `DeployService` so nobody re-derives it. The `Cancel` path already handles
  the running case via context cancel (`engine.go:305-308`).

Picks — decided (were dev calls):

- **Promote force defaults to false, everywhere.** Web hard-codes `true`
  today, which means the panel button silently overrides a refusal the API
  and CLI respect. Force is an override; an override that is always on is
  not one. The panel gets a checkbox.
- **Image auto-update respects the upper-env gate.** An upper env exists to
  receive promotions and nothing else; `apply:141` is named in `02` as the
  sanctioned path. A tile with `update_policy=auto` rebuilding there
  (`infra/imagewatch/watcher.go:175`) defeats the point of the ladder. If
  someone wants an auto-updating upper-env tile, that is a promotion policy,
  not an image watch.

Files: `service/deploy.go`, `service/release.go`, `service/plan.go`,
`service/staging.go`, `service/applyengine.go`;
`stackconf`/`staging`/`envops`/`orgconf` 84 handler sites (api 25, web 47,
prhook 12), `infra/deploy` 31, `workqueue` 8.

### 8. BackupScheduleService + BackupDestinationService

Depends on: points 1, 3.

Methods: `2.10`, as written.

Rows closed: `1.7` — 5 flagged rows (3 A, 2 B). The scheduler half is
already closed by point 1 [verified]. Remaining:

- [A] create schedule: three kind derivations, `VolumeFor` missing on
  config, `backup.Validate` not called by config, keep default 7 on web and
  0 everywhere else, no managed gate on a file-owned key [from 02].
- [A] update: web clears cron and tz from the form, API and CLI cannot clear
  tz, config plans and applies `cur[0]` only so extra panel schedules are
  invisible and get deleted on block removal [from 02].
- [A] create destination: web trims, API stores raw, so `ResolveNamed`
  misses a name with a stray space [from 02].

Picks — decided:

- SP3: `backup.Validate` runs on every path, config included. One `VolumeFor`
  guard. One trim.
- SP3: the `backup:` key is file-owned, so `editGate` applies on both
  surfaces. Neither has it today.
- Keep default: 7. The DB default is 7 and dead; 0 means "keep nothing",
  which is not a sane default for a backup.
- Tz clearable everywhere. "Cannot clear" is an artefact of patch-non-empty,
  not a rule.

Picks — decided (was a dev call):

- **The file owns the whole schedule list for a tile.** Config planning and
  applying `cur[0]` only (`stackconf/backups.go:119,232`) means extra panel
  schedules are invisible to the plan and then deleted when the block is
  removed — the plan says one thing and does another, which is the worst of
  the three options. One `backup:` block is the full desired set. Panel
  creates on a file-owned tile go through `editGate` and stage, like every
  other file-owned field.

Files: `service/backupschedule.go`, `service/backupdestination.go`;
`backup` 9 sites, `backup.LoadSchedules` 10 (routed via point 1);
`destinationUsers` duplicated verbatim twice.

### 9. VolumeService + StorageService + VolumeOwnership

Depends on: points 3, 7.

Methods: `2.9`, as written.

Rows closed: `1.2` — 9 flagged rows (5 A, 3 B, 1 C). The three C rows are
out of scope (see "Not in scope"); a service cannot fix an identity
mismatch. Remaining:

- [B] re-attach a volume from A to B: web enqueues both targets, config
  deploys only the new one, so A keeps a stale bind [from 02].
- [B] `DELETE /apps/:id` with a volume id runs `teardownTile` and never
  enqueues the attached service, unlike `deleteVolume`; web refuses to
  delete an attached volume and neither API path does [from 02].
- [B] web `DeleteStorage` has no `OrgID` branch, so it skips
  `DropOrgShareVolumes` and the managed-org check and cannot see
  `${{ org.storage.NAME }}` refs [from 02].
- [A] create volume, target rule three ways: web service-and-unmanaged, API
  any non-managed non-volume app including cron, config any tile [from 02].
- [A] API `createStorage` hard-codes `ServerID:"local"`, web uses `:id`
  [from 02].
- [A] storage attach: config service-only, API writes for any kind, web has
  no kind check [from 02].
- [A] sub-path name: web slugifies then checks empty, API checks empty then
  slugifies, so a punctuation-only name yields `Name==""` [from 02].

Picks — decided:

- SP3 gives one target-kind rule (service, unmanaged), the attached-volume
  refusal on every delete path, the kind check on storage attach, and the
  `OrgID` branch on delete.
- SP2: re-attach redeploys both the old and the new target.
- Sub-path name: check empty *after* slugifying. The API ordering is the
  one that catches a punctuation-only name.
- `ServerID` comes from the caller, never hard-coded. The API's `"local"`
  is why CLI pools always land on the manager.

Picks — resolved by reading:

- **`TileConf` grows both fields.** Read `store/repo/sqlite/tiles.go:184-212`:
  `volume_name` and `max_size_mb` are real columns and `UpdateTile` already
  writes them. They are tile settings like `mount_path` next to them, and
  the only reason config cannot declare them is that nobody added the
  `TileConf` field. Adding them also fixes the staged create, which cannot
  carry either today because it round-trips through `TileConfOf`.

Picks — decided (was a dev call):

- **`ConvertLegacyMounts` becomes a `VolumeService.Create` caller, at the
  back of the point.** It is behind a once-flag so it only runs on an
  install that predates volume tiles, but leaving a fifth writer with its
  own slug rule is exactly the thing this refactor deletes, and as a caller
  it is a small change. If the flag is set on every rig, deleting it
  outright is also fine — check `volumes_migrated` before writing code.

Files: `service/volume.go`, `service/storage.go`, `service/volumeown/`
(a leaf, per D1); `storagetiles` 6 sites; `volmove` from web only; the 13
"which volumes does X own" implementations listed in `1.2`.

### 10. RegistryAdminService + OrgRegistryService + ConnectorService + PREnvService

Depends on: points 2, 6, 7.

Methods: `2.8`, as written.

Rows closed: `1.9` — 7 flagged rows (6 A, 1 B). Named:

- [SEC, A] web `DeleteRegistry` (`web/handler/settings/handler.go:657`) is
  `DeleteRegistry(c.Param("id"))` with no load and no managed guard, where
  the API 409s. A POST removes the managed registry and boot recreates it
  with a fresh password, breaking every org's derived system credential
  until `EnsureSystemCredential` runs [verified].
- [SEC, A] org registry credential create and delete, and tag delete, are
  owner on web and any member on the API. A member with a `stacks:write`
  key mints a long-lived push/pull credential [from 02].
- [B] API `patchRegistry` skips `registry.EnsureManaged` that web runs;
  traefik routes to an alias the service may not carry [from 02]. The proxy
  half is closed by point 2.
- [A] create org credential: the API 503s without a managed registry, web
  mints without one [from 02].
- [A] delete image tag: name validation and the in-use matcher differ, so a
  stored `ImageTag` without a host is protected by the API and deletable
  from web [from 02].
- [A] set tile connector: four copies of "connector must belong to org", two
  of which fail open — the planner allows when `len(OrgConnectors)==0` and
  the engine silently nils, producing an unauthenticated clone [from 02].
- [A] PR env settings have no `editGate`/`rejectManaged` though the file
  overwrites `comment`/`status` at PR open, and `pr_envs.enabled` is never
  persisted [from 02].

Picks — decided:

- SP3: the managed guard on delete, the `ValidRepoName`/`ValidTag`
  validation, the in-use matcher, and the org check on connectors — all four
  copies become one, and it fails closed.
- SP3: `editGate` on PR env settings, since `comment` and `status` are
  file-owned.
- SP2: `EnsureManaged` runs on every path that sets the managed domain.
- Delete connector gains a reference check over `tile.ConnectorID`,
  `stack.ConfigConnectorID` and `org.ConfigConnectorID`. Today the dead id
  is kept and fails later in `cigate` and the planner.
- Stack-scoped hook gains `stackTracksRepo`, matching the connector route.
  Without it a manual webhook opens `pr-N` on a stack tracking another repo.

Picks — decided (were dev calls):

- **Org registry credentials are owner.** A push/pull credential is
  long-lived, it is shown in plaintext exactly once, and it grants write to
  the org's images. Web already treats it as owner
  (`web/org/registry.go:166,193,218`); the API's member level
  (`api/v1/registry.go:26-34,101,200`) is the outlier and is the [SEC] row.
  Tag delete goes with it — it destroys an artefact a running tile may
  reference. Recorded in point 15's table, decided here.
- **`PREnvService.Update` is sparse — an explicit exception to D6.** D6
  exists to kill the eleven copies of the *tile* field list, where a sparse
  patch means a forgotten field. `PRConfig` is a four-field settings blob
  with a rotate-secret side channel, not an entity with copies, so the
  reason for D6 does not apply. Sparse also stops the CLI read-merge-writing
  (`internal/cli/cmd/lifecycle.go:625-647`) to work around PUT replacing
  `enabled`.

Files: `service/registryadmin.go`, `service/orgregistry.go`,
`service/connector.go`, `service/prenv.go`; `registry` 11 sites,
`githubapp` 10, `cigate` (janitor plus prhook); four inline connector
filters.

### 11. OrgService + MemberService + OrgPlanService + AuthService + APIKeyService

Depends on: point 14 (MailService) for the invite path. Otherwise standalone.

Methods: `2.5`, as written. `AuthService` and `AdminService` already exist
in the core package and get extended, not created.

Rows closed: `1.8` — 12 flagged rows (10 A, 2 B). Named:

- [SEC, A] `KeyAuth` (`api/v1/auth.go:163-166`) loads the user by id and
  checks only that the row exists — never `Active`. Deactivating a user
  closes nothing: sessions and API keys both keep working [verified].
- [SEC, A] any org member can plan, approve and apply an org plan from the
  API/CLI; web is owner-only [from 02].
- [SEC, A] API key write scopes are granted against the active cookie org
  while keys have no org column; demotion leaves scopes in place [from 02].
- [A] invite bounds, role coercion, remove-without-404 all differ [from 02].
- [A] password strength lives in three handlers with two rules and none in
  the service [from 02].
- [B] the API never mails an invite; `*API` has no Mailer [from 02].

Picks — decided:

- SP1: `--role admin` 400s instead of silently becoming `member`.
- SP3: the member-exists check, the last-owner guard and the 404 on remove
  apply on both surfaces.
- `Active` is checked at principal resolution, not only at login. One
  `PasswordStrength` in the service, called by all three handlers.
- One invite expiry rule: default and cap, both surfaces.

Picks — exception to SP3:

- **Org plan approval level.** SP3 would keep web's owner-only. That is
  probably right, but it is a genuine authorisation design question rather
  than a forgotten guard, and applying an org plan can rename the org and
  create or delete stacks. Flagged to the dev below rather than decided by
  a standing rule.

Picks — decided (were dev calls):

- **Org plan approval is owner-only.** Applying an org plan can rename the
  org, and create or delete stacks. Web is already owner-only; the API's
  member level is the outlier and is the [SEC] row. A member who needs to
  ship config gets a stack binding, not org-level apply.
- **API keys keep no org column; scopes are validated at use time against
  current roles in the target org.** This is the `2.5` brief's answer and
  the only one that survives a demotion — an org column freezes the grant at
  mint time, so demoting a member leaves their key writing. Use-time
  validation also removes the reason `cancelDeployment` was exploitable:
  the scope stops being the whole authorisation.
- **Email folds at the store.** `store/repo/sqlite/users.go:29`
  `GetUserByEmail` is exact and `users.email` UNIQUE is case-sensitive
  (`001_initial.up.sql:341`), so the eight handler sites that lowercase are
  a workaround for a store that does not. Fold in the store, and take a
  one-time migration to collapse any existing case-variant rows. Doing it at
  eight entry points is how the ninth gets forgotten — which is exactly what
  `web/account/handler.go:78` already is.
  Note: this needs the settle-first runtime check first, not to decide the
  fix but to size the migration (whether any duplicate-by-case rows exist).

Files: `service/org.go`, `service/member.go`, `service/orgplan.go`,
`service/apikey.go`, extend `service/auth.go` and `service/admin.go`;
two `orgconf.Runner` inline constructions; two API-key minting copies plus
the unauthenticated `Exchange` third site (`web/cli/handler.go:153-161`).

### 12. NotificationService → `service/notify`

Depends on: nothing. A leaf, and mostly a move — it can land any time after
point 1, including in parallel.

Methods: `2.13`, as written.

Rows closed:

- [D7] the `infra/ → handlers/` inversion. Four `infra/` packages import
  `handlers/notify` today (`infra/deploy/engine.go:533`,
  `infra/jobs/jobs.go:421`, `infra/imagewatch/watcher.go:142`,
  `infra/cigate/cigate.go:117`) [verified by grep in the review].
- [B] API `stopApp`, `restartApp`, `toggleCron`, `createDB` deploy,
  `setProvisionPublic`, `putStackVars`, `putEnvVars` fire no nudge where web
  does [from 02].
- [A] `KindDeployFailed` fires with two payload shapes, and `KindImageUpdate`
  is reused for check failures, so opting out of update notifications also
  drops failure alerts [from 02].

Picks — decided:

- SP2: the nudge happens in the service, so both surfaces get it. That is
  most of the [B] row, and it lands for free as points 3, 5 and 6 route
  through it.
- Image check failures get their own kind. Reusing `KindImageUpdate` makes
  an opt-out silently suppress alerts, which is the failure mode a
  notification system must not have.
- One title/body builder for `KindDeployFailed`.

Files: move `handlers/notify` to `service/notify`; `Push` 7 call sites (all
`infra/`), ws nudges ~40.

### 13. SettingsService + NodeService + AdminService

Depends on: point 2 (`Proxy.Resync` after every settings write).

Methods: `2.11`, as written.

Rows closed: `1.10` — 5 flagged rows (4 A, 1 D). Named:

- [D] `service/admin.go:16` imports `docker/api/types/swarm`, the only class
  D hit in the tree [verified by grep].
- [A] org defaults: web is owner plus a 409 on config-managed, the API is
  member with no managed check, and the next apply overwrites either
  [from 02].
- [A] server defaults: web renames the server unvalidated on any `:id`, the
  API is fixed to `"local"` and never sets the name [from 02].
- [A] agent ensure has two entry policies: boot only with 2+ nodes and 10
  retries, `AddNode` once and unconditional [from 02].
- Node lifecycle rules (five) live only in `web/server/nodes.go`, so any
  node API re-derives them [from 02].

Picks — decided:

- SP3: the managed gate applies at every level, not just web org.
- SP1: an empty server name 400s.
- One `EnsureAgent` retry policy, the boot one, used by `AddNode` too. The
  case the boot retry exists for (registry not up yet) is exactly what a
  first-node `AddNode` hits.
- `AdminService` drops the swarm import; the spec shape it needs comes from
  the InstallerSpec package (point 16).

Picks — decided (was a dev call):

- **All three levels are managed-gated.** Config owns `defaults:` and
  replaces the whole blob at org (`orgconf.go:687-700`), stack
  (`apply:932-937`) and env (`apply:955-964`), so an ungated write at stack
  or env level is a write the next apply silently reverts — the same bug as
  point 6's apply-policy row, and `02` already flags the org-level version
  of it. Gate all three; web org is the one that already gets it right.

Files: `service/settings.go`, `service/node.go`, extend `service/admin.go`;
settings web 4 + api 4 + config 2; proxy admin web 6, api 4, cli 5; nodes
web only, 10 sites.

### 14. MailService → `service/mail`

Depends on: nothing structural. A leaf, one method.

Methods: `2.14`. `SendInvite(ctx, org, invite, link) (failed bool)`.

Rows closed: [B] the API never mails an invite (`2.5`, `2.14`) [from 02].

Picks — resolved by reading:

- **Not blocked. `BASE_URL` is already available.** Read
  `cmd/stackrd/main.go:74,125-130,577`: it is parsed at boot into
  `baseOrigin` and already handed to the web server config. `MailService`
  takes `baseOrigin` at construction and builds the invite link itself, so
  it never needs an `echo.Context`. This point is small.
- **`MailFailed` is deliberately not persisted.** `repo.Invite.MailFailed`
  is `db:"-"` with a comment saying it is set for the creating request only,
  so the page can say the mail bounced while the link still works
  (`store/repo/models.go:91-94`). Not a gap. **Remove it from the
  settle-first list.**

Picks — decided (was a dev call):

- **Mailer stays env-only.** Adding a settings surface for SMTP is a feature,
  not part of an extraction, and nothing in `02` depends on it. Out of scope
  for this point; it can be a `SettingsService` level later.

Files: `service/mail/`; one writer today.

### 15. AccessService

Depends on: points 3, 5, 6 landed, so the pattern is proven. Deliberately
late despite having the most detailed sketch in `02`.

This one stays a brief plus a table, not a full point. `2.5`'s eight-part
brief is the design and does not need restating. The work is filling the
per-operation table, one level per verb.

Four rows are already decided by the points above, and they set the pattern
for the rest — **where the surfaces disagree, the higher level wins, because
the lower one is a forgotten guard and not a considered grant**:

| verb | level | decided in |
|---|---|---|
| `registry.credential.create` / `.delete` | Owner | point 10 |
| `registry.tag.delete` | Owner | point 10 |
| `orgplan.approve` / `.apply` | Owner | point 11 |
| `deployment.cancel` | Write | point 7 |

The remaining rows follow the same rule, and the two that do not have an
obvious higher level are the wizard steps (`/setup/domain`,
`/setup/connector`, member today where the rest of the wizard is owner —
pick Owner, a draft org has no members who need them) and the read verbs
(Read, uncontested).

What it closes: the confirmed [SEC] rows where the two surfaces picked
different levels for one verb, plus D4 — one error policy, decided once,
translated per surface.

Entry criterion: the table above extended to every verb in the `g-access`
list. That is mechanical now that the rule is named; it was the open
question before.

Files: `service/access.go`; every gate helper in the `g-access` table,
`orgForCreate`, `requireConfigStack`, `requireBackup`,
`resolveResourceTenancy`, and the web `loadStack`/`load`/`loadTile`/
`loadDeployment`/`loadBackup`/`settingsOrg`/`ownedOrg`/`ownedSettingsOrg`/
`loadOrg` family.

### 16. Small candidates

From `2.15`. Each is small and depends on a service above. Specced together
because none is a point on its own.

- **AuditService** → `service/audit`, a leaf. Thin: `Record`, `PanelViews`,
  `ServeValue`. Closes [B] the unaudited secret writers — `apply:1802-1816`,
  `orgconf.go:723-727`, `envops.go:219-231,274-281,548-553`,
  `managedtiles.go:383-389`, `provision_s3.go:177-178` [from 02]. Decided:
  SP2, `VariableService.Set/Unset` records on every path, so audit stops
  being a caller duty. Lands with point 6.
- **ImageWatchService** — owns `image_digest` baselining for its three
  writers, the check history, the interval setting, and routes the auto
  branch through DeployService (inheriting the upper-env gate) and
  ManagedInstanceService (which writes status). Depends on points 3, 5, 7.
  The upper-env question is decided in point 7: auto-update respects the
  gate.
- **ContainerService** — or a `NodeService` method set. Decided by SP3: one
  system-container guard covering stop, remove, start and terminal. Today
  stop guards any system container, remove only a running one, start and
  terminal nothing (`web/container/handler.go:202,212,224-235`,
  `terminal.go:36`). Web-only today; an API twin needs this first.
  Depends on point 11.
- **MetricsService** — already one implementation. Needs only a home outside
  `infra/metrics` that does not import `managedtiles`. Depends on point 11.
- **InstallerSpec** — a package, not a service, and it runs outside the
  server. The panel service spec (env names, mounts, network, hostname,
  constraints) lives only in `internal/installer/steps.go:128-169` with a
  hand copy in `scripts/dev/deploy-test.sh:118-131`, so a new env var
  reaches fresh installs and never upgraded ones. Defined once, imported by
  the installer, `AdminService.Upgrade` and `seed.go`. Takes the root-domain
  grammar too, so the panel and the installer refuse the same hosts —
  `localhost` is refused by the installer and accepted by the panel today.
  Relay port 15000 is the same kind of shared constant.

### 17. OrgPlanApply on the work queue

Depends on: points 7 and 11. Added after 7–11 landed, because it is the one
row from `1.8` those points did not close — the attempt is recorded in
`04-progress.md` and `05-assumptions.md`.

Rows closed: `2.5`'s `[B]` "org plan apply runs inline on the request
context" (`api/orgconfig.go:125`, `web/org/config.go:214`).

The stack-level apply went on the queue long ago. The org-level one did not,
so approving an org plan still runs `orgconf.Runner.Apply` to completion on
the request goroutine. What is inside it is not the whole installation, as
first written — the declared stacks only *plan*, and their applies go on the
queue as usual — but it is unbounded network work all the same: one bucket
probe per declared org share, and one repository fetch per declared stack
(twice for the ones that need the second middleware pass), plus a teardown
per stack the file has stopped declaring.

The damage is not the wait. It is that `SetOrgConfigPlanError` writes through
the same context: a request that dies mid-apply loses the record of the
failure, and the plan row keeps the *previous* run's message and stays
`pending` for ever. That is the exact failure `stackconf/job.go` was written
to stop.

**Why it was not done in point 11.** Both approve handlers redirect to the
org's *post-apply* slug, because an org plan can rename the org and the old
slug 404s. The setup wizard is worse: its next step lives under that new
slug. Queuing it was tried, the wizard's own journey tests failed on exactly
that coupling, and it was reverted rather than left half done.

Picks — decided:

- **The redirect uses the org id, not the slug.** `loadOrg` already accepts
  an id — that is why a bookmark from before a rename still resolves — so
  `/orgs/<id>/plans/<planID>` survives whatever the file renames the org to.
  This is what unblocks the whole point, and it is invisible to the user.
- **Requeue on restart, not fail.** An org apply re-loads the file, re-diffs
  against whatever exists now, and reconciles, which is the same convergent
  property that decided `stackconf.RegisterApply`. Unlike a promote, it is
  not a decision that could be re-taken wrongly.
- **Dedupe on the plan id**, matching `stackconf.EnqueueApply`: two people
  pressing Apply on one plan is one apply.
- **The progress banner is the stack plan page's, lifted.**
  `project/configplan.templ` already loads the plan's `WorkItem` and renders
  a banner that polls itself every 2s through `hx-select` and stops when the
  job is done, with the step text coming from the job rather than guessed
  from the plan status. Both org plan pages get it — `orgPlanPage`
  (settings) and `setupPlanPage` (the wizard).
- **The wizard stays on the plan screen and advances itself.** It polls like
  the settings page; when the job finishes the poll answers `HX-Redirect` to
  the next step, built from the org id. Same end state as today, one
  "Applying: …" screen in between. Not a Continue button: approving already
  means "get on with it", and an extra click is a step the wizard never had.
- **The API answers 202 with the plan still pending**, matching
  `POST /config/plans/:id/approve`. `stackr org config approve` reports it
  as queued and points at the plan, which is what `stackr plan approve`
  already does.

Not in scope: `Runner.Apply` itself, which does not change. Nor the org
share probe and the per-stack fetch inside it — those are what make it slow,
and making them concurrent is a separate question from where they run.

Files: `config/orgconf/job.go` (new, ~70 lines, modelled on
`config/stackconf/job.go`); `cmd/stackrd/main.go` (one registration);
`api/v1/orgconfig.go`, `web/org/config.go`, `web/org/setup.go` (approve
paths and their two redirects); `web/org/plans.templ`, `web/org/setup.templ`
(the banner); `internal/cli/cmd/org.go` (the approve's output line).

### Pick tally

Across points 3 to 16: **66 picks, all made.** 42 by the three standing
picks, 9 by reading the code, 15 by judgement. Two standing-pick exceptions
(points 6 and 7) and one D6 exception (point 10), each argued in place.

Three findings were **downgraded** by the reads, and they matter because
`02` counts them as drift:

- The two "did not finish" vocabularies (point 7) are two meanings, not
  drift. `waiting_ci` surviving a restart is deliberate — `cigate` re-adopts
  it.
- Deployments needing an atomic claim (point 7): they do not. The owning
  work item is claimed atomically, which guards the row one level up.
- `inv.MailFailed` not persisting (point 14) is `db:"-"` by design, with a
  comment saying so. Removed from settle-first.

One read **changed a decision**: D6 ("services take full structs") does not
extend to the store. `UpdateTile` excludes status, slug, shared_net and
home_node on purpose, each for a named past bug. The service takes a full
struct; the UPDATE keeps its exclusions.

Three picks are **reversible product calls** — defensible either way, and
the ones to overrule first if they feel wrong: promote force defaulting to
false (point 7), the file owning the whole `backup:` schedule list
(point 8), and auto-update respecting the upper-env gate (point 7).

Nothing is now blocked on a decision. The gate on starting is the
settle-first list, which is two items shorter than it was.
## Not in scope

- The class C rows (`1.2`). An engine package does not fix an identity
  mismatch; they need a table or a resolver first. Recorded, scoped
  separately.
- Boot one-shots stay in `main.go` as callers (`2.15`), not services.
- Graph and canvas handlers stay in `handlers/web` (D5).
- Encryption-at-rest decisions (settle-first item 3) are a store change.

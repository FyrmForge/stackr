# Sweep E — boot, cmd binaries, installer

Files read (every non-generated, non-test `.go` in the shard):

| File | Lines |
|------|-------|
| `cmd/stackrd/main.go` | 912 |
| `cmd/stackrd/agent.go` | 83 |
| `cmd/stackrd/seed.go` | 67 |
| `cmd/stackrd/ws.go` | 47 |
| `cmd/stackr/main.go` | 11 |
| `cmd/stackr-install/main.go` | 11 |
| `cmd/proxyrelay/main.go` | 29 |
| `internal/installer/answers.go` | 200 |
| `internal/installer/flags.go` | 216 |
| `internal/installer/form.go` | 214 |
| `internal/installer/steps.go` | 378 |
| `internal/installspec/installspec.go` | 174 |
| `internal/deploystate/deploystate.go` | 48 |
| `internal/netaddr/netaddr.go` | 35 |
| `internal/proxyrelay/proxyrelay.go` | 92 |

Total 2517 lines.

`main.go` is DI wiring and most of it is legitimate: constructing services, filling
`rows`/`ops` knots, handing the two routers the same values. The findings below are
only the places where wiring turns into deciding.

## Findings

### cmd/stackrd/main.go:365 — finish deploys the previous process abandoned
- **Rule:** a deployment row left `running`/`queued` by a dead process is stale and
  must be failed, except `waiting_ci`, which `cigate` re-adopts.
- **Category:** 4 (direct store call from boot) + 3 (boot recovery step).
- **Owner it should have:** `DeployService` (a `RecoverStale`/`Boot` method) — it
  already owns which deployments may exist and what statuses mean
  (`internal/deploystate`).
- **Duplicated at:** the complementary half of the rule lives in
  `infra/cigate` (re-adopt `waiting_ci`); the exclusion is only implicit here.
- **Severity:** medium. (Previously audited — confirmed at this line.)

### cmd/stackrd/main.go:493-509 — the deploy-finished rules
- **Rule:** (a) a `function` tile with `RunOnDeploy` fires a `TriggerDeploy` job
  when its own deploy reaches `done`; (b) a `done` deploy of an `image` tile
  baselines the pulled digest so the image watcher has something to compare to.
- **Category:** 1 (branch on `tile.Kind`, `tile.RunOnDeploy`, `tile.SourceType`,
  `d.Status`) + 4 (`clus.LocalDigest`) + 5 (side effect fired by the wirer).
- **Owner it should have:** the engine's own post-deploy step, or
  `DeployService`; the digest baseline belongs with `ImageWatchService`, which is
  the only reader of `tiles.image_digest`.
- **Duplicated at:** `store.SetTileImageDigest` at `:506` is the only write to
  that column outside `service/imagewatch.go`; `TileService` owns every other
  tile-row write.
- **Severity:** high — two domain rules and a raw store write in a closure that
  no test can reach, chained onto `gh.DeployFinished` at `:385-387` by
  shadowing (`prevFinish`), so the order of two unrelated effects is decided in
  `main`.

### cmd/stackrd/seed.go:31-45 — a second, thinner DomainResourceService.Create
- **Rule:** `ROOT_DOMAIN` becomes an `instance`-level domain resource owned by
  the `local` server, unless the host is already taken.
- **Category:** 2 + 6 (duplication across surfaces).
- **Owner it should have:** `DomainResourceService.Create`
  (`service/domainresource.go:170`).
- **Duplicated at:** `service/domainresource.go:170`. The copy here skips
  `ValidateResourceHost` (`:29`, which is the shared `installspec.CheckRoot`
  grammar plus the scheme/port/case rules) and keeps `HostTaken`. So
  `ROOT_DOMAIN=Foo.example.com:8080` seeds a row the panel would have refused.
- **Correction (`12-business-logic-plan.md` §2 #2):** this entry originally
  claimed the copy also skips `canonLevel`, `checkOwner` and the ACME/proxy
  resync. It does not, in any way that matters — `seed.go` hardcodes
  `"instance"`, so `canonLevel` is a no-op for it, and it sets no ACME email,
  so there is nothing to resync. **One skipped rule, not four.**
- **Severity:** medium (was recorded as high on the four-rule reading).

### cmd/stackrd/seed.go:25-66 — the whole first-boot seeding workflow
- **Rule:** seeding runs once, keyed on `KeyInstallSeeded`; a bad
  `TRUSTED_PROXY_CIDRS` aborts the whole seed; `TRUST_CLOUDFLARE=1` only turns
  the setting on if the range fetch succeeded, otherwise it stays off with a
  warning; the seeded flag is written last.
- **Category:** 3 (orchestration: four writes with decisions between them).
- **Owner it should have:** `SettingsService` (it already owns
  `KeyTrustedProxies`/`KeyTrustCF` and the proxy resync) — a `SeedInstall` method
  taking the three answers.
- **Duplicated at:** the "off rather than on with nothing trusted" rule is the
  Proxy page's, spelled again here; the parse policy itself is correctly shared
  (`svcproxy.ParseTrustedList` → `netaddr.ParseTrusted`).
- **Severity:** medium — the rules are right, they just live in `package main`
  where only a running daemon exercises them.

### cmd/stackrd/ws.go:17-47 (wired at main.go:344-361) — websocket room authorisation
- **Rule:** the room→permission map. `notifications` is open to any signed-in
  user; `containers`/`server`/`flows` need `p.Admin`; `org:<id>` needs
  `VerbOrgRead`; `project:<id>` resolves the stack and checks `VerbOrgRead` on its
  org; anything else is refused. Plus "the user must exist and be active".
- **Category:** 1 + 4 (raw `store.GetUserByID`, `store.GetStack` from `package
  main`).
- **Owner it should have:** `AccessService` — it already owns "may this principal
  do this verb in this org"; the room parsing belongs next to the room names in
  `service/notify`.
- **Duplicated at:** the same admin/org/stack checks are spelled per handler in
  `handlers/web` and `handlers/api/v1`; the user-active check duplicates
  session middleware. `RevokeService` (`main.go:723`) exists to close rooms a
  membership change invalidated — it and this function disagree about who owns
  room membership.
- **Severity:** high — it is an authorisation decision, it fails closed only by
  accident of the `switch` default, and it is unreachable from any test that
  does not build `main`.

### cmd/stackrd/main.go:459-484 — the nightly cleanup policy
- **Rule:** every 24h: truncate `traefik/access.log` if over 50MB
  (unconditionally, not gated on the setting); then, **only if**
  `KeyCleanupEnabled == "1"`, prune docker on the manager node and garbage-collect
  the registry.
- **Category:** 1 (branch on a settings value) + 3 (three effects in an order that
  matters: GC takes a read lock on the whole registry store) + 4 (`clus.Prune`,
  `registry.GarbageCollect` straight from boot).
- **Owner it should have:** `AdminService` (it already owns the box-level
  maintenance surface) or a `CleanupService`; the 50MB/24h numbers are policy and
  should sit with the setting that switches the rest of it on.
- **Duplicated at:** none found — which is the problem: the panel has no way to
  run this on demand, so there is no second spelling to drift from yet.
- **Severity:** medium.

### cmd/stackrd/main.go:562-572 — "ensure the agent only on a real swarm"
- **Rule:** `EnsureAgent` runs at boot **iff** `rt.ListNodes` returns two or more
  nodes; a list error is also a skip.
- **Category:** 1 (branch on cluster state) + 4 (`rt.ListNodes` from boot).
- **Owner it should have:** `NodeService.EnsureAgent` itself — the comment above
  it (`:551-559`) says the upgrade path depends on this running on every boot of
  a multi-node swarm, which makes the guard part of the rule, not part of the
  wiring.
- **Duplicated at:** `service/node.go` owns the retry policy but not this gate;
  the panel's "Add node" path reaches `EnsureAgent` without it, so a
  single-node box adding its second node gets a different answer than a reboot
  does.
- **Severity:** medium.

### cmd/stackrd/main.go:445-456 — the metrics slice reader closure
- **Rule:** slice stats for a tile come from a freshly constructed
  `managedtiles.Service`, and a nil read with a nil error means "no stats"
  rather than an error.
- **Category:** 3/4 — an adapter with a decision in it, plus a second
  `managedtiles.NewService(clus, store, rows)` (the first is `dbService` at
  `:486`) built purely to break an import cycle.
- **Owner it should have:** `managedtiles` should expose the `metrics.SliceRead`
  shape, or `metrics` should take `dbService`; either way the mapping is not
  wiring.
- **Severity:** low — but it is the one place boot constructs a service twice.

### cmd/stackrd/main.go:590-592, 600-607 — node sync/ping schedule
- **Rule:** the manager writes its own swarm id onto its `servers` row at boot,
  then every 30s re-syncs and pings every other node's swarm port.
- **Category:** 3 + 5 (a periodic side effect owned by the caller).
- **Owner it should have:** `NodeService` (or `nodes.Service` itself) — the
  cadence is a health-check policy, and `nodeSvc.Sync`'s comment explains a
  correctness bug that depends on it running.
- **Duplicated at:** `nodeSvc.Sync` is also called from the servers page; only
  boot runs the ticker.
- **Severity:** low.

### cmd/stackrd/main.go:576-583 — daily upgrade check
- **Rule:** check for a new release at boot and every 24h so the rail badge has
  something to show.
- **Category:** 5 (scheduled side effect fired by the wirer).
- **Owner it should have:** `AdminService` — `CheckUpgrade` is already its
  method; the cadence belongs with it, next to the janitor tasks at `:657-663`
  which is where every other periodic task is registered.
- **Severity:** low — inconsistency more than a rule (three different scheduling
  mechanisms in one file: `janitor`, `time.Ticker`, and bare `time.Sleep` loops).

### cmd/stackrd/main.go:266-272, 309, 596, 691 — the boot recovery sequence
- **Rule:** at boot, in this order: create the overlay pool, sweep networks whose
  environment is gone, sweep leftover forward relays, sweep leftover move
  receivers, backfill an overlay onto every environment that has none.
- **Category:** 3 (a startup sequence that encodes policy) + 4 (`rt.*` and
  `netpool.*` called directly with the raw store).
- **Owner it should have:** one `Recover(ctx)` on the services that own the rows
  — `EnvironmentService` for the network backfill and sweep (`netpool.Backfill`
  already takes `envSvc`, so it is half-moved), the forward/move sweeps on
  whatever owns relays.
- **Duplicated at:** `cmd/stackrd/agent.go:64` repeats `rt.SweepMoveReceivers`
  for the agent side — correct, but two callers of an unowned recovery step.
- **Severity:** low — the comments make the ordering explicit and it is genuinely
  boot-only work; flagged because `netpool.Sweep(ctx, store, rt)` takes the raw
  store at `:272`.

### cmd/stackrd/main.go:302-306 — managed registry start
- **Rule:** the registry is ensured in a goroutine so a slow `registry:2` pull
  does not block boot; a failure is logged, not fatal; a missing signer
  (`:282-286`) is also non-fatal and downgraded to a 503 on the token route.
- **Category:** 1 (the fatal/non-fatal decision is a policy) + 4
  (`registry.EnsureManaged` gets both `store` and `registrySvc`).
- **Owner it should have:** `RegistryService` — it was extracted for exactly this
  row (commit "the managed registry gets one owner"); `EnsureManaged` still takes
  the raw store beside it.
- **Severity:** low-medium — the store parameter is the leftover half of an
  extraction that already happened.

## Clean files

- `cmd/stackr/main.go`, `cmd/stackr-install/main.go`, `cmd/proxyrelay/main.go` —
  entry points, one line each.
- `cmd/stackrd/agent.go` — process bootstrap for a binary with no database and no
  domain rows. `SelfImage` → `MoveImage`/`VolumeToolImage` (`:42-45`) and
  "no key, exit" (`:48-55`) are deployment facts about this process, not domain
  decisions.
- `internal/proxyrelay/proxyrelay.go` — a TCP pipe with an idle reaper. No domain
  concepts at all.
- `internal/netaddr/netaddr.go` — one trusted-proxy grammar, and it is the shared
  one: `svcproxy.ParseTrustedList` (`service/proxy/proxy.go:367`) and
  `installer.CheckProxies` (`answers.go:115`) are both thin wrappers over it. The
  panel and the installer cannot drift.
- `internal/deploystate/deploystate.go` — the deploy status vocabulary, factored
  out precisely because four copies had drifted. Rules, but rules with one home
  that everything imports.
- `internal/installspec/installspec.go` — the panel's service spec and the
  root-domain grammar, shared by the installer and `AdminService.Upgrade`
  (`service/admin.go:34`, `service/domainresource.go:39`,
  `infra/runtime/runtime.go:86`). This is the model the rest of the shard should
  copy.
- `internal/installer/form.go`, `flags.go` — TUI construction, flag binding,
  DNS warning text. Every `Validate` closure delegates to the `Check*` functions.
- `internal/installer/answers.go` — validation, but of *installer* answers (data
  dir, ports, Let's Encrypt email, panel host under root). The one rule the
  daemon also holds (root-domain grammar) is delegated to `installspec.CheckRoot`
  at `:99`, and the proxy list to `netaddr` at `:119`. No duplication.
- `internal/installer/steps.go` — docker commands, swarm pool selection,
  `daemon.json` merge, preflight. Host-level install work with no daemon
  counterpart; the service spec itself is delegated to `installspec.CreateArgs`
  at `:126`.

## Note on the installer binaries

The brief asked whether the installer duplicates rules the daemon's services own.
It does not, any more: `installspec` and `netaddr` exist specifically to hold the
two rules both sides need (domain grammar, trusted-proxy grammar), and both sides
import them. The only place installer-shaped knowledge is re-spelled outside
those packages is `cmd/stackrd/seed.go`, which is the daemon reading the
installer's answers and writing them with its own, weaker rules.

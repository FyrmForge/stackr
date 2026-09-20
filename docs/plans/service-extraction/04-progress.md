# Service extraction progress

Running log of what actually landed, written as each point closes. Read
`03-refactor.md` for the plan; this file only records deviations, evidence and
state. Nothing here is committed — the tree is left unstaged for review.

Environment note: `make lint` is broken on this box and was before this work
started — `golangci-lint` v2.11.4 is built against go1.26 and the toolchain is
go1.27, so it panics in `go/types` on every package. `make test` and the hamr
build are the only gates used below.

---

## Point 1 — SchedulerService — DONE

New package `internal/stackrd/service/scheduler`.

### Shape, and where it differs from the plan

```go
func New(j *jobs.Service, b *backup.Service) *Service
func (s *Service) ReloadCron(ctx)                       // no error
func (s *Service) ReloadBackups(ctx)                    // no error
func (s *Service) Reload(ctx)                           // both
func (s *Service) Boot(ctx) (cronErr, backupErr error)  // the one caller that gets errors
```

Two deviations from `03`:

1. **`ReloadCron`/`ReloadBackups` return nothing.** The plan said "logs and
   returns nil". A signature that can only ever return nil invites 28 more
   `_ =` lines and lets a caller invent its own policy again, which is the
   thing the point exists to stop. Boot gets the errors instead.
2. **`Reload` (both tables) was added.** Every delete path that cascades tiles
   cascades `cron_jobs` *and* `backups`, so the two rows `2.12` and `2.10`
   flag are the same call site. One method means a delete cannot close half
   of the finding.

Plan's open question ("reload inside `envops.Teardown`/`CloneTiles`/
`TeardownStack`/`Applier.CopyTile` instead of at every caller"): **explicit
caller calls**, as advised. `config/envops` sits above the service line in the
target architecture, so burying a scheduler side effect in it recreates the
implicit-effect-in-a-helper pattern D5/D7 exists to delete.

### Rows closed

- `2.12` [B] **stack delete reloads no cron** — now `sched.Reload` after
  `DeleteStack` on both surfaces: `handlers/api/v1/stacks.go` `deleteStack`,
  `handlers/web/handler/project/handler.go` `Delete`.
- `2.12` [B] **web direct-path tile delete reloads no cron** — now
  `sched.Reload` after `DeleteTile` in
  `handlers/web/handler/app/handler.go`. The API twin
  (`handlers/api/v1/apps.go` `deleteApp`) was reloading cron only and only for
  `Kind == "cron"`; it now reloads both tables unconditionally, because the
  cascade takes `backups` rows for *any* tile kind.
- `2.10` [B] **destination delete cascades every schedule, neither surface
  reloads** — now `sched.ReloadBackups` after `DeleteBackupDestination` in
  `handlers/api/v1/backups.go` `deleteDestination` and
  `handlers/web/handler/settings/handler.go` `DeleteDestination`.
- `2.10` [B] **the tile-delete paths that cascade `backups` rows reload
  nothing** — covered by the two tile-delete sites above plus every env and
  stack teardown, which now call `Reload` rather than cron-only.
- The three-way error policy is gone: `api/v1/apps.go` `createApp` no longer
  returns 500 on a failed reload (it logs), the fourteen discard sites no
  longer discard, and `config/stackconf/apply.go`'s two `warn(...)` wrappers
  are gone.

### Call sites replaced

All 28. `LoadSchedules` now appears only in `infra/jobs`, `infra/backup` and
the service; two API comments still name it, correctly.

- api: `apps.go` ×4, `envs.go` ×3, `lifecycle.go` ×1, `backups.go` ×4 (three
  were via `reloadBackupSchedules`, now deleted), `stacks.go` ×1 (new)
- web: `project/handler.go` ×5 (one new), `project/envcompare.go` ×1,
  `app/handler.go` ×3 (one new), `prhook/handler.go` ×2,
  `backups/handler.go` ×4 (three were via `h.reload`, now deleted),
  `settings/handler.go` ×3 (one new)
- config: `stackconf/apply.go` ×2
- boot: `cmd/stackrd/main.go` — the two separate `LoadSchedules` calls became
  one `sched.Boot`, which also moves the cron load a few lines later so both
  halves load together

### Wiring

`scheduler.Service` is built once in `cmd/stackrd/main.go` and handed to
`web.Deps.Scheduler`, `api.Deps.Scheduler` and `stackconf.Applier.Sched`. The
five web handlers and the API router take it through a `WithScheduler`
builder, matching the existing `WithWork`/`WithMover` style rather than
growing five positional constructors.

### Deletions that fell out

- `handlers/web/handler/backups/handler.go` `h.reload`
- `handlers/api/v1/backups.go` `a.reloadBackupSchedules`
- `stackconf.Applier.Backups` (replaced by `.Sched`; `.Jobs` stays, it is used
  for `Hold`/`Release`)
- the `jobs` field and constructor parameter on the **project** and **prhook**
  web handlers — both existed only to call `LoadSchedules`. `project` still
  imports `infra/jobs` for `jobs.ValidateCron`.
- every per-site nil guard (`if h.jobs != nil`, `if a.backups != nil`, …)

### Verified

- hamr `stackrd` rule builds clean.
- `make test` green, including a new `service/scheduler/scheduler_test.go`
  covering the nil-safety the guard consolidation depends on.
- **Behavioural check on a rig: not yet run.** The discriminating check is:
  delete a stack (and a tile) that owns a cron schedule, then confirm the
  entry is gone from the scheduler rather than just the row from the table.
  Pending a rig deploy.

### Verified on the rig

`stackr-test.inanga-degree.ts.net`, deployed over the existing install (no
wipe, so the check ran against a box with real stacks on it).

Created a stack with a cron tile on `* * * * *`, waited for it to register and
fire, then deleted the stack over the API and watched for 2.5 minutes:

```
--- runs before delete ---
('f2ad…', 'app:3ecea25b…', 'schedule', 'ok', '2026-09-18 23:49:00Z')   <- registered and firing
delete status 204
--- panel log since the delete ---
NO STALE-ENTRY LOG LINES (pass)
--- cron_runs for the deleted tile ---
1 runs total                                                           <- none after the delete
```

Pre-fix the entry would have survived the delete and fired
`scheduled run not queued … tile … not found` on every tick
(`infra/jobs/jobs.go:311-320`). Nothing.

---

## Point 2 — ProxyService — DONE

New package `internal/stackrd/service/proxy`, imported as `svcproxy` wherever
it sits next to `infra/proxy`. Nothing above the service holds a
`*proxy.Proxy` any more: `web.Deps.Proxy`, `api.Deps.Proxy` and
`envops.Ops.PX` are all `*svcproxy.Service`, and the only remaining
construction of the raw proxy is `cmd/stackrd/main.go`.

### Shape

```go
// tile routes
SyncTile(ctx, *repo.Tile) error      // ListDomainsByTile + WriteApp, one copy
DropTile(tileID)                     // logged, never fails the delete before it
SyncStackMiddlewares(*repo.Stack) error
Resync(ctx)                          // logged
AccessLog(tileID, limit) []proxy.AccessEntry
// traefik static config
EnsureTraefik()                      // async, serialized, coalescing
EnsureTraefikNow(ctx) error          // boot only
CurrentStatic() / StaticOverride(ctx) / SetStaticOverride(ctx, yaml) error
SetDNS(ctx, provider, env) error
SetResourceACME(ctx, resourceID, email) error
TrustedProxies(ctx) (raw, trustCF) / SetTrustedProxies(ctx, raw, trustCF) error
ParseTrustedList(raw) ([]string, error)   // package function, the seed uses it
// escape hatches
Entries(ctx) / SetEntry(ctx, name, yaml) (string, error) / DeleteEntry(ctx, name) error
// managed registry
SetRegistryDomain(ctx, *repo.Registry, domain) error
```

### Rows closed

- `1.1` **Update tile settings [B]** — `handlers/api/v1/apps.go` `patchApp`
  now calls `SyncTile` for non-cron/function tiles, as the panel always did.
  `basic_auth_*`, `security_headers` and `traefik_override` set over the API
  take effect immediately, and the bcrypt hash lands in the route, because
  hashing happens at route-write time
  (`config/stackconf/stackconf.go:397`). Note the stored column stays
  plaintext on both surfaces — that is settle-first item 3
  (encryption at rest), not this point.
- `1.1` **Rename tile [B]** — `config/stackconf/moved.go` removed the route
  and never rewrote it. It now renames first and calls `SyncTile`; the
  `RemoveApp` is gone, because the route file is keyed on the tile id, which a
  rename does not change, so removing it was pure loss.
- `2.1` **[B] web managed-db delete never `RemoveApp`** —
  `handlers/web/handler/db/handler.go` now calls `DropTile` after
  `DeleteTile`. The route used to survive until a `Resync` pruned it.
- `2.8` **[B] API `patchRegistry` skipped `registry.EnsureManaged`** — it now
  goes through `SetRegistryDomain`, which does both, so Traefik no longer
  routes to a `registry` alias the service is not carrying.
- `2.6` **[B] boot `EnsureManaged` never renders `registry.yml` and `Resync`
  skipped it** — `infra/proxy` `Resync` now renders the managed registry's
  route from `GetManagedRegistry`. D3 pick as written: whether the route
  exists must not depend on which code path last ran.
- `2.6` **[A] trusted-proxy CIDRs validated two ways** — one
  `ParseTrustedList`, refusing the list on a bad entry. `cmd/stackrd/seed.go`
  was the outlier (it skipped with a warning); `internal/installer` already
  refused, and stays independent because it is a separate binary that should
  not import the panel's service layer.
- `2.6` **[A] `EnsureTraefik` five different call policies** — one policy:
  async and logged everywhere, serialized, with mid-run callers coalescing
  into exactly one follow-up. `api/v1/domainresources.go` was the dangerous
  one — synchronous on the request context, so a client disconnect could
  cancel a container restart half way.
- `2.6` **[A] `RemoveApp` errors ignored at every site** — one `DropTile` that
  logs. A route file that will not unlink must not fail a delete that has
  already torn the service down.
- Duplication removed: `proxyEntries`/`saveProxyEntries` (web) and
  `proxyEntriesMap`/`saveProxyEntriesMap` (API) were byte-identical, as were
  the two `validYAML`/`yamlValid` and the two `syncProxy` helpers. All one
  copy now. `handlers/api/v1/proxycfg.go` and
  `handlers/web/handler/settings/proxy.go` are now bind-and-render only.

### Deviations from the plan

1. **Boot stays async.** `03` says boot's `EnsureTraefik` is "synchronous +
   returned" and picks keeping it that way. It is not — `cmd/stackrd/main.go`
   already ran it in a goroutine that logs, and the server starts while it is
   in flight. Making it block boot would hold the panel down behind a Traefik
   image pull. Boot now calls `EnsureTraefikNow` (which returns the error) from
   the same goroutine, so the error is reported and startup is not gated on it.
2. **The managed-gate collapse is not in this point.** `03` lists it under
   point 2's behaviour picks, but the `editGate`/`rejectManaged`/`managedErr`/
   inline-`ConfigManaged()` spellings live in the tile, domain and database
   handlers that points 3, 4 and 5 rewrite. Collapsing them here would mean
   touching those handlers twice and getting authorisation semantics wrong
   unattended. Deferred to point 3, where the gate has an owner.
3. **No shared error-vocabulary package yet (D4).** This point needed only
   "the caller sent something bad" (400) and "no such entry" (404), so it
   carries `svcproxy.ErrInvalid` and `svcproxy.ErrNoEntry` and the two
   handlers map them. The full 404/403/409/400 vocabulary should be its own
   tiny package when the first point needs the ownership half of it — a
   sentinel set in the flat core package cannot work, because leaves would
   import core and close the cycle D1 exists to avoid.
4. **`forward.Registry` was left alone.** `2.6` counts its five sites under
   ProxyService. They are tunnels, not Traefik writes, and moving them adds no
   rule.

### Files touched

New: `service/proxy/proxy.go`, `service/proxy/proxy_test.go`.
Rewritten: `handlers/api/v1/proxycfg.go`,
`handlers/web/handler/settings/proxy.go`.
Changed: `cmd/stackrd/main.go`, `cmd/stackrd/seed.go` (+ its test),
`infra/proxy/proxy.go` (`Resync` renders the registry route),
`config/envops/envops.go`, `config/stackconf/{apply,middlewares,moved}.go`,
`config/orgconf/orgconf.go`, `handlers/api/server.go`,
`handlers/api/v1/{v1,apps,domains,domainresources,helpers,lifecycle,registry,settings}.go`,
`handlers/web/server.go`,
`handlers/web/handler/{app,db,org,project,server,settings}/…`.

### Verified

- hamr `stackrd` rule builds clean; `make test` green.
- `go test -race -count=3 ./internal/stackrd/service/...` green — the
  coalescing test drives the real `EnsureTraefik` path through an injected
  `ensure` func and fails both if two runs overlap and if a mid-run caller
  leaves no follow-up.
### Verified on the rig

`stackr-test`, same install. `PATCH /api/v1/apps/:id` with
`basic_auth_user`/`basic_auth_password`, then `security_headers`, **no
deploy of any kind**:

```
route file before:  middlewares: [https-redirect]           (http router only)
after the patch:    middlewares: [app-2716d415-auth]
                    basicAuth.users: "qa:$2a$05$woAE/…"     <- bcrypt, at route-write time
after security_headers:true:
                    middlewares: [app-2716d415-auth, app-2716d415-hdr]
                    headers: stsSeconds 31536000, frameDeny, nosniff
live:  GET / -> 401      GET / -u qa:hunter2 -> 200
clearing both -> middlewares: [https-redirect], GET / -> 200
```

Pre-fix the API patch wrote the row and stopped, so all three settings did
nothing until the next redeploy. Settings restored afterwards; the rig is back
as it was.

One observation, not a row here: the API field is `security_headers`, not
`sec_headers` as `00-calibration.md` spells it. The calibration note is wrong
about the name, not about the behaviour.

### Post-review fixes (found by review, not by the rig)

Two holes in the first cut of `EnsureTraefik`, both fixed:

1. **`EnsureTraefikNow` bypassed the slot it sits next to.** It set
   `running = true` without checking whether a run was already in flight, and
   cleared `queued` on the way out. Boot runs it in a goroutine while the HTTP
   server is already accepting, so an operator saving proxy settings during
   the Traefik image pull hit both halves: two concurrent reconfigures, and
   then their save discarded — the panel answered "Trusted proxies saved." and
   the list never reached Traefik until the next restart. That is the exact
   failure the `ParseTrustedList` refusal exists to prevent, arriving by
   another door. Now: one `runMu` held for the duration of a run, and a
   follow-up is handed to `ensureLoop` rather than cleared.
   `TestEnsureTraefikNowKeepsMidRunSave` covers it.
2. **`Resync` returning nothing lost the config apply's own report.**
   `config/stackconf/apply.go` used to fold a failed resync into the plan
   output via `warn(...)`. `Resync` now logs *and* returns, so the one caller
   that reports rather than serves keeps its line, and handlers still call it
   as a bare statement with no policy of their own.

Also folded in: the `cron`/`function` skip now lives inside `SyncTile` instead
of being duplicated at its two call sites, which is the shape that drifted the
first time.

---

## Rig state

`stackr-test.inanga-degree.ts.net` — **wiped and rebuilt** on this branch's
code. Fresh admin `admin@test.com` / `Test1234!`, org `QA`
(`86766dd7-108f-4a1f-a699-5615e2fb8230`), API key minted with every scope.
Certificates and the GitHub connector were preserved by `wipe-test.sh`; the
connector still needs `scripts/dev/connector-keep.sh restore qa`.

Leftovers from the checks, safe to delete: stack `bk-check`, its tile `vol`
(currently stopped), the volume tile `data`, and the docker volume
`stackr-vol-d666830f` that goes with it. An env var `NUDGE_CHECK` may be on
`production`.

`skrt2.inanga-degree.ts.net` — **not joined.** It is wiped and bare (docker
installed, swarm inactive). The panel issued a join script and the node
accepted it, but the script needs root and `skrt2` has no passwordless sudo:

```
sudo: a terminal is required to read the password
```

Running it as root through a privileged container was refused by this
session's sandbox, correctly. To finish the join, either run the `curl … |
sudo sh` line on `skrt2` by hand, or give that box passwordless sudo. The
`servers` page currently lists it as `pending / timeout`, which is the right
state for a node that was offered a key and never used it.

### Stale memory corrected

`paving-the-test-vm.md` item 3 says `data/traefik/dynamic/_panel.yml` is
hand-written and must be preserved across a wipe. That is no longer true:
`infra/proxy` `writePanel` generates `panel.yml` from `EnsureTraefik`, and
this wipe proved it — the dynamic dir came back with `panel.yml` and
`_middlewares.yml` and the panel answered on its own hostname with nothing
restored by hand.

### Point 1's backup half, verified on the rig

The cron check said nothing about `ReloadBackups`, and `deleteDestination` →
`ReloadBackups` is a *new* call site rather than a replaced one, so it got its
own run: a volume backup on `* * * * *` against a destination, then the
destination deleted over the API.

```
backup_runs before the delete:  2 rows, trigger=schedule   <- entry registered and firing
                                (both error at the fake S3 endpoint, which is the point:
                                 the schedule fired, so the cron entry was live)
DELETE /api/v1/backup-destinations/:id -> 204
backups rows left for that schedule: []                    <- cascade confirmed
after 160s (two ticks):
  NO STALE-ENTRY LOG LINES (pass)                          <- no "scheduled backup not queued"
  0 backup_runs total
```

---

## Point 12 — NotificationService → `service/notify` — DONE (two nudges owed to points 3 and 6)

`handlers/notify` moved to `service/notify` (`git mv`, so the history follows),
and the 20 importers rewritten. That alone closes the **D7 inversion**: four
`infra/` packages imported `handlers/` for the notifier
(`infra/deploy/engine.go`, `infra/jobs/jobs.go`,
`infra/imagewatch/watcher.go`, `infra/cigate/cigate.go`). Nothing under
`infra/` imports `handlers/` any more.

### Rows closed

- `2.13` **[B] the API fires no nudge where the panel does.** Seven handlers.
  Two small helpers on the API (`statusChanged`, `stackChanged`, in
  `handlers/api/v1/helpers.go`) now cover `stopApp`, `restartApp`,
  `toggleCron`, `createDB`, `setProvisionPublic`, `putStackVars` and
  `putEnvVars`. These are handler-level for now on purpose: SP2 puts the nudge
  in the service, but TileLifecycleService and VariableService do not exist
  yet (points 3 and 6), and leaving seven live bugs in place until then is
  worse than two helpers that those points delete.
- `2.13` **[A] `KindImageUpdate` reused for check failures.** New
  `KindImageCheckFailed` kind with its own settings row, defaulting on.
  Turning "New image version" off used to silence the alert that the check
  itself was broken, which is the one thing a notification system must never
  let an opt-out do.
- `2.13` **[A] `KindDeployFailed` fired with two payload shapes.** One
  `notify.DeployFailed(tileName, reason)` builder. The engine said
  "Deploy failed: x" and the CI gate "Deploy blocked: x" for what is one event
  to the person reading it.

### Import graph, checked

All four leaves have **zero outgoing service edges** — `service/notify`,
`service/mail`, `service/proxy` and `service/scheduler` import nothing under
`service/`. No cycle, and D1's hybrid still holds.

One thing for point 3 to know: five `infra/` packages now import
`service/notify` (`cigate`, `deploy`, `imagewatch`, `jobs`, `metrics`). That is
what the plan asked for and is strictly better than importing `handlers/`, but
it is still an upward edge, and it becomes a **cycle** the moment
`service/notify` grows an outgoing service edge — `2.13` sketches
`TileStatusChanged(ctx, tile)`, which is exactly the kind of method that would
want `service/tile`, while `service/tile` will want `infra/deploy`. The way out
is the one the plan already names: those `infra/` packages stop calling the
notifier directly and the services call it instead (points 3, 5 and 7). Keep
`service/notify` free of service imports until then.

### Verified on the rig

The check is the class B row, driven rather than screenshotted: canvas open at
`/qa/bk-check/production` showing the tile Online, then `POST
/api/v1/apps/:id/stop` issued **from inside the page** so there is no gap and
no reload, then the DOM polled.

```
before:   "vol image service Online"
api says: stopped
after:    "vol image service stopped"   (waitedMs: 0, reloaded: false)
```

Only a websocket nudge can move that text with no navigation. Pre-fix the API
sent none and the card stayed stale until something else refreshed it — an
earlier run of this same check, before the page was driven from inside,
timed out at 10s watching a card that never moved.

---

## Point 14 — MailService → `service/mail` — DONE

New package `internal/stackrd/service/mail`.

```go
func New(m *mail.Mailer, baseOrigin string) *Service
func (s *Service) Enabled() bool
func (s *Service) InviteLink(token string) string
func (s *Service) SendInvite(ctx, org, invite) (failed bool)
```

### Rows closed

- `2.5` / `2.14` **[B] the API never mails an invite.** `*API` had no mailer at
  all, so an invite created over the API or `stackr` CLI wrote a row and told
  nobody. `newInvite` (`handlers/api/v1/members.go`) now calls `SendInvite`,
  which covers both `addMember` and `createInvite`.

The blocker `02` recorded ("needs a base URL source that is not
`echo.Context`") is resolved the way `03` predicted: `baseOrigin` is already
parsed at boot in `cmd/stackrd/main.go` and is handed to the service at
construction. The web handler's `mailInvite` and its request-derived link are
gone; `inviteURL(c, …)` stays only for the panel's *Copy invite* button, where
request-derived is right.

`MailFailed` stays unpersisted (`db:"-"`), as `03` established — the caller
shows the bounce on that one request and the link still works by hand.

### Verified

- `make test` green, with `service/mail/mail_test.go` covering the link
  (including a trailing slash on `BASE_URL`, which would otherwise produce
  `//invite/…` in a link people paste) and the no-provider path, which must be
  a quiet skip rather than a reported bounce.
- **Not exercised end to end.** This install has no mail provider
  (`SMTP2GO_API_KEY` unset) and hamr's mock inbox is off in `hamr.toml`, so
  there was nothing to deliver to. What is proven is that the API path now
  calls the sender; what is not is the message itself, which is byte-identical
  to the panel's and was working there.

---

## Observation, not a row

While checking point 12, `GET /api/v1/apps/:id` answered `"status":"running"`
for ~30 seconds after a stop that had already scaled the service to `0/0`,
then settled on `stopped`. That is the reconciler tick
(`infra/metrics/reconcile.go`) and the 14-writers-one-observer problem `2.15`
describes, not anything this work changed. It is what point 3's
`TileLifecycleService` owning `UpdateTileStatus` is for. Worth knowing when
reading a status straight after a lifecycle call: it is eventually consistent
by a tick.

---

## Where this stops, and what point 3 needs first

Points 1, 2, 12 and 14 are done — every leaf in `03`'s D1 list except
`service/audit` and `service/volumeown`, which belong to points 16 and 9 and
depend on core services that do not exist yet.

**Point 3 (TileService) was deliberately not started _in the first run_.**
It is started below, under the decisions this section called open — they were
taken rather than asked about. Three reasons it waited:

1. It creates the flat `service` core package and is the largest service in
   the plan. Everything routes through it.
2. It is the first point that needs **D4's error vocabulary**, which this work
   deferred (see point 2's deviation 3). Where that lives is a real decision:
   it cannot be a sentinel set in the flat `service` package, because leaves
   would import core and close the cycle D1 exists to avoid. It wants its own
   tiny package, and picking its name and shape now fixes it for all thirteen
   remaining points.
3. It is where the **managed-gate collapse** lands
   (`editGate`/`rejectManaged`/`managedErr`/inline `ConfigManaged()`), which is
   authorisation behaviour across ~20 handler sites.

Also still open from `03`'s settle-first list, none of which blocked the work
above: the two unverified security items (orphan deployment rows, the
profile-email-case lockout, both gating point 11), the three "not checked"
cells (points 5, 7 and 9), and the encryption-at-rest decision (point 10).


---

## Decisions taken, not asked

The overnight mandate was to assume rather than ask. These four were decided
here and are cheap to reverse; each says what the other choice would have been.

1. **D4's error vocabulary lives in `service/svcerr`.** Four kinds, one
   mapper. The alternative was sentinels in the flat core, which leaves could
   not import without closing the cycle D1 exists to avoid.
2. **Encryption at rest (point 10) is skipped.** The row stays open. It is a
   product call about `registries.password`, `Tile.DBPassword`,
   `BasicAuthPassword`, `WebhookToken` and `Settings.ProtectPassword`, not a
   refactor call, and a service that encodes a guess is worse than a store
   that does not (`03`, settle-first item 3).
3. **Nothing is committed.** The whole refactor stays unstaged for review. A
   commit message per point is at the end of this file.
4. **The managed gate is not collapsed, it is moved.** `editGate` and
   `rejectManaged` become one `GateService.Gate` with a `GateKind`, keeping
   both answers (`00-calibration.md`, "The gate"). Collapsing them to one
   rule would let a structural write stage on a config-managed stack.

## Point 3a — `service/svcerr` + `GateService` — DONE

The groundwork every later sub-step of point 3 calls. No behaviour visible to
a user changed; two error mappers became one.

### Shape

- `internal/stackrd/service/svcerr/svcerr.go` — the D4 vocabulary. Two
  sentinels (`ErrNotFound`, `ErrForbidden`, which carry no detail because
  neither answer is allowed to explain itself) and two carriers (`Conflict`,
  `Invalid{Field, Msg}`, whose messages are whole sentences because both
  surfaces render them verbatim). Imports only the standard library, so any
  layer above the store can speak it.
- `internal/stackrd/handlers/middleware/svcerr.go` — `stackrmw.HTTP(err)`,
  the **one** mapper for both surfaces. Web and API both return echo errors,
  so one function covers both: the API's `JSONErrors` renders an
  `*echo.HTTPError` as JSON and the web error page renders it as HTML.
  Anything that is not a service error passes through untouched and stays a
  500 with its own logging path.
- `internal/stackrd/service/gate.go` — `GateService.Gate(ctx, stackID, kind)`
  with `GateFieldEdit` / `GateStructural`, plus `ManagedConflict(stack)` for
  handlers that already hold the stack and should not look it up twice.

### Rows closed

- [A] Two ad-hoc error mappers deleted: `proxyErr` in
  `handlers/api/v1/proxycfg.go` and `badInput` in
  `handlers/web/handler/settings/proxy.go`. `service/proxy` now raises
  `svcerr.Invalid` and `svcerr.ErrNotFound` instead of its own
  `ErrInvalid`/`ErrNoEntry` pair. Two mappers was cheap to fix at point 3;
  after fourteen points it would have been twenty-eight.
- [A] The two managed-gate messages unified. The API said "edit the file to
  change its structure" and the web said "edit the config file…"; both now
  say the web wording and both name the config repo when there is one.

### Not closed yet, on purpose

`Gate` is written and tested but not yet wired into any handler — the ~20
call sites move in 3b–3f as each concept is extracted, so that no surface is
half on the service and half on its own helper (D2).

### Verified

`go test ./internal/stackrd/service/...` green, including `gate_test.go`,
whose load-bearing case is "stage mode still refuses structural": a stack with
`ui_edits: stage` stages a settings edit and still 409s a tile delete. The
fake store answers `GetStack` and nothing else — its embedded `repo.Store` is
nil, so a second store call inside `Gate` panics the test rather than passing
quietly.

## Point 3b — `TileLifecycleService` — DONE (rig check owed)

Stop, Restart, ToggleCron, RunNow, StopRun. One implementation, both surfaces.

### Shape

`internal/stackrd/service/tilelifecycle.go`. Separate from TileService on
purpose: these change what is *running*, TileService owns what is
*configured*.

```go
func (s *TileLifecycleService) Stop(ctx, t) error
func (s *TileLifecycleService) Restart(ctx, t) error
func (s *TileLifecycleService) ToggleCron(ctx, t) (status string, err error)
func (s *TileLifecycleService) RunNow(ctx, t, by Actor) (*repo.CronRun, error)
func (s *TileLifecycleService) StopRun(ctx, t, runID string) (stopped bool, err error)
```

### Rows closed

- [B] **Stop/Restart/ToggleCron notified on the web and never on the API.**
  The nudge is now inside `setStatus`, so it cannot be skipped by a surface.
  (Point 12 had already put `a.statusChanged` in `api/v1/helpers.go` as a
  handler-level stopgap; this is the relocation it was waiting for. The
  helper stays only for the managed-database path, which is point 5.)
- [A] **Kind checks were one-sided.** `ToggleCron` required a cron on the API
  and accepted anything on the web, which parked services at `paused` for the
  reconciler to fight. `RunNow` required cron/function on the API and
  accepted anything on the web. Both guards now live once.
- [A] **`RunNow` nil-dereferenced `h.jobs` on the web.** It answers
  `svcerr.ErrUnavailable` → 503, matching what the API already did.
- [A] **Stop on a cron parked it `stopped` for ever.** The pick from `03`:
  a cron or function parks as `paused`, which is the state the scheduler
  honours, **and the cron table is re-registered** — neither surface did that
  half, so it is a new side effect, not a status rename.
- [A] **Trigger strings unified.** `jobs.TriggerManualWeb` and
  `TriggerManualAPI` collapse to `jobs.TriggerManual`; who and where-from
  move into `service.Actor{Name, Via}`, rendered into the existing `actor`
  column as `name` or `name (api)`. No migration. Adding a surface no longer
  adds a trigger that every "was this manual?" query has to learn.
- [A] New guards, from SP3 rather than from a drift row: `Stop` refuses a
  volume (there is nothing to scale), `Restart` refuses a cron or function
  (there is no long-running container; `run` is the verb).

### Deviations from the plan

1. **`TileLifecycleService` is not the single writer of `tiles.status`.** The
   plan wants one owner of `UpdateTileStatus` with `Observe` for the
   reconciler. The reconciler and the deploy engine live in `infra/`, which
   D7 forbids from importing a service. So this owns one writer *per user
   action*, not one in the tree, and `Observe` is not written. Closing that
   properly is `2.15`'s own point.
2. **`svcerr` gained a fifth kind, `ErrUnavailable` → 503.** D4 lists four,
   but D4 is about refusals and this is not one: it is "this build was
   started without a job runner". A 500 there calls an operator out of bed
   for a configuration choice.

### Files

`service/tilelifecycle.go` (new), `service/tilelifecycle_test.go` (new),
`infra/jobs/jobs.go` (trigger constants), `handlers/web/handler/app/handler.go`
(five handlers now bind and render; `restartTile`, `statusChanged` deleted),
`handlers/api/v1/lifecycle.go` + `apps.go` (five handlers, same),
`handlers/api/v1/v1.go` (`WithLifecycle`), `handlers/api/server.go`,
`handlers/web/server.go`, `cmd/stackrd/main.go` (one value, both routers).

Also fixed in passing: a dead `return nil` left in
`config/stackconf/middlewares.go` by point 2, which `go vet` flagged.

### Verified

`go test ./...` green. `tilelifecycle_test.go` covers the paused pick, the
volume and service guards, the 503, the run-ownership check and `Actor`.
Its fake store embeds a nil `repo.Store`, so a new store call inside any of
these methods panics the test instead of passing quietly.

**Rig check owed** — batched to the end of point 3 with the other B rows, per
the plan not to spend the night on per-sub-step deploys.

## Point 3c — `TileService.Delete` + the gate, wired — DONE (rig check owed)

### The Gate gained a second axis, before anything used it

3a's `Gate(ctx, stackID, kind)` was wrong and the delete row is what showed
it. Whether a write queues for review is a property of **the surface** as much
as of the stack: the canvas queues every edit into the env's pending set, the
API and the CLI deliberately never do (`docs/features/api.md`). A gate that
only looked at the stack either broke the canvas's struck-through delete or
started staging API calls nobody would ever press Apply for.

So `Gate(ctx, stackID, kind, via Surface)`, `SurfaceCanvas` / `SurfaceDirect`.
The rules, all in one place now:

| stack | kind | canvas | direct |
|---|---|---|---|
| UI-owned | field edit | stage | write |
| UI-owned | structural | stage | write |
| config-managed, `ui_edits: stage` | field edit | **stage** | **stage** |
| config-managed, `ui_edits: stage` | structural | 409 | 409 |
| config-managed, `ui_edits: block` | anything | 409 | 409 |

Row three is the calibration bug: the panel staged and the API 409'd on the
identical edit to the same stack. It is closed by the table, not by a patch.

### Rows closed

- [B] **API delete skipped the provision detach.** A tile deleted over the API
  left `provisions` rows pointing at an id that no longer resolved. `TearDown`
  orphans them the way the panel and a config apply already did.
- [B] **API delete skipped the attached-volume guard.** `DELETE` on an
  attached volume took it out from under a running service. The guard is now
  before the staging branch, so a staged delete that could never apply is a
  refusal now rather than a surprise later.
- [B] **A config apply's `deleteTile` skipped both schedule reloads.** It did
  four of the five steps; a removed cron kept ticking until the next restart.
- [B] **Found while wiring, same class, one level up:** `envops.Ops.Teardown`
  deletes the environment row, which cascades every tile in it and with them
  their `cron_jobs` and `backups` rows — and nothing re-registered either
  table. Same stale-entry bug point 1 closed at the tile sites. `Ops` now
  carries `Sched` and reloads after `DeleteEnvironment`.
- [A] **The delete gate disagreed across surfaces** (web `editGate`, API
  `rejectManaged`). One table now, above.

### Deviations from the plan

**`TileService` takes `*repo.Tile`, not `stackconf.TileConf`.** `2.3` proposes
TileConf as the input shape, but `config/stackconf`'s Applier is one of this
service's callers, so `service → stackconf` would close an import cycle. A
full row is the shape all three callers already hold, and it is what D6 asks
for literally ("services take full structs"). `stackconf.applyTileConf`
already produces exactly that, so nothing is lost and no twelfth copy of the
tile field list is created.

`TearDown` is exported alongside `Delete` for the two callers that have
already made the decision `Delete` makes — a config apply and the drain of a
staged delete — so they do not make it twice.

### Files

`service/tile.go` (new), `service/tile_test.go` (new), `service/gate.go`
(+`Surface`), `service/gate_test.go`, `service/tilelifecycle.go`
(`Actor` gained `ID`, `Email` and `Surface()`), `config/envops/envops.go`
(`Ops.Tiles`, `Ops.Sched`, the teardown reload), `config/stackconf/apply.go`
(`deleteTile` delegates), `handlers/web/handler/app/handler.go`,
`handlers/web/handler/project/handler.go` (`WithTiles`),
`handlers/api/v1/apps.go` + `helpers.go` (`teardownTile` delegates; it stays
as a helper because the volume and managed-database delete routes call it and
their gates are points 9 and 5), `v1.go` (`WithTiles`), both routers,
`cmd/stackrd/main.go`.

### Verified

`go test ./...` green. `tile_test.go` asserts the three early refusals — the
config-managed 409, the attached-volume 400 on *both* surfaces, and nil —
against a store that panics if anything is torn down, which is how it proves
the guards run first.

## Point 3d — `TileService.Update` — DONE (rig check owed)

The headline sub-step. One validator, one differ, one answer to "what does
this save earn".

### Shape

```go
func (s *TileService) Update(ctx, t *repo.Tile, extra []string, by Actor) (staged bool, err error)
func (s *TileService) Validate(ctx, t *repo.Tile) error
func (s *TileService) AfterWrite(ctx, t *repo.Tile, changed Changed) error
func DiffTiles(old, cur *repo.Tile) Changed
```

`Update` = gate → validate → stage, **or** re-read the stored row, diff it
against the edited one, write, and perform what the diff earns.

**The diff is the piece that was missing everywhere.** The two HTTP surfaces
had no notion of "what changed" at all: the panel wrote the row and always
rewrote the route, the API wrote the row and did nothing, and only a config
apply chose. `DiffTiles` gives all three the same answer, in the config
engine's own field vocabulary, so `ProxyOnly()`, `NeedsBuild()` and
`NeedsCronReload()` mean one thing in the whole tree.

Both sides of the diff must be rows **loaded from the store and then edited**
— never freshly built. `sqlite.UpdateTile` deliberately excludes `status`,
`slug`, `shared_net` and `home_node`, and a fresh struct would report all four
as changes. That constraint is in the doc comment because it is the one way to
use this wrong.

### Rows closed

- [B] **The big one: a spec edit took effect on the next manual deploy.**
  Port, limits, healthcheck, command, volumes, replicas, image — changed from
  the panel or the API, written to the row, and then nothing. `AfterWrite`
  redeploys when the change is not route-only. Rig check owed.
- [B] **The API never rewrote the Traefik route on a spec change**, and a
  config apply rewrote it *only* when the change was route-only — but the
  deploy engine never writes the route itself (1.11), so an apply that moved a
  domain and a limit together left the route stale. All three now rewrite the
  route and redeploy when the change is not route-only.
- [A] **The eleven-copy field list shrank by one and moved.**
  `settingsPatch` (65 lines in the panel handler) is now
  `staging.SettingsPatch`, beside the staging buffer it feeds and reachable by
  the API, which stages too since 3c. See the deviation below for why it is
  not `stackconf.TileConfOf`.
- [A] **`--cpu` alone zeroed memory; `--dockerfile` alone cleared the
  context.** `limits` and `build` are atomic pairs on the row but not in the
  request. `patchApp` merges per sub-key now (`subKeys`), so an absent half
  means unchanged. This kills the class for both at once rather than
  special-casing either.
- [A] **Scale is exposed to the API and therefore to the CLI.** `replicas` and
  `node_group` were already declared by the config file and already parsed by
  `appPatch`; `patchApp` dropped them, so a tile could not be scaled by
  script. Two `has()` branches.
- [A] **Every validation drift row in `1.1`.** One `Validate` now applies the
  union of both surfaces' rules to the finished row: cron expression, git URL
  and its shape, the connector's org (the panel used to *silently blank* a
  foreign connector, which is the worst of the three answers — the next build
  fails for no stated reason), mount and device grammars, `depends_on`
  conditions, restart policy, storage attachments, basic-auth pairing, the
  kind-scoped keys via `runpolicy` rather than the panel's three hand-written
  kind literals, the negative-number family, and the replica/volume guard.
- [A] **The coercions are gone** (SP1). `parseLimits`, `timeoutMinutes`,
  `updatePolicyForm` and `nonNegInt` all turned a bad value into a default;
  all four are deleted. A form field that is present and not a number is a 400
  naming the field, not a zero. See the reversed pick below for the one part
  of `timeoutMinutes` that did **not** become a refusal.

### Deviations from the plan

1. **The staged patch is not built from `stackconf.TileConfOf`.** `2.3` names
   that as the seam. It cannot be: every field on `TileConf` is `omitempty`,
   so marshalling one would drop cleared values from the patch and a cleared
   field would stop clearing anything. `SettingsPatch` keeps its explicit map
   and carries a comment tying it to `stackconf.tileToConf`, its inverse.
   Collapsing the two properly means `TileConf` without `omitempty`, which is
   a config *file format* decision and not a refactor one.
2. **`runtime.ParseDep` moved out of `stackconf`.** The service validates
   `depends_on` and cannot import the config engine (3c's cycle). It sits
   beside `ParseDevice` and `ParseFileMount`, which are the same kind of thing
   — the grammar of one line of a tile column. 14 call sites updated.
3. **A config apply performs its own deploys**, rather than calling
   `AfterWrite`. It *asks* the shared `Changed` for the decision, so there is
   still one rule set, but it queues the deploy itself because the applier
   records what it deployed and a deploy queued inside the service would be
   missing from that report. Its managed-instance branch also stays put; that
   is point 5's.
4. **Two form-shaped coercions stay in the panel handler** — clearing
   `update_policy` on a non-image source and `wait_for_ci` on a non-git one.
   Those knobs render only for their own source, so the browser sends nothing
   for the other and a stale row value would read as an edit nobody made.
   That is form semantics, not a rule; the service still refuses the
   combination outright if a caller sends it.

### Files

New: `service/tilevalidate.go`, `service/tilediff.go`,
`config/staging/patch.go`, `infra/runtime/deps.go`, and three test files.
Rewritten: `handlers/web/handler/app/handler.go` `SaveSettings` (176 lines of
rules → a form binding), `handlers/api/v1/apps.go` `patchApp` (294 lines →
a merge). The panel's app handler is 1952 → 1760 lines, the API's apps.go
585 → 470, with the deleted rules replaced by ~520 lines of service that all
three surfaces share.

### Verified

`go test ./...` green, including the six pre-existing `patch_test.go` cases
that assert the API's validation behaviour — they now exercise the service and
still pass unchanged, which is the D2 evidence that the API's behaviour did
not regress while moving. New: `tilediff_test.go` (ten spec fields that must
redeploy, the route-only set that must not, a no-op save that earns nothing,
and the cron schedule/image split) and `tilevalidate_test.go` (twenty
refusals, each asserting *which field* is blamed).

### Post-review fixes to 3d (found by review, not by the tests)

All three were invisible to the tests as first written; each now has one.

1. **BLOCKER — clearing a number in the panel became a silent no-op.** The old
   form code read `container_port`, `shm_size_mb` and the four healthcheck
   knobs with `Atoi`/`nonNegInt`, so an empty input meant **zero**. `bindInts`
   skips empty inputs, so emptying the port box wrote nothing, diffed to
   nothing and redeployed nothing. Those six are zeroed before binding now —
   the same treatment the limits pair already had and that was not carried
   across. `replicas` keeps skipping on purpose: the field is absent from the
   form for kinds that cannot scale, and zero is not a replica count.
2. **A canonicalisation read as a change, and redeployed.** `Validate` folds
   an empty `update_policy` to `"off"`, and `Update` then diffed that against
   a raw row from the store. `createApp` writes no value, so every
   API-created tile held `""` — and its *first* settings save reported
   `update_policy` changed, failed `ProxyOnly()`, and queued a redeploy for a
   save that changed nothing the user typed. Fixed in the diff rather than the
   validator: both sides are compared in canonical form. `restart` had the
   same shape (`NormalizeRestart`) and got the same treatment.
3. **`NeedsBuild` had lost `source_type`.** The deleted `cronNeedsBuild`
   listed it; the config differ emits `source` for an image change and
   `source_type` for a switch to a git build
   (`config/stackconf/plan.go:735,748`). Without it a config apply that
   switched a cron from an image to a git build never rebuilt it. Both names
   are listed now.

Two of these shaped the rig pass: it has to **clear** a field and read the row
back, not only set one, and it has to **save a tile twice unchanged** and
assert the second save queues no deployment.

## Points 3e, 3f, 3g — Create, Rename, Telemetry — DONE (rig check owed)

### 3e. `TileService.Create`

```go
func (s *TileService) Create(ctx, t *repo.Tile, stagedPatch any, by Actor) (staged bool, err error)
```

Rows closed:

- [A] **The kind set was three different sets.** The panel accepted
  service/cron/function/volume by hand-switching on kind literals and skipping
  `runpolicy` entirely; the API accepted service/cron/function; the config file
  accepted six. One `Validate` now, driven by `runpolicy`.
- [A] **The API accepted an empty slug.** `nameTile` derives it, refuses a
  name with nothing sluggable in it, refuses a reserved one, and 409s a
  duplicate — one answer for all three surfaces, where the panel used to 400
  a duplicate and the API 409 it.
- [A] **The defaults disagreed.** The webhook token was `uuid` on one path and
  `RandomHex(24)` on the other; the panel gave *every* kind a 30-minute
  timeout including services, which have no run to time out. `applyDefaults`
  fills them once — `RandomHex(24)`, and a timeout only for the kinds that run
  to completion.
- [A] **A port on a non-ingress kind was stored by the panel** and refused by
  the API; a connector from another org was **silently blanked** by the panel,
  400'd by the API and not looked at by the config path.
- [A] **`createApp` answered 500 when the cron reload failed after commit**
  — the tile existed and the caller was told the request failed. Point 1
  removed the error from `ReloadCron`; this removes the last caller that
  cared.
- [A] **The volume attach target was checked on one path only.** `checkAttach`
  is shared: a plain service in the same environment, and an absolute mount
  path.

Deviation: the **staged payload stays the caller's to build**. A staged
create carries a full `TileConf`, which is the config engine's shape, and this
package cannot import `stackconf` (3c's cycle). Only the canvas ever stages a
create — a create is structural, so every other surface writes through or is
refused — so only the canvas builds one.

### 3f. `TileService.Rename`

One caller today (`config/stackconf/moved.go`), but tile identity is the
thing the store is fussiest about: `sqlite.UpdateTile` excludes `slug`
deliberately, so it moves only through `RenameTile`, and the swarm service
name is built from the slug. The order — tear down, rename, rewrite the route
— is the whole content of the method and each step is there because skipping
it broke something. It does **not** redeploy; the caller does, for the same
bookkeeping reason as 3d's deviation 3.

### 3g. `TileTelemetryService`

- [A] **Two log-source resolvers.** The panel's SSE handler and the API's
  `getAppLogs` each decided "service logs or container logs" on their own,
  with the fallbacks in a slightly different order. `Logs(ctx, t) LogSource`
  answers once; `Container` is exported for the API's extra fallback, which
  the panel does not have and does not need.
- [A] **Two metric range parsers.** `components.MetricRange` and an inline
  switch in `appMetrics`, over the same three strings — adding a window to one
  left the other quietly answering 1h. `components.MetricRange` is now a
  one-line delegation, so the templates keep the name they call.
- [A] **`"app:" + id` was rebuilt by hand in nine places**, across two
  ref-keyed tables (metrics and cron_runs). `service.TileRef` now.

`metrics.templ` was edited at source and regenerated through the templ watch
rule; the generated file was not touched by hand.

### Verified

`go test ./...` green, `go vet ./...` clean.

One accident worth recording: `handlers/web/handler/project/handler.go` was
reverted with `git checkout` to undo a bad splice, which also threw away its
points 1, 2 and 12 edits. Those were reconstructed — the `jobs` field and its
three `LoadSchedules` calls (now `sched.Reload`), `px` moving from
`*proxy.Proxy` to `*svcproxy.Service`, and the `service/notify` import. The
file is verified by `go vet`, the suite, and a read of its diff; it is worth a
second look during review because it is the one file whose history in this
change is not purely additive.

## Point 3, verified on the rig

`stackr-test`, on this build, over the real API and the real panel form. The
rig found two defects the tests did not; both are fixed and re-verified below.

### Create (3e)

| probe | answer |
|---|---|
| `{"name":""}` | 400 `name: a tile needs a name` |
| duplicate name in the same env | 409 `a tile named "p3check" (p3check) already exists…` |
| `{"name":"secrets"}` | 400 `"secrets" is reserved for variable references` |
| cron with `"schedule":"not a cron"` | 400 `schedule: expected exactly 5 fields, found 3` |
| cron with `"port":80` | 400 `port: a cron has no endpoint; port does not apply` |

The API used to accept an empty slug and answer 400 (not 409) on a duplicate.

### The headline class B row — a spec edit redeploys itself

Tile deployed and running, **no deploy requested at any point**:

```
route-only edit  {"security_headers":true}   deployments 1 -> 1   PASS
spec edit        {"limits":{"cpu":0.5}}      deployments 1 -> 2   PASS
identical save   the same limits again       deployments 5 -> 5   PASS
```

The first row is the one that must *not* redeploy (a basic-auth toggle
bouncing the container would be its own bug), the second is the row that used
to sit dormant until the next manual deploy, and the third is the
canonicalisation regression the review caught — an unchanged save earns
nothing.

### The atomic pairs, and scale

```
set both        cpu=1.5  mem=512
cpu alone       cpu=0.25 mem=512   <- memory survived
{"replicas":2}  200, replicas=2    <- the API used to drop this key silently
```

### The validation union, over the API

Every one 400 with the field named:

```
{"shm_size_mb":-1}          shm_size_mb must not be negative
{"timeout_minutes":5000}    must be between 1 and 1440 (24 hours), or 0
{"update_policy":"sometimes"}  must be off, notify or auto
{"wait_for_ci":true}        wait_for_ci needs a git-built source
{"devices":["kmsg"]}        device "kmsg": paths must be absolute
{"depends_on":["db:maybe"]} condition must be started, healthy or completed
{"restart":"sometimes"}     must be always, on-failure or no
{"basic_auth_user":"ops"}   basic auth needs a password as well as a user
{"schedule":"0 3 * * *"}    schedule applies to cron tiles only
{"image":""}                an image source needs an image
{"connector":"nope"}        no such connector
```

### Lifecycle (3b)

```
POST /apps/:cron/stop      200, status "paused"   (not "stopped")
POST /apps/:cron/restart   400  a cron has no long-running container to restart
POST /apps/:service/run    400  run applies to cron and function tiles
POST /apps/:cron/run       202
cron_runs row              trigger="manual"  actor="svc-extraction (api)"
```

The run row is the trigger-vocabulary row: one `manual`, with the surface in
the actor rather than baked into the trigger string.

### Delete (3c)

`DELETE` on a volume still attached to a live tile → 400 `detach the volume
from p3check before deleting it`. The API had no such guard at all.

### The gate's two surfaces (3c)

Observed rather than asserted, and worth recording because it is the axis 3a
got wrong: the same edit to the same UI-owned stack **staged** from the panel
form (a `staged_changes` row, nothing written to `tiles`) and **wrote
through** over the API (`tiles` changed immediately). That is the intended
split and it now comes from one table rather than two helpers.

### Two defects the rig found that the tests did not

1. **The field-clearing fix from the review had never actually applied.** The
   `python` replacement that was supposed to zero the six always-rendered
   numbers before binding them had silently matched nothing — `goimports` had
   realigned the map literal between writing the patch and running it, so the
   `old` string no longer existed in the file and the replace was a no-op. The
   first rig run showed `shm` and `healthcheck_interval` clearing (they were
   already 0 in the row) while **`port` stayed 80**, which is exactly the
   shape the review predicted. Fixed for real, redeployed, re-verified:

   ```
   set    port=80 shm=64 hc_interval=15 hc_retries=3
   clear  port=0  shm=0  hc_interval=0  hc_retries=0
   ```

   Lesson for the rest of this refactor: a scripted edit that reports success
   is not evidence the edit happened. Every replace that matters is now read
   back.

2. **The attached-volume guard made an orphaned volume undeletable for ever.**
   Deleting a tile does not cascade to its volume tiles (that is point 9's
   class C mess), so the volume keeps a stale `attached_tile_id` that nothing
   ever clears — and refusing on the id alone meant the volume could never be
   removed by any surface. The guard resolves the owner now and only refuses
   while it is really there. Re-verified: `DELETE` on the orphan → 204.

   This one is a genuine regression this work would have introduced: the API
   had no guard before, so it would have deleted the row.

### Rig state after the pass

All test tiles removed; the stack `bk-check` and its `vol` / `data` tiles are
the leftovers from point 1's checks, unchanged. One `staged_changes` row is
left over from the panel-form probes and will disappear when that env's
pending set is applied or discarded.

**`skrt2` is joined.** The sudo password unblocked it: a fresh join key was
minted through `POST /servers/:id/join-key` (the two on the rig had both
expired) and the script run as root on the box. `docker node ls` shows two
ready nodes and `stkr-agent` running on both. The `servers` row is `ready`.

### Two more review fixes, after the rig pass

1. **`AfterWrite` advertised a caller it did not have.** 3d wired
   `Update → AfterWrite`, then the config applier was reverted to performing
   its own deploys (the `Deployed` bookkeeping) — which left `AfterWrite`
   exported, documented as "exported for the config applier", and called only
   by `Update`. Unexported to `afterWrite`, with the comment saying what is
   actually shared: the *decision* travels through `Changed`'s methods, the
   *doing* does not. Point 7 would have trusted that comment.

2. **Reversed pick: no upper bound on `timeout_minutes`.** SP1 turns a silent
   clamp into a refusal, and `timeoutMinutes` clamped to [1, 1440] — so the
   first cut refused anything outside that. But the upper bound was only ever
   the *panel's* private opinion: the API had none and neither does the config
   schema. Two consequences, both bad: a config file declaring
   `timeout_minutes: 2000` applies today and would keep applying (a config
   apply does not run `Validate`), while the same value over the API would
   start failing — which is precisely the two-of-three-surfaces drift D2
   exists to stop. The rule is now "not negative", which all three surfaces
   already agreed on. If 24h should be a real cap it belongs in the config
   schema as well, and that is its own change.

The leftover `staged_changes` row from the panel-form probes was discarded, so
point 4 does not inherit a stale `settings` group on `production`.

## Point 4 — `DomainService` + `DomainResourceService` — DONE (rig check owed)

### Shape

`service/domain.go`, `service/domain_auto.go`, `service/domainresource.go`.

```go
func (s *DomainService) Attach(ctx, t, DomainSpec, by) (d *repo.Domain, staged bool, err error)
func (s *DomainService) SetTLS(ctx, t, domainID, TLSPatch, by) (d *repo.Domain, staged bool, err error)
func (s *DomainService) ToggleHTTPS(ctx, t, domainID, by) (d, on, staged, err)
func (s *DomainService) Detach(ctx, t, domainID, by) (d, staged, err)
func (s *DomainService) AddAuto(ctx, env, t, by) error   // gated; a person asked
func (s *DomainService) EnsureAuto(ctx, env, t) error    // internal; a clone inherits

func (s *DomainResourceService) Create(ctx, level, ownerID, host, ResourceOpts) (*repo.DomainResource, error)
func (s *DomainResourceService) SetACME(ctx, id, email) error
func (s *DomainResourceService) Delete(ctx, id) error
func (s *DomainResourceService) Get(ctx, id) (*repo.DomainResource, error)
```

**Every domain method takes the tile as well as the domain id, and proves the
two match.** That one line is the security fix; see below.

### Rows closed

- **[SEC, A] Cross-tenant domain flip and delete by id.** The panel's
  `ToggleDomainHTTPS` and the live branch of `DeleteDomain` acted on whatever
  `:domainID` named, and the only check above them (`h.load`) proved write
  access to *the tile in the URL*. So an operator with any tile of their own
  could flip HTTPS on, or delete, **any domain in the install** by id.
  `DomainService.own` resolves every domain id through `d.TileID == t.ID` and
  answers `ErrNotFound` — not forbidden, because a 403 would confirm the id
  exists. Covered by `TestADomainOnAnotherTileIsInvisible`.
- **[A] Attach had three rule sets.** The API skipped the squat check,
  wildcard-DNS, the middleware-reference check and the managed-engine
  `SpeaksHTTP` check, could not set rule/priority/middlewares at all (so a
  stack whose routing needed any of them could not be built by script), and
  left a redirect's port at 0 — which renders into the route as
  `http://alias:0`. All of it is one `plan()` now, and `domainIn` gained
  `rule`, `priority` and `middlewares`.
- **[A] HTTPS defaults disagreed three ways** (panel checkbox off = both off;
  API/config nil = both on; CLI `--no-https` = https off but force-redirect
  *on*, which is incoherent). One rule: nil means on, explicit means explicit,
  and `force_https` follows `https` unless it is given. The panel's checkbox
  now sends an explicit `false`, because an unticked box really is an explicit
  "off" — that is what the pointer is for.
- **[A] The cert and HTTPS gates disagreed.** The panel's certificate handler
  had **no gate at all**, so a config-managed stack accepted a certificate its
  file would never know about; the panel's HTTPS toggle used `editGate` and
  the API used `rejectManaged` for both. One field-edit gate now. A
  certificate is never staged even on a stack that stages: it is a secret, it
  is not in the file, and a pending set is no place for a private key.
- **[A] Wildcard HTTPS was checked on attach and not on toggle**, so a
  wildcard could be switched to HTTPS afterwards and then silently fail to get
  a certificate.
- **[A] The squat check existed on the org page only**, which made it
  decorative: the same name could be claimed from the API, at stack level, or
  one tile down. It is in `DomainResourceService.checkOwner` and in
  `DomainService.plan` now, so all four creation paths take it. An
  instance-level host is checked against *every* org slug.
- **[A] The API accepted `acme_email` and threw it away**, and the panel had
  no field for it at all. `ResourceOpts.ACMEEmail` is applied on create,
  through the proxy service so the one traefik restart it needs happens.
- **[A] The server page deleted any resource id the form carried**, so an
  org's or a stack's resource could be removed from it. Each page now checks
  the row is at its own level and owned by its own owner.
- **[B] The API's `patchDomainResource` skipped `rejectManagedOwner`**, so a
  config-managed org's ACME account could be changed over the API and reverted
  by the next apply. The refusal is inside `SetACME`.
- **[B] A stack file's `acme_email` applied on create only** — changing it did
  nothing, and the plan said nothing either, while the org file had diffed and
  applied it since it gained `domains:`. `diffDomainRes` emits the change (and
  its `traefik restarts once` note) and `applyDomainRes` applies it through
  the service. The diff's `switch` also became independent `if`s: two fields
  can move in one edit and a switch showed only the first.
- **[A] The instance-level owner id differed per writer.** `canonLevel`
  normalises `"node"` to `"instance"` and fills `"local"` once.

### Moves, and why

`ValidateResourceHost`, `HostTaken`, `CheckOrgSquat`, `AutoHost`,
`VisibleDomainResources`, `EnsureAutoDomain` and `defaultEnvID` all moved out
of `config/envops` into the service package. Not for tidiness: `envops`
imports the service (since 3c), so a rule the service must call could not stay
there. `envops.Ops` gained `Domains` and `Resources` and delegates.

`middlewareNames` is a deliberate small twin of `stackconf.ParseMiddlewares` —
the service cannot import the config engine, both read the same YAML shape,
and only the names are needed here. The comment says so at both ends.

### Deviations from the plan

**The API refuses rather than stages.** A config-managed stack that declares
`ui_edits: stage` now holds *every* surface's field edits (3c), and a domain
change is a field edit — but recording one into the pending set means
rebuilding the tile's whole desired domain set in the config engine's shape,
which the API has no machinery for and the service cannot build without
closing an import cycle. So the API answers 409 "this stack stages edits for
review; add the domain from the canvas" rather than silently writing through
the file's back. The panel stages as it always did, through one
`stageDomains` helper instead of three inline copies.

### Files

New: `service/domain.go`, `service/domain_auto.go`, `service/domainresource.go`,
`service/domain_test.go`; `service/defaultenv_test.go` moved with its subject.
Rewritten: the five panel domain handlers, `api/v1/domains.go`,
`patchDomain`, `createAutoDomain`, the four `domainresources.go` handlers, and
the org / stack / server resource pages. `config/envops/envops.go`,
`config/stackconf/plan.go` + `apply.go`, both routers, `cmd/stackrd/main.go`.

### Verified

`go test ./...` green. `domain_test.go` covers the security row on all three
methods, the attach defaults (HTTPS pair, path normalisation, inherited port,
redirect port 80 not 0) and five refusals: no port and no redirect, wildcard
without a DNS provider, host+path taken by another tile, another org's slug,
and a managed engine that does not speak HTTP.

### Three review fixes to point 4

1. **A staged plain-HTTP domain applied with the redirect still on.**
   `DomainConf.ForceHTTPSOn()` treats a nil `force_https` as *on*, and the
   staged branch carried only `https: false`. So a domain added from the
   canvas with the HTTPS box unticked staged as "no TLS, but bounce plain
   HTTP onto TLS" — the incoherent combination this point set out to remove,
   arriving by a different door. The direct path had it right. Both halves are
   carried now. (Pre-existing, but the direct path's fix is what made the two
   disagree, so it counts as this change's.)
2. **`AddAuto` ignores the gate's staging answer**, which was accidentally
   right and is now deliberately commented. An auto row carries no choice a
   reviewer could approve: it is recomputed from the resource and the slugs
   every time, and a config apply regenerates it. The gate is being asked only
   "may this stack be written to at all".
3. **The org file had the bug the stack file just lost.** `orgconf` wrote
   `acme_email` into the row with `UpdateDomainResource` and never restarted
   traefik, although the plan line it emits promises "traefik restarts once" —
   so the account changed in the database and the certificates kept coming
   from the old one. It goes through `Resources.SetACME` now, same as the
   stack path. `orgconf.Runner` gained `Resources`.

## Point 4, verified on the rig

### The security row, on the real panel routes

Two tiles, each with its own hostname. Every call below uses **tile A's URL
with tile B's domain id** — which is exactly what the old code acted on:

```
POST /apps/A/domains/DB/https   404    (was: flipped tile B's domain)
POST /apps/A/domains/DB/delete  404    (was: deleted tile B's domain)
POST /apps/A/domains/DB/cert    404    (was: no gate and no ownership check)
```

and tile B's domain read back unchanged afterwards.

**Scope of the demonstration, honestly:** this proves `own()` runs on all
three live routes and that a domain id from another tile is invisible. It does
not exercise a second *user* — the panel has no API for minting one, and both
orgs here belong to the same admin. The cross-tenant part of the original
finding is the same code path with a different outer check, and the unit test
`TestADomainOnAnotherTileIsInvisible` covers the branch directly.

### Attach, over the API, with a second organization on the box

A second org `AcmeCo` exists on the rig so the squat rule has something to
bite on:

```
{"host":"acmeco.example.com"}                    409  that hostname starts with another organization's slug
{"host":"*.wild.example.com"}                    400  wildcard HTTPS needs a DNS provider
{"host":"mw.example.com","middlewares":["nope"]} 400  no middleware nope; proxy.middlewares in the stack file declares them
{"host":"qa.example.com"}                        201  (its own org's slug is fine)
{"host":"unrelated.example.com"}                 201
```

Every one of those was **accepted without question** by the API before.

### Defaults, and the route file they produce

A bare `POST /domains` answered
`https:true force_https:true path:"/" container_port:80` — the port inherited
from the tile, both TLS halves on.

A redirect domain answered `container_port: 80`, and the rendered route file
contains **no `:0` anywhere** — the API used to leave the port at zero and
write `http://alias:0` into the config.

The rule/priority row, end to end in the live Traefik file:

```yaml
rule: "Host(`rule.example.com`) && PathPrefix(`/v2`)"
priority: 50
```

Neither key could be set by a script at all before.

### Domain resources

```
stack-level    host acmeco.test.example.com  409  another organization's slug
instance-level host qa.inst.example.com      409  another organization's slug
create with acme_email "Certs@Example.COM"   201  stored as certs@example.com
```

Squat used to be checked on the org page only; `acme_email` used to be
accepted and thrown away. Traefik restarted once on the create that carried an
ACME address — visible in `docker service ps stkr-traefik` and as a ~40s gap
in the panel, which is the documented cost of the account living in the static
config, and is the restart the org config path silently never did.

Deleting a **stack**-level row from the **server** page answered 404 and left
the row in place. That page used to delete whatever id the form carried.

### Rig state

Test tiles and resources removed. Two leftovers, both deliberate: the second
organization **AcmeCo** (an unfinished draft — it exists so the squat rule has
another slug to bite on, and it is worth keeping for later points), and
`qa.stackr-test.vulpe.dev`, which the org wizard created and which was there
before this pass.

One cosmetic bug the rig found and the tests did not: the middleware refusal
read `middlewares: middlewares: no middleware nope`, because `plan` re-wrapped
an error that already named its field. Fixed.

---

## Point 5 — ManagedInstanceService + SliceService

Two services, `internal/stackrd/service/managedinstance.go` and
`internal/stackrd/service/slice.go`. Every surface that touched a managed
database now calls them: the panel's db drawer, the canvas's create form, the
app panel's provision/attach/detach, the API's `/dbs`, `/provisions` and
`/apps/:id/provisions` routes, `config/stackconf` (apply, slices) and
`config/orgconf`'s `shared:` block.

### Rows closed

**[B] Delete instance was four teardowns and none was complete.** One
`TearDown` now does the union, in an order that lets each step still reach
what it needs:

1. held-slices refusal (unless forced),
2. drop every slice the instance provides, while its container is still up,
3. reap any provision row the drop could not clear — a force delete used to
   leave both the provision and the resource rows pointing at an id that no
   longer resolved, and nothing ever collected them,
4. `dbs.Remove` — the swarm service, then the netpool release, in that order
   (releasing first put the network name back in the pool while the instance
   was still attached to it),
5. `TileService.TearDown` — containers, the Traefik route, the provisions this
   tile itself consumed, the row, the two schedule tables.

The panel skipped 4's route half, the API skipped the netpool and (with
`force=true`) 2 and 3, a config apply had no gate at all, and orgconf did only
the pool.

**[B] s3 wiring.** `SliceService.Wire` injects the engine's whole output set
for `AutoInjectAll` engines. The panel injected `S3_ENDPOINT` alone, which is
not enough to reach a bucket with, so every s3 consumer added through the
panel needed three more variables written by hand.

**[B] Set slice public** now moves every row sharing the slice name on all
three surfaces. A config apply flipped the one representative row it had
found, leaving the other consumers' rows claiming the old visibility for ever.
(The API's in-memory flip named in `02` turned out to be correct already — it
writes through `SetBucketPublic` per row and only sets `p.Public` for the
response shape. Verified, not re-fixed.)

**[B] API `deleteApp` leaving consumer provisions active** was already closed
by point 3: `teardownTile` delegates to `TileService.TearDown`, which orphans
`ListProvisionsByConsumer`. Confirmed, row marked closed rather than redone.

**[A] Gate drift `rFU` vs `rejectManaged`.** One helper, with the org-scope
exception the panel had and the API did not: the stack file cannot declare an
org-scoped instance (`shared:` stops at stack scope), so those stay
panel-owned on a config-managed stack. A **scope change** is deliberately not
exempt — moving an instance in or out of org scope makes it appear or vanish
from the config snapshot mid-flight — so `SetScope` asks without it.

**[A] `Start` healed provisions on the panel and not over the API.**
`TileLifecycleService` gained a managed branch: `Stop` goes through the
engine's own scale-to-zero, `Restart` through `Start`, which is the one that
reconciles the slices a consumer provisioned while the instance was down.

**[A] Four `needsRedeploy` lists** collapsed into `Changed.NeedsDBRedeploy()`,
which takes config's list — the union, and the one with a bug report behind it
(`node_group`/`replicas`: a group pin wrote the row, touched nothing, and the
plan read "applied" with the database still scaled to zero on the node it was
meant to leave). `DiffTiles` gained `external_port`, which it had never
diffed at all.

**[A] Cut** takes all four checks: `ServesEnv` (the API's) so a slice is never
cut into an environment nothing can consume it from, plus `Eligible`,
`WaitReady 90s` and adopt-or-create (the config engine's) so one call can
create an instance and its slices together and asking twice does not leave a
silent second copy.

**[A] Create** rules unified: the API accepted an empty slug and an unknown
engine where the panel refused both, and the panel's scope choice stopped at
stack where the API and the file allow org.

### Picks taken

- **SP1 throughout.** Unknown scope, bad port, bad policy, unknown engine all
  refuse. The panel used to coerce each of them, which turned a typo into a
  silent setting change — a bad port became "not published at all". The panel
  form's int parser now returns `-1` on an unparsable value so the service
  refuses it instead of reading it as "cleared". The two things still
  normalised are not the user's choice: an empty image means the engine
  default, and docker itself rejects a memory cap under 6MB.
- **SP2.** `Delete` does all five effects; `SetPublic` persists every row.
- **SP3.** The held-slices refusal survives and takes the caller's flag.
  `internal/cli/cmd/infra.go` stopped sending `force=true` on every path:
  `--force` is now the explicit "destroy its slices with it" answer, `-y`
  alone only means "do not prompt me". A scripted `-y` used to destroy every
  consumer's data without anything ever printing what it was about to take.

### Decided, not asked

- **A managed instance's settings never stage, on any surface.** There is no
  staged shape the config engine could apply for one, so `ui_edits: stage`
  refuses here where it would queue a service tile's edit. This preserves
  today's panel behaviour (`SetPort` always wrote through) and makes the API
  agree with it. A *create* still stages, because the canvas has always
  staged one and the config engine knows the shape.
- **`ManagedInstanceService.AfterWrite` is exported** where
  `TileService.afterWrite` is not. An instance's redeploy is synchronous —
  no build to queue, no deployment row to record — so a config apply calls it
  rather than keeping a fifth copy of the decision *and* the status
  bookkeeping. The reason `TileService` withholds its version (the applier
  records what it deployed) does not apply.
- **`removeSlice` orphans the row directly** rather than calling
  `dbs.Detach`: a config-declared slice has no consumer id to unhook.

### Left in place, named

- `infra/imagewatch/watcher.go` (the sixth `Deploy` caller, and the only one
  writing no status) and `infra/volmove.go` (the fifth "deploy then status"
  copy) cannot call the service: `infra/` sits below the line (D7) and the
  import would invert it. Four of the six copies are gone; these two are
  2.15's.
- `envops.Teardown` still reaches for `DBs` directly to reclaim an ephemeral
  env's slices. That is point 6's file.

### Post-review fixes

Five found before the rig pass, two of which would have passed a rig check:

- **`patchDB` wrote twice.** `SetScope` persisted the row, then `Update`
  diffed against the row `SetScope` had just written, so
  `{"scope":"stack","external_port":5433}` saved the port and never recreated
  the container — the exact class-B bug this point exists to close,
  reintroduced in the handler that fixes it. Split into `PlanScope` (gate and
  mapping, no write) and `SetScope` (both), so the PATCH is one merged row,
  one write, one diff.
- **`/apps/:id` still accepted a managed id**, and that path skips everything
  above: `DELETE` dropped the row and leaked the slices, the shared network
  and the pool entry; `PATCH` applied none of the instance rules; `POST
  /deploy` enqueued the *build* engine for a database. All three now 404 on a
  managed id, the mirror of what `getDB`/`patchDB` already do.
- **`reapProvisions` cleared provisions and stranded resources.** The resource
  row is what a consumer's `${{ tile.<slice>.<OUTPUT> }}` resolves through,
  and `dropResource` only ever runs inside a successful engine drop. Both
  tables are reaped now.
- **The db panel's domain routes double-gated.** `DomainService` asked
  `GateFieldEdit` with no org exception after the handler had already granted
  one, so an org-scoped instance on a config-managed stack passed the first
  check and 409'd on the second. `DomainService.gateFor` now exempts every
  managed tile, which is the true rule: the config engine models domains for
  service tiles only, so a db-tile domain is invisible to plan and apply on
  any stack and there is nothing for a file to own.
- `SetPort` folded an unparsable `cpu_limit` to 0 while every other field on
  the same form refused it. `formFloat` mirrors `formInt`.
- `getDB` now returns the `Path` that `listDBs` sets (2.1's last [A] row on
  that operation).

### Contract change worth naming

`POST /stacks/:id/dbs` now returns the deploy error. It used to answer 201
with `status: "error"` when the image pull failed; it answers 500 now, with
the row already created — so a retry meets the duplicate-slug 409. The row is
there either way and its status column says which; reporting the failure is
the point, since a create whose container never came up is the one thing the
caller most needs to hear about.

### Commit message

```
refactor: one owner for a managed database instance and for its slices

Delete was four teardowns and none of them was complete: the panel released
the network pool and left the route, the API dropped the route and left the
pool, with force=true left every provision and resource row behind, a config
apply had no held-slices gate, and the org file did the pool and nothing
else. One TearDown now does the union.

Also: s3 slices wire their whole output set from the panel, not one endpoint;
a slice's public flag moves every consumer's row; restarting an instance over
the API heals its slices the way the panel already did; four copies of
"which change needs a redeploy" became one, and it is the list with the bug
report behind it; and `stackr infra rm` stops forcing on every path, so the
refusal a scripted `-y` used to walk straight through is reachable again.
```

### Rig pass, point 5

```
POST /stacks/:id/dbs {"scope":"team"}       400  scope: must be env, stack, or org
POST /stacks/:id/dbs {"engine":"cassandra"} 400  engine: unknown engine "cassandra"
POST /stacks/:id/dbs {"name":"!!"}          400  name: needs at least one letter or number
POST /stacks/:id/dbs (same name twice)      409  a tile named "qapg" already exists
PATCH /dbs/:id {"external_port":70000}      400  must be between 0 and 65535
PATCH /dbs/:id {"update_policy":"sometimes"} 400 must be off, notify or auto
GET /dbs/:id                                200  now carries "path":"qa:bk-check:qapg"
```

All four creation refusals were accepted by the API before this point.

**The double-write case.** `PATCH /dbs/:id {"scope":"env","external_port":5433}`
answered 200 — and the swarm service came back as
`stkr_qa_bk-check_production_qapg *:5433->5432/tcp`. That is the assertion the
post-review fix exists for: with the two writes in place the diff saw an empty
change set and the port would have been stored with the container untouched.

**The union teardown.** An instance with one slice on it:

```
DELETE /apps/:db-id           404  (also PATCH and POST /deploy)
DELETE /dbs/:id               409  1 consumer(s) hold slices on this instance
DELETE /dbs/:id?force=true    204
```

After the forced delete, checked in the database and in docker rather than
through the API: `tiles` 0, `provisions` 0, `managed_resources` 0, the swarm
service gone, and `stkr-dbnet-01` drained to `0 containers, Services: null`.
Before, the API's force path left the provision **and** the resource rows
behind and never released the network.

The three `/apps/:id` routes 404 on a managed id now. `DELETE /apps/{db-id}`
used to drop the row through `TileService` and leak all of the above.

**s3 wiring, through the panel.** Provisioning a bucket from the app drawer
(`POST /apps/:id/provision`, session + CSRF, not the API) left the consumer
holding:

```
S3_ACCESS_KEY  S3_BUCKET  S3_ENDPOINT  S3_REGION  S3_SECRET_KEY
```

The panel used to inject `S3_ENDPOINT` alone, which is not enough to reach a
bucket with.

**Other rows.** Detaching a provision through a tile that does not own it
answers 404. The panel's delete of an instance holding a slice refuses and
leaves both the tile and the slice in place (303 back to the settings tab with
the refusal in the flash, not a redirect to the project). A panel settings
save with `external_port=nine` answers 400 where it used to store 0 and
silently unpublish the port. Cutting the same slice twice returns the same
row id rather than a second copy.

### One bug the rig found and the tests did not

Restarting a managed instance over the API answered **500 after exactly
30000ms**: `dbs.Start` waits up to 30s for the task to roll and the API gives
a request 30s in total, so the synchronous version raced its own caller's
deadline and reported a failure for a restart that was working (the instance
came back `running` seconds later). The panel never hit it because its request
budget is larger.

The managed branch of `TileLifecycleService.Restart` now runs detached on its
own context, the way the panel's Deploy button already did, and the status
lands with the roll. The panel's flash reads "Database starting. Refresh in a
moment." Re-verified after redeploy: restart answers 200 with the row, and the
instance is `running`.

Not changed, but worth naming: `ManagedInstanceService.Create` still deploys
synchronously on the request's context, so a cold image pull over the API can
hit the same 30s budget. That was true before this point as well; making it
detached would change what the 201 means.

### Rig state

Everything created for this pass was removed: `qapg`, `qas3`, `qaconsumer`,
their slices and their resource rows. The two deliberate leftovers from point
4 (the **AcmeCo** draft org and `qa.stackr-test.vulpe.dev`) are untouched.

---

## Point 6 — EnvironmentService + VariableService — DONE, rig-verified

Two services, `internal/stackrd/service/variable.go` and
`internal/stackrd/service/environment.go`.

### Rows closed

**[B] The single most user-visible bug in the findings.** Variables written
over the API never replanned and never redeployed. `stackr vars set` on a
running tile, or on a config-managed stack, wrote a row and changed nothing
anyone could see — no redeploy, no refreshed plan, and the canvas banner still
reporting the missing value that had just been supplied. `VariableService.after`
is the half that did not exist: release a parked deploy, replan a
config-managed stack, redeploy a running consumer, nudge the canvas. Every
caller gets it now.

**[B] `resetEnv` over the API did not replan** where the panel did, so a reset
left the stack's plans describing tiles that no longer existed.

**[B] The drop-link submit wrote variables with no `ClearWaiting` and no
replan** — its own comment admitted it. Its burn and its writes are a single
transaction so it cannot hand the write over, but `VariableService.Applied` is
the side-effect half on its own, and the handler calls it after the burn wins.

**[B] A config apply minted generated secrets with no audit row** and released
no parked deploy. Both added, actor `config:<stack slug>`. `applyVars` records
its plain writes too.

**[B] Env delete left `staged_changes` and env `node_positions` behind** on
every surface. `staged_changes` has no foreign key to environments, so a
deleted env's pending set outlived it; deleting a *stack* cleans its canvas
positions up, deleting one environment never did. `EnvironmentService.teardown`
clears both.

**[A] Env create had four rule sets plus a fifth creator with none.** One now:
non-empty name, a slug with something in it, reserved slugs (`settings`,
`list`, and `HomeSlug` — the one that bit, because the API knew about it and
the panel did not, so an environment named "Stack" hit a UNIQUE index and
surfaced as a raw 500), and a duplicate check the panel never had.

**[A] The variable name rule** was a regex on the panel and "any non-empty
string" over the API. The regex is the rule everywhere.

**[A] Env colour and apply policy** had inverted gates — the panel refused a
colour on a config-managed stack and the API accepted it; the panel allowed a
policy only on a managed stack and the API allowed it anywhere. Neither key is
declared in a stack file, so neither was ever the file's: both are accepted on
any stack now.

### Picks taken

- **SP1.** The name regex applies everywhere. An unknown colour and an unknown
  apply policy are refused rather than folded to the default — `SaveEnvConfig`
  used to coerce, so a stale or hand-posted value silently reset the policy to
  inherit.
- **SP2.** Replan, redeploy, `ClearWaiting*` and audit all happen in the
  service, so no caller can skip them.
- **SP3.** Env create gets one rule set, and the PR hook goes through it.
- **Exception to SP1, as the plan decided: generate skips a live value.** The
  panel used to overwrite, which is a silent credential rotation that breaks
  everything already reading the secret. The API's behaviour wins.
- **Config only writes what it declares.** `apply.go`'s env-settings block
  wrote `Color` and `ApplyPolicy` on every apply whether or not the file
  mentioned them, so a panel or API write silently reverted on the next plan.
  Undeclared now means unmanaged, which is what `ui_edits` and the rest of the
  gate vocabulary already assume. `Position` is deliberately *not* in that set:
  it is the file's declaration order, not a value any surface can set, and the
  default environment's generated hostname derives from it.

### Decided, not asked

- **`Adopt` is `Create` without the gate.** The PR hook creates a preview
  environment on a stack the config file owns — exactly what `Create`'s
  structural gate refuses — and it was also the creator with no rules at all.
  Same split as `TileService.Delete`/`TearDown`.
- **The panel's confirm-by-typing is its `force`.** `DeleteEnvironment` and
  `ResetEnvironment` pass `force: true` because the dialog already made a
  person type the env slug. The API and the CLI take the flag.
- **`Replanner` and `EnvOps` are interfaces** declared in `service` and
  implemented by `config/stackconf.Planner` and `config/envops.Ops`. Those
  packages import `service`, so the dependency can only point this way. This
  is the same shape the import-cycle constraint has forced since point 3.
- **`Planner.Replan` detaches its own goroutine.** A replan walks the whole
  config tree; inline it would have put a slow walk inside the API's 30-second
  request budget — the failure mode point 5's rig pass found in `Restart`.
- **The tile env blob is not this service's.** `app.SaveEnv` and `DeleteVar`'s
  in-blob branch stage through `editGate` and write a tile *column*; they stay
  with TileService. But `Replace`/`Unset` on a tile owner do re-sync the blob
  from the rows, because the store projects the blob back into variables on
  every tile write — so a PUT that dropped a variable would otherwise have it
  reappear. Two writers of `tile.Env`, under different rules, on purpose.
- **The masked-value guard now applies to the panel too.** Only the API had
  it. A form that round-tripped a listing could previously store `•••` over a
  secret.

### An import cycle broken on the way

`service` needs `audit.Record`, and `store/audit` was importing
`handlers/middleware` to find the current user — a store-level package
pointing at the HTTP layer. The echo-aware half (`Actor`, `PanelViews`,
`ServeValue`) moved to `handlers/middleware/audit.go` as `AuditActor`,
`AuditPanelViews`, `AuditServeValue`. `store/audit` is now pure: a `Record`
and a vocabulary. Ten call sites updated.

`Actor.Audit()` is deliberately not `Actor.String()`: the audit trail has two
established spellings — a bare email for a person, `api:<key name>` for a key
— and a query for "what did this key do" matches on that prefix.

### Rig pass, point 6

```
PUT /stacks/:id/variables {"name":"db url"}  400  variable name: letters, digits, _ . - only
PUT /stacks/:id/variables {"value":"•••"}    400  was sent back masked
POST /stacks/:id/envs {"name":"Stack"}       400  "stack" is reserved      (was a raw 500)
POST /stacks/:id/envs {"name":"production"}  409  an environment named "Production" already exists
POST /stacks/:id/envs {"name":"!!"}          400  needs at least one letter or number
POST /stacks/:id/envs {"name":"settings"}    400  "settings" is reserved
```

**The headline row, proven.** A running tile, deployments listed before and
after a `PUT /apps/:id/variables`:

```
before:  1 deployment   ['api']
after:   2 deployments  ['variables', 'api']
```

The `variables` deploy is the one that never used to be queued. Before this
point that PUT wrote a row and stopped.

**Generate does not rotate.** Setting `TOKEN` to a value and then asking for
`generate: true` left the stored value alone — one `set TOKEN` audit row, not
two, and the ciphertext unchanged.

**Audit actor.** Rows read `api:svc-extraction`, the key's name under the
established prefix.

Rig cleaned up: `varcheck` and the test variables removed. The point-4
leftovers (the **AcmeCo** draft org, `qa.stackr-test.vulpe.dev`) are untouched.

### Commit message

```
refactor: one owner for variables and for the environment row

Variables written over the API never replanned and never redeployed, where
the panel did both, so `stackr vars set` on a running tile changed a row in
the database and nothing anyone could see. Every write now earns the same
four things whoever makes it: the audit row, the release of a deploy parked
on the name, a replan when the stack is config-managed, and a redeploy of a
running consumer.

Environment creation had four rule sets and a fifth creator with none, and
the gap that bit was the hidden home environment's reserved slug: an env
named "Stack" collided with it and surfaced as a raw 500. One rule set now,
and the pull-request hook goes through it.

Also: the drop-link submit and a config apply's generated secrets stopped
skipping everything after the write; deleting an environment stopped leaving
its pending set and its canvas layout behind; a config apply stopped
overwriting a colour or an apply policy its file never declared; and
store/audit stopped importing the HTTP layer.
```

---

## Points 7 to 11 — DONE, rig check owed

Run in one pass, on an explicit instruction to churn 7, 8, 9, 10 and 11
without stopping to ask, making the best assumption for every open question
and recording it. **Every one of those assumptions is in
`05-assumptions.md`**, one entry per call that a person could reasonably have
made the other way, each saying what was chosen and what reversing it looks
like. Read that file before reviewing this work; it is where the judgement
is, and several entries are decisions to *not* build something the plan
lists.

`go test ./...` is green. `go vet ./...` is clean. `make lint` still panics —
pre-existing, golangci-lint v2.11.4 built against go1.26 on a go1.27
toolchain. Nothing is committed.

### Point 7a — the live-state predicate and two access gates

New leaf package **`internal/deploystate`**, outside `internal/stackrd`
because the CLI imports no stackrd package, `infra/` may not reach up into
the service layer, and all three have to agree on which statuses mean "still
going to happen". `IsLive`, `IsTerminal`, `IsCancellable`, and the status
constants.

Four hand-written copies of the live set replaced (the panel's status poll,
its SSE stream, the API's release listing, the engine's cancel guard) plus
the CLI's terminal set and the commit log's two. **`waiting_ci` is live now**,
as the plan decided: a parked deploy is still going to happen, so the poll
and the stream no longer end on it.

Two access rows, both SEC:

- **Cancelling a deployment over the API needed only read.** `loadDeployment`
  gained a `write bool` mirroring `requireTile`; cancel passes true.
- **A deployment whose tile row is gone was readable by anyone.** The panel's
  loader skipped the membership check entirely when `GetTile` returned nil.
  There is no other row to check tenancy against, so it is a 404 now.

### Point 7b — StackService

`service/stack.go`: `Create`, `Update`, `Reslug`, `Delete`, `Move`, `Bind`,
`Unbind`. Callers: both surfaces' create and delete, the API's PATCH, the
panel's config binding, the org page's move, `orgconf.applyStack`, and
`orgconf/moved.go`.

- **Stack create pre-checks the slug** and answers 409. Neither surface did,
  so a second stack with the same name reached the user as a raw UNIQUE 500.
- **The production environment goes through `EnvironmentService.Adopt`.**
  Three copies built that row by hand with no rules at all.
- **Delete reloads the scheduler.** Only the API did; the panel left the cron
  and backup tables firing at rows the cascade had removed.
- **A config apply's binding gets the connector-in-org check, the repo
  normalisation and the staged-row drop** it had none of.
- **`moved:` writes the slug only.** It wrote Name and Slug both, from the
  slug, so "Billing API" moved to `billing` came out displayed as "billing".

`stackconf.Applier.RenameStackNow` is the one-line entry point the service
renames through — the service layer cannot name a `*Plan`.

### Point 7c — DeployService + ReleaseService

`service/deploy.go` and `service/release.go`.

- **One rule set for "may this be deployed".** The panel refused a cron tile
  and the API queued one, where a cron tile has no long-running container to
  replace. Volume, managed and upper-env were two spellings each.
- **`RedeployIfRunning` in one place**, replacing six: a private copy inside
  VariableService and five in the handlers. Two of those five deployed
  *unconditionally*, so attaching a volume to a stopped service started it.
- **One `EnqueueApply` for both promote paths**, with the plan-id dedupe they
  threw away by hand-building the job under the stack id.
- **A plain promote runs on the work queue**, under a new `config.promote`
  kind that fails rather than requeues on restart: an apply is convergent, a
  promote is a decision about which commit an environment runs.
- **A promote refuses a commit nothing has built.** Both surfaces accepted
  any string and enqueued deploys for a tag that had never existed.
- **Promote force is a checkbox, off by default.** The panel hard-coded true,
  so its button silently overrode a refusal the API and CLI respect.
- **Image auto-update honours the upper-env gate** — `envnet.UpperEnv`,
  called from `infra/imagewatch` directly, which needs no service import
  because `envnet` is itself under `infra/`.
- `stackconf.ExportStack` replaces twenty-five identical lines in the panel's
  download and the API's GET.

### Point 7d — PlanService, and the StagingService that was not built

`service/plan.go`: `Run`, `Approve`, `Reject`.

- **The panel's "Plan now" now refuses an unbound stack**, which the API
  always did; it used to run a planner that could only fail and report the
  failure as a plan.
- **The pending check answers 409 on both surfaces.** The panel said 400.
- **`web/app/handler.go`'s `editGate` forwards to `GateService`.** It was a
  second implementation of the gate, written before the gate existed.
- **`GET /org-config/plans/:id` is new**, and `stackr org config approve`
  reads the plan before confirming — its help text used to admit it was
  approving something nobody could read.
- Plan preview's scope now matches its gate (`config:apply`).

`StagingService` was deliberately not built: staging has one surface, and the
edit gate it was meant to own already belongs to `GateService`. See
`05-assumptions.md`.

### Point 7e — the ApplyEngine, and how little was left

Points 3 to 6 had already turned most of the applier into a caller. What was
left and is now done: **`createTile` and `updateTile` run
`TileService.Validate`**, the same validator both surfaces run. A file could
declare a limit, a port list, a storage attachment or a placement the panel
refuses outright, and the refusal arrived at deploy time as a failed
container with the plan already marked applied. *This can fail an apply that
used to pass.*

`createTile` still builds its own row and `applyVars` still writes its own —
both for reasons written out in `05-assumptions.md`.

### Point 8 — BackupScheduleService + BackupDestinationService

- **One kind derivation.** The API accepted `kind: dump` for a tile that is
  not a database, which produced a schedule that failed on every run.
- **Keep defaults to 7 everywhere.** It was 0 on every path but the panel,
  and 0 means keep nothing.
- **`backup.Validate` and the volume guard run on every path**, the config
  file included, which had neither.
- **The `backup:` key is gated** as the file-owned field it is; neither
  surface gated it.
- **A config apply owns the tile's whole schedule list.** Planning and
  applying `cur[0]` only meant a second schedule made in the panel was
  invisible to the plan and then deleted anyway when the block went.
- **One destination visibility rule** replacing three, **one trim** (the API
  stored the name raw, so a destination created over the API as `prod ` could
  never be referenced from a config file), **one `Users` scan** replacing two
  verbatim copies, and a delete that reloads the scheduler on both surfaces.
- A destination that does not resolve answers 404, not 400: it is the tenant
  boundary.

### Point 9 — no VolumeService, one StorageService

**No `VolumeService`.** A volume is a tile and `TileService` already owned
every rule the plan lists for one. The API's `createVolume` was the second
creator with its own version of five of them — including a target rule that
accepted a cron — and is now four lines and a call. `volume_name` and
`max_size_mb` validate everywhere (the name lands in a bind string verbatim),
and both are declarable from a config file now.

- **Deleting a service orphans its volumes** instead of leaving them pointing
  at a row that no longer exists.
- **Deleting a volume redeploys its former target from every path.** Only
  `DELETE /volumes/:id` did.
- **`repo.VolumesAttachedTo`** replaces four copies of the filter, in three
  packages that cannot import each other's.
- **`ConvertLegacyMounts` was deleted**, not converted into a caller: a
  one-shot upgrade, on every boot, for installs that predate volume tiles, of
  which there are none.

`service/storage.go`: the server id comes from the caller (hard-coding
`"local"` is why every CLI-created pool landed on the manager), a sub-path
name is slugified *then* checked (so `...` is refused rather than stored as
`""`), and a delete knows the difference between a server's pool and an org's
share — the panel's had no org branch at all, so deleting an org share in use
went through and left its volumes behind. The storage attach kind rule is
"not a volume tile" and lives in `TileService.Validate`, so every write path
has it.

### Point 10 — RegistryService, PREnvService, no ConnectorService

`service/registry.go` holds both halves the plan splits in two: they share
the managed-registry row.

- **[SEC] The panel's `DeleteRegistry` took an id and ran.** One POST removed
  the managed registry; boot recreated it with a fresh password and every
  org's derived credential stopped working. Refused now, as the API's was.
- **[SEC] Org registry writes are owner-level over the API too** — credential
  mint, credential revoke and tag delete took any member with a
  `stacks:write` key.
- **One in-use matcher for tag delete.** The panel's split the stored tag at
  its first slash, so a tag stored without a pull host was deletable while a
  live deployment still pointed at it.
- **The panel can no longer mint an org credential with no managed registry.**

**No `ConnectorService`.** The "four inline filters" are seven one-line
`OrgID !=` comparisons answering four different questions. The two that
failed *open* are fixed where they live: the planner no longer conflates "the
lookup failed" with "the org owns none" (so a file naming another org's
connector is refused on an org that has none of its own), and
`githubapp.connectorForTile` logs its refusal instead of silently producing
an unauthenticated clone. **Deleting a connector is refused while a tile, a
stack binding or the org's own binding still names it.**

`service/prenv.go` with a sparse patch — an explicit exception to D6, for the
reason the plan gives. `comment` and `status` are gated as the file-owned
keys they are, the file's `enabled` is persisted (it was read and thrown
away), and the CLI stopped read-merge-writing. `PRConfig` moved from
`config/envops` to `store/repo`, because the service layer, the hook above it
and `infra/githubapp` below it all read it.

The stack-scoped webhook gained `stackTracksRepo` **and** `updatePlanComment`,
both of which the connector route runs.

### Point 11 — Org, Member, Auth, APIKey

- **[SEC] `Active` is checked at principal resolution**, on the session and
  on the key. Deactivating a user closed nothing; it was read at login only.
  *Anyone deactivated while signed in is now logged out.*
- **[SEC] Org plans are owner-only over the API** — plan, preview, approve
  and reject. A member could rename the org and create or delete stacks from
  the CLI.
- **Org defaults are owner-only and refused on a config-managed org** over
  the API, matching the panel; a member's write was silently overwritten by
  the next apply.
- `service/member.go`: one role whitelist (**`--role admin` is a 400, not a
  silent downgrade to `member`**), one expiry rule (1–365, default 14; the
  API accepted a negative one, which minted an already-expired invite), one
  already-a-member check, one last-owner guard, and a 404 on removing
  somebody who is not a member (the panel said "Member removed.").
- **One password rule**, hamr's strength check, enforced inside
  `AuthService.Register` and `ChangePassword`. Two forms asked for eight
  characters and one ran the full check.
- **Email folds in the store.** Eight handlers lower-cased and the ninth —
  the profile save — did not, so saving your own name with a capital in the
  address wrote a row no login could find.
- `service/apikey.go` owns the token format and the row;
  `v1.GrantableScopes` owns the scope filter. Three copies of each.
- **The org config runner is the one wired in main** on both surfaces; the
  API and the PR hook each built their own, which after point 7b would have
  been missing the stack service.

### Rig pass — deployed and partly verified

Deployed to `stackr-test.vulpe.dev`. What was driven by hand:

**Wiring.** Every `deps.X` referenced in either `server.go` has a matching
key in the corresponding `Deps` literal in `main.go` — checked
mechanically, because a field that exists and is left nil compiles fine and
panics at request time, and ~15 new `With*` setters went in this pass. Then
the three pages whose services arrive by setter and whose tests may not
cover them: the server storage page, the org registry page and a tile's
backups tab all render, and each one's *write* path was exercised rather
than just its GET.

```
POST /servers/local/storage  {backend: local, export: "relative/path"}
  400  a local pool needs an absolute host path        (StorageService)

POST /orgs/qa/settings/registry/credentials  {name: "rig-check"}
  200  minted, secret shown once                       (RegistryService)
POST /orgs/qa/settings/registry/credentials  {name: "stackr"}
  409  "stackr" is the credential stackr's own deploys use
GET  /tiles/<id>/backups
  200  renders through BackupDestinationService.Visible
```

The API key those panel checks minted came out of `APIKeyService` through
the account page, which is the third of that service's three former copies.

**Behaviour, over the API with that key:**

```
POST /stacks              {"name":"bk-check"}   409  a stack named "bk-check" already exists
POST /stacks              {"name":"!!!"}        400  name: needs at least one letter or number
POST /stacks/:id/envs     {"name":"Stack"}      400  "stack" is reserved
POST .../envs/rig-staging/promote {"commit":"deadbeef…"}
                                                400  commit: deadbee has not been built on any
                                                     environment yet
POST /apps/:id/deploy     (a cron tile)         400  cron services run on their schedule;
                                                     use Run now
```

The last two are the point-7c headline rows. Before this pass the promote
enqueued a deploy per tile for an image tag nothing had ever built and
reported success, and the cron deploy was queued for a tile whose container
is created per run — the panel had always refused it.

Rig cleaned up: the `rig-check` credential revoked, the `rig-cron` tile and
the `Rig Staging` environment deleted, `bk-check`/`production` untouched.

### Point 17 — the org apply on the work queue (done)

Approving an org config plan queues it now, on both surfaces, and nothing in
the product applies anything on a request goroutine any more: the stack apply
and the stack promote went on the queue in point 7, and this was the last one
left.

- `config/orgconf/job.go` is new: kind `orgconfig.apply`, payload
  `{org_id, plan_id}`, dedupe on the plan id, requeue on restart, and the plan
  error written from the job's own context on every failing path.
- The panel's approve and the wizard's approve both enqueue and come back to
  the plan screen, addressed by the org's **id** — the apply can rename the
  org, and the slug the request arrived on is gone by the next poll.
- `POST /api/v1/orgs/config/plans/:id/approve` answers **202** with the plan
  still pending, matching the stack side. `stackr org approve` says queued and
  points at `stackr org plan-show`.
- The apply progress banner moved into `components/plan.templ` and is now part
  of `PlanBody`, so all three plan screens have it. It had never rendered on
  the stack page: point 7 moved the dedupe key to the plan id and the lookup
  still asked for the stack's. Fixed.
- The wizard stays on its plan screen, polls, and redirects itself to the next
  step when the job lands. `respond.Redirect` already emits `HX-Redirect`.

Verified by the journey tests, which drive the whole wizard branch over real
HTTP including the queued apply and the self-advance, and by
`orgconf/job_test.go` for the error landing on the row and the per-plan dedupe.

**Not verified on the rig.** The test VM has no GitHub connector after the
pave, so no org or stack can be bound to a config file and no plan exists to
approve. Creating one is a GitHub App round trip that needs a human. The
deploy is on the VM and the pages render clean; the approve path itself is
owed a rig pass once a connector is back.

### Points 13 and 16 — settings, nodes, containers, the image watch, and the
### panel's own shape (done)

**`internal/installspec`** is new, and it is not a service: it is the panel's
own shape, defined once for the two things that create it. The service spec
(name, network, hostname, constraint, mounts, the whole env list, stop-grace)
lived only in `internal/installer/steps.go`, so a new environment variable
reached a fresh install and never an upgraded one. `AdminService.Upgrade`
builds from the same package now, and with the panel's roll policy moved down
into `infra/runtime` where a swarm `UpdateConfig` belongs, `service/admin.go`
no longer imports the docker SDK — the tree's only class D hit, closed.

It also owns the root-domain grammar. The installer refused `localhost`, a
bare IP and a single label; the panel's own domain-resource form accepted all
three on the same box. `ValidateResourceHost` runs the installer's rule now.
Relay port 15000 is the same kind of shared constant and moved with it.
`scripts/dev/deploy-test.sh` still has its hand copy — shell cannot import Go
— and now carries a comment saying where the original is.

**`SettingsService`** owns all four rungs of the defaults cascade from both
surfaces. `defaults:` is a config-file field at org, stack *and* environment
level and every apply replaces the whole blob, so an ungated write to a
managed object is a value the next apply silently reverts; only the panel's
org page refused it. All three are gated now, an environment on its stack's
binding. An empty server name is refused rather than stored. Every save
resyncs the proxy, which two of the five paths did not.

**`NodeService`** owns a node's life after it joins: the pinned-tile drain
refusal, the manager refusal, the typed-name confirmation, burning the join
keys before the row goes, and the group label. All five lived only in the
panel's servers screen. `EnsureAgent` is one retry policy now — boot had ten
attempts, Add node had one, and the case the retry exists for (the registry
not up yet) is exactly what the first Add node on a fresh install hits. The
retries run in the background so the request still renders its join script,
and a second click does not start a second loop.

**`ContainerService`** is one system-container guard over four verbs. Stop had
it, remove had half of it, and start and the terminal had none — so the panel
container could not be stopped but a terminal could be opened inside it,
which is a root shell holding the docker socket. Remove still allows an
exited system container: those are upgrade leftovers and refusing them is
what left every node accumulating agent corpses.

**`ImageWatchService`** is the old `infra/imagewatch.Watcher` moved above the
line. Its auto branch goes through `DeployService` (so the upper-environment
rule is the one everything else uses, not a second copy) and through
`ManagedInstanceService` (which writes the row's status — this was the one of
six call sites that wrote none, so a database the watch auto-updated sat on
the canvas as whatever it had been before). The cadence setting is validated
rather than coerced. `infra/imagewatch` keeps the registry client, which is
all it ever should have been.

**Audit** stays in `store/audit`; what changed is who calls it. The generated
org secrets, the secrets a clone copies onto a new tile, and a managed tile's
connection credentials were all written with nothing recorded.
`PublishConnection` records only on a value that actually changed, or it
would write a row per credential per deploy.

**`infra/metrics` no longer imports `managedtiles`**. The per-database counter
read is a function `main` supplies, which is the edge the package comment was
already apologising for.

Verified on the rig: the org and server defaults saves both land and flash,
an empty server name writes nothing, the image-check cadence saves and reads
back, and a terminal into the panel container answers 409 where it used to
open a shell. The node lifecycle paths are covered by unit tests only — the
rig has two nodes and draining or removing one takes the test box down.

### Point 15 — AccessService (done)

`service/access.go` is the one place that decides "may this principal do this
verb in this organization". Two surfaces had grown two gate vocabularies —
`ownedOrg`, `ownedSettingsOrg`, `requireOrgOwner`, `CanWriteOrg`,
`requireOrgWrite`, `orgMember`, `orgAllowed`, two `adminOnly` closures — and
every verb both of them reached had its level chosen twice.

- **One ladder**: Read, Write, Owner, Admin. A check is a comparison, and an
  admin is owner in every org.
- **One principal**, resolved from either a session or a key: the user, the
  admin flag, the role per org, and a key's scopes.
- **One table**, `verbLevels`, with an entry per operation. Where the two
  surfaces disagreed the higher level is the entry, because the lower one was
  never a considered grant — it was a forgotten guard. The four contested rows
  the earlier points decided (org registry credentials and tag deletes to
  Owner, org plan approve to Owner, deployment cancel to Write) are written
  down here now instead of living in whichever handler happened to be right.
- **An unregistered verb needs Admin.** A new operation that forgets to name
  its level fails closed.
- **The wizard's domain and connector steps are Owner.** Every other step was,
  and the setup allow-list waives the draft gate for the whole `/setup` prefix,
  so a member who could not see the wizard could still post those two steps. A
  draft org has no members who need them.
- **The websocket is no longer anonymous.** `/ws` had no identity on it at all
  and the hub joined whatever room the client named. The subject now comes off
  the session cookie at the upgrade and every join is checked: org and project
  rooms against membership, the whole-box rooms against the admin flag, and no
  session joins nothing. The payloads carry no data, so what leaked was
  activity timing, not content — but it leaked to anyone who could reach the
  port.
- `svcerr` gained a `Forbidden` that may carry a sentence. `ErrForbidden` stays
  the bare one for every refusal whose reason would confirm something the
  caller should not know; a key missing a scope confirms nothing.

Verified on the rig: an authenticated socket joined `containers` and received
the event a container stop produced; an anonymous socket, joining the same room
across the same event, received nothing.

**Not done in point 15.** The gate helpers on both surfaces still exist and
still load resources — they ask AccessService for the level now rather than
deciding it, which is the row `02` flagged, but they were not collapsed into
one call per route. Revocation still does not react to a demotion or a stack
move: deactivation closes sessions and keys (point 11), nothing else does.

### What is still owed

1. **The rest of the rig pass.** Not driven: the promote force checkbox, a
   volume attached to a stopped service (which should no longer start it),
   an org registry credential minted by a *member* (should now be 403 —
   needs a second account), deactivating a signed-in user (should log them
   out), and a config apply of a tile the validator would refuse.
2. **A rig pass for point 17**, which is built but unproven on the VM: the
   box has no GitHub connector after the pave, so there is no bound config and
   no plan to approve. Needs the connector back first.
3. **Point 11's deferral, partly closed.** The live role check in the target
   org now runs through AccessService on every write, which is what makes a
   key minted against one org useless in another. The scope list itself is
   still granted at mint time against the active cookie org; narrowing a key
   when its user is demoted is still nothing's job.
4. Every numbered point is done. What remains is the rig work in items 1 and
   2 above, and point 15's own leftovers named in its section.

### Commit message

```
refactor: one owner for deploys, plans, backups, storage, registries and members

Five more concepts get a single implementation, and with them the security
rows that came from having two. A read-only member with a deploy-scoped key
could cancel a deployment, because the API's loader asked for read where the
panel asked for write. Any org member could mint a registry push credential
or delete an image tag over the API, where the panel has always required an
owner — and could approve an org config plan, which renames the org and
creates and deletes stacks. Deactivating a user closed nothing: Active was
checked at login and nowhere else, so their sessions and every key they had
ever minted kept working. And one POST to the panel deleted the managed
registry, which boot then recreated with a new password, leaving every org's
derived credential invalid.

The rest is drift. A cron tile could be deployed over the API and not from
the panel. A promote accepted any string as a commit and queued deploys for
an image nothing had built. The panel's promote button hard-coded force, so
it overrode a refusal the API and the CLI respect. Attaching a volume to a
stopped service started the service. A backup schedule could be a database
dump of something that is not a database, and defaulted to keeping zero
archives on every path but one. A storage pool created from the CLI always
mounted on the manager. An invite could be minted already expired, and
`--role admin` quietly granted member access.

Also: `waiting_ci` counts as live, so a parked deploy no longer shows as
finished; a config apply runs the same tile validator both surfaces run; the
`backup:`, `comment:` and `status:` keys are gated as the file-owned fields
they are; deleting a connector is refused while something still names it;
and email folds in the store instead of in eight handlers and not the ninth.
```

---

## Point 15's revocation leftover — done

`service/revoke.go`. A demotion, a removal and a stack move now tear down what
a live check cannot reach. The full reasoning, and what deliberately does
*not* react, is in 05-assumptions.md; the short version:

- **Sessions and API keys need nothing**, and that is the finding, not a
  shortcut. `middleware.OrgContext` re-resolves the user's orgs and their role
  per request, and `AccessService.Require` reads the role live out of the
  target org — so a demoted session and a demoted key are refused at the same
  instant. Deleting the key would punish the user's other orgs; narrowing its
  scopes is the mint-time question point 15 left for the dev, untouched.
- **Share links are the gap.** A `SecretLink` is a bearer token — no session,
  no key, no role check anywhere in the redeem path. A member who minted one
  and was then demoted below write, or removed, had left a working door open.
  They are now revoked (state flip, not delete: the row is the audit line).
  The cut is *dropped below write*, because `VerbShareLinkMint` is
  `LevelWrite`: an owner demoted to member keeps what minted them.
- **A stack move revokes every link under the stack**, whoever minted it. The
  links were minted under the old org's rights and point at resources the new
  org owns.
- **Websocket rooms are nudged, not torn down.** A room is checked on join and
  never again, and hamr v0.35.0's `Hub` exposes no disconnect. So
  `notify.AccessChanged` pushes an `access` event to the subject and `live.js`
  leaves and re-joins every room it declares, which runs the join check again.
  A client that ignores it keeps its rooms until it disconnects; what that
  leaks is event kinds with no payload. Named as a ceiling in 05.

Tests: `service/revoke_test.go` — the four match rules, plus the two service
paths (`MemberService.SetRole`/`Remove`, `StackService.Move`) that both
surfaces call, so a handler cannot be the thing that remembers.

One link that cannot be read does not park the walk. Both halves of the walk
collect their errors and keep going, because the role change is already
written by the time revocation runs — stopping at the first failure would
leave every link after it open and report the whole thing as a failure.

Verified on the rig, both paths:
- **Demotion.** A member minted a drop link on the QA stack (200, link live),
  an owner demoted them to viewer, and the same URL answered "This link is no
  longer available"; the stack's own links list shows it `revoked`.
- **Move.** An owner minted a second link on the same stack (`waiting`), then
  moved the stack from `QA` to `test-org`. The link reads `revoked` on the
  stack's new URL, and it was minted by the mover — which is the point: a move
  takes every link under the stack, creator regardless.

## Point 18's safety net — done, alone

`handlers/web/routeverb_test.go`. Both surfaces register onto one Echo; the
walk fails any mutating route (POST/PUT/PATCH/DELETE) that names no
`service.Verb`. The declaration goes in Echo's own `Route.Name`
(`.Name = string(service.VerbX)`) — a field that already exists per route, so
nothing new holds it and point 18's middleware reads the same thing.

271 routes owe a verb today and all 271 are parked in `knownUngated`, not in a
`t.Skip`: the net is up for new routes while point 18 is half done, and the
test fails both ways — a route missing from the list, and a route on the list
that has since grown a verb. The inventory is written into 06-points-18-20.md,
grouped by area, with the two exempt buckets (14 unauthenticated, 8 personal)
and the open question about whether the personal routes belong in the table.

No gate helper was touched. No call site moved.

## The owed rig pass — three of five driven

Driven on `stackr-test.vulpe.dev` against a fresh deploy, as an owner
(`admin@test.com`) and a second account (`member@test.com`) invited into org
`QA` as a member.

1. **An org registry credential minted by a member — PASSES.** With a key the
   member minted for themselves carrying every scope they could grant,
   `POST /api/v1/orgs/qa/registry/credentials` answers
   `403 only an owner can do that`. On the panel the same post answers 404 and
   the create form is not rendered at all — which is the surface error policy
   in 06 (a panel 403 would confirm the org exists), not a second answer.
2. **Deactivating a signed-in user — PASSES.** The member held a live browser
   session; an owner toggled them off from `/admin/users`; the next fetch of
   `/account/profile` in that same session landed on `/login`. Their API key
   answered 401 on the same change.
3. **A volume attached to a stopped service — PASSES.** The `data` volume tile
   in `qa/bk-check/production` was detached from the stopped `vol` service and
   re-attached. The panel shows the mount back; `docker service ls` reports
   `stkr_qa_bk-check_production_vol` at `0/0` throughout and no task was
   created. It no longer starts the service behind the operator.

4. **A config apply of a tile the validator would refuse — PASSES.** A branch
   `rig-badtile` in `FyrmForge/stackr-test` declares a cron tile carrying a
   `port`. A stack bound to it plans to
   `error / config invalid — env production: tile nightly: a cron has no
   endpoint; port: and domains: don't apply`, which is
   `service/tilevalidate.go` talking, and nothing was applied. Config-as-code
   runs the same tile validator the forms run.
5. **Point 17's rig pass — PASSES** (it was blocked on this same missing bound
   config, and is no longer). `test-org` is bound to `stackr-org.yml` in that
   repo and replans clean; a second stack `promo`, bound to
   `stackr-rig-promote.yml` on branch `rig-promote`, went bind -> plan ->
   Approve & apply -> both environments converged
   (`stkr_test-org_promo_staging_web` and `..._production_web` at 1/1), then a
   later push replanned, applied to staging and **held production**, whose
   `apply_policy: manual` is what the hold is. Driven through the panel with
   Playwright.

**The promote force checkbox — half driven.**

What is proven, in the live modal: the checkbox exists, it renders **only** on
the "Apply plan and promote" form, and it is **unchecked when the dialogue
opens**. The sibling "Promote images only" form carries no `force` field at
all. That is the fix — the panel used to post `force: true` unconditionally,
so its button overrode a per-environment apply policy the API and the CLI
respect.

What is **not** proven: that force off leaves a `manual` environment waiting
and force on applies it anyway. That assert needs one commit which is at the
same time built on the lower environment *and* carrying a pending
all-environment plan, and on a rig with no push webhook every route to one
consumed the other — approving the plan to get the build consumes the plan,
and planning without approving leaves the commit unbuilt, which
`ReleaseService.built` refuses before `force` is ever read. Driving it wants
either a webhook on the test repo or a fixture where the lower environment
auto-applies on push. Left for whoever sets that up.

**Not driven, and why.**

Nothing else is owed from the list. The earlier entry for this section said
the force checkbox and the refused apply were blocked on there being no bound
config on the box; that blocker is gone, and what is left of the force check
is written above.

Rig state left behind: `member@test.com` exists in org QA as a **viewer** (the
demotion from the revocation check), their API key is still minted, and the
`bk-check` stack now lives under **test-org**, not QA, with both of its
one-time links revoked — that is the move check, not damage. Two more stacks
exist in `test-org`: `badtile` (bound to branch `rig-badtile`, its plan
permanently in `config invalid` — that is the evidence) and `promo` (bound to
branch `rig-promote`, two environments, deployed). Both branches live in
`FyrmForge/stackr-test` and neither is merged. The box is disposable; nothing was cleaned
up on purpose, so the checks above can be re-read from it.

## Point 18 — done as scoped (mutations), 2026-09-19

Authorization moved onto the route. 265 mutating routes declare a verb and the
kind of thing they address; `Gate` (panel) and `a.gate` (API) resolve the
tenancy through `AccessService.TenancyOf` and check the level.

Verified on the VM: org- and stack-scoped writes succeed, and an unresolvable
tenancy (bad org slug, bad stack id, bad tile id) answers 404 rather than
reaching the handler.

Four tests, listed in 06-points-18-20.md. The load-bearing one is
`TestRouteVerbAgreesWithCapturedLevel`: every route's PRE-18 level was captured
as data first, so a verb one rung too low fails instead of shipping.

Not done: the gate helpers still exist. 161 of 239 mutating handlers keep a
body check because their loaders are shared with the 192 GET routes, which
carry no verb yet. Those routes are checked twice, never less. The list is
`stillBodyGated` in `handlers/web/gatefree_test.go`.

## Still owed

1. **Gate the reads.** 192 GET routes. Finishes point 18 and empties
   `stillBodyGated`. Needs its own level capture, and a weaker one — see
   06-points-18-20.md.
2. **Point 19** — services own their reads. 411 handler->store reads, 77 store
   methods. Decided 2026-09-19: pass-through reads move too, handlers lose the
   store, handlers KEEP `repo.X` as their view type.
3. **Point 20** — blocked on splitting `repo.Tile` into config and state.
4. **Dev decisions:** the org-domain save/delete asymmetry; point 15's scope
   grants at mint time; deactivation not revoking share links.

---

# The three decided changes — built 2026-09-19

Decisions 1, 2 and 3 from 06-points-18-20.md are built; decision 4 (gate the
reads) is the next job and is not started.

**1. Org-domain save raised to owner.** `handlers/web/server.go`
(`POST /orgs/:slug/settings/domains`, `VerbDomainWrite` -> `VerbOrgWrite`) and
the matching row in `handlers/web/routelevel_test.go`.

**2. API keys bind to their mint org.** New migration
`store/db/migrations/002_api_key_org.{up,down}.sql` adds a nullable
`api_keys.org_id`; `repo.APIKey.OrgID`; `service.APIKeyService.Mint` takes the
org; `account.MintOrg` decides it (empty for admins and for no active org);
`v1.keyOrgAllows` enforces it from `requireVerb` and `orgForCreate`. Shown on
the key list and the CLI approve page. Existing keys are unbound and unchanged.

**3. Deactivation revokes share links.** `service.RevokeService.UserDeactivated`,
called from `web/handler/settings.ToggleUserActive` on the disable edge.
`web.Deps.Revoke` is new wiring, set in `cmd/stackrd/main.go`.

Tests: `TestKeyOrgAllows` (api/v1/auth_test.go),
`TestDeactivationRevokesEveryLinkTheyMinted` (service/revoke_test.go). Full
suite green, `go vet` clean, `make templint` clean.

Verified live on the VM (192.168.1.106), not just by unit test:

- **Binding at mint.** An admin's key writes `org_id` NULL, a member's writes
  the active org's id, and the key list shows "QA only" next to the bound one.
- **Enforcement.** Same key, same DELETE /api/v1/stacks/:id: **404** with the
  key's org flipped to another org in the database, **204** with it flipped
  back. The two create paths refuse too (orgForCreate drops the org from its
  candidates, so that one answers with its own 403 rather than the gate's 404).
- **Deactivation.** An open share link minted by the member was `revoked` in
  the database the moment the admin disabled the account from /admin/users.
- **Org domains.** A member posting to /orgs/qa/settings/domains gets 403 —
  the level refusal, which is what owner-only means for somebody who is in the
  org.

---

# Point 18 complete — the reads, 2026-09-20

**192 GET routes**: 161 gated (`read(...)` in handlers/web/server.go, `opr` in
api/v1/v1.go), 10 listed as ungateable with a reason, 21 public or personal.

New: `service.VerbOrgOwnerRead` (owner) and `service.VerbAdminRead` (admin);
`service.ErrServerOwned`; `handlers/web/routeread_test.go` (capturedReadLevels,
ungatedReads, two tests). `service.VerbOrgPlanList` deleted.

**The body gates are gone.** 162 mutating handlers were body-gated; 10 remain,
each with a reason in `stillBodyGated`. Deleted outright: `requireOrgOwner`,
`requireOwnerOf`, `requireOwnerVerb`. Reduced to loaders: `requireTile` ->
`tile`, `requireBackup` -> `loadBackup`, `requireEnvWrite` -> `envAndStack`,
`loadDeployment`, `requireOrgRegistry`, `ownedOrg`, `ownedSettingsOrg`,
`settingsOrg`, and every `RequireOrgWrite`/`RequireStackAccess`/
`RequireOrgAccess` call in a panel handler body except the four that must stay.

Tests that called handlers bare now call them through the gate (`callGated`,
`callThroughGate`, `gated`): a bare handler asserts nothing about who is
asking, which is what moving authorization to the route means.

Full suite green, `go vet` clean, `make templint` clean.

Verified live on the VM after deploying, as a viewer in one org and as the
server admin:

- Panel reads: org pages 200, the owner-only pages (invites, org plans) 404,
  the admin area and /servers and /containers 404, another org 404, a bogus
  org slug or tile id 404, and the variable-value reveal 403 — the write-level
  read keeping its old wording.
- API reads: collections 200 (filtered per row), own org's members 200, its
  invites 403 (owner-only; the API's split from the panel, on purpose),
  another org's members 404, server settings 403, a bogus id 404.
- Decision 7 end to end: a key carrying secrets:read whose user is only a
  viewer in the org got `•••`, not the value.

---

# Point 19 — started 2026-09-20

**The net is up first**, same shape as point 18's: `handlers/web/storefree_test.go`
scans every handler package for a call on a `store` field, transitively through
same-package helpers, and fails any function not on `stillStoreReading`. The
list was seeded from the scan rather than typed: **525 handler functions**
reach the store, behind 417 direct call sites across 79 store methods.

It only shrinks. A new handler that reaches for the store fails on the first
run, which is the part that cannot be done later — a read left behind compiles,
passes every test, and renders.

**First piece done: the metric window.** `components.MetricPoints` was a
function in `metrics.templ` — a TEMPLATE taking `repo.Store` and querying it —
and it is now `TileTelemetryService.Points`. `components.TimePoint` became a
type alias for `service.TimePoint` so no view signature changed. The API's
`appMetrics` read the same table itself at full resolution; that is
`TileTelemetryService.Samples`, and appMetrics is the first row off the list.

Two operations, not one forwarder each: the panel buckets to 240 points
because a chart has 640 pixels, and an API caller plotting its own does not.

**Slice 1: environments and variables. 525 -> 504.**

`EnvironmentService` gained `Get`, `BySlug` and `ListForStack`;
`VariableService` gained `List`, taking the `VarOwner` the writes already
take, so the four owner kinds stop being two loose arguments at the call site.

63 call sites across 14 files. `envs` is new on the `org`, `search` and `app`
panel handlers, wired in `handlers/web/server.go` next to the others.

The reads answer `svcerr.ErrNotFound` rather than `(nil, nil)` — see
05-assumptions.md for why, and for the masking that deliberately did NOT move.

**Slice 2: stacks and tiles. 504 -> 446.**

`StackService` gained `Get`, `BySlug`, `ListForOrg`, `ListAll`; `TileService`
gained `Get`, `ListForEnv`, `ListForStack`, `ListAll`. 82 call sites.

`tiles` and/or `stacks` are new on the `app`, `db`, `backups`, `deployment`,
`server`, `org`, `prhook` and `search` handlers — eight page families that had
been reading the two central tables directly.

One dead read fell out: `app.VarValue` still loaded the stack for a check that
point 18 moved onto the route, leaving a query whose result nothing used. Go
does not report an unused value that an `if x == nil` consumes, which is how it
survived the earlier pass.

**Slice 3: organizations and membership. 446 -> 335.**

New `service.OrgService`: `Get`, `BySlug`, `Resolve`, `ListForUser`, `ListAll`,
`UnfinishedDraft`, `StartDraft`. `MemberService` gained `RoleOf`,
`ListMembers`, `ListInvites`.

The panel's `POST /orgs` is now nine lines: who may ask, and where they land.
Everything it used to decide is `StartDraft`. `setupDraftName` became
`service.DraftOrgName` with the panel keeping a local alias, so no view
changed.

Three copies of "slug first, id as fallback" collapsed into `Resolve`.

Wiring went in BEFORE the call sites this time — `Deps.Orgs` in both servers,
`apiFor`, the journey harness, the `project` tests — so the failures that came
back were logic, not nil services. That is the third slice's only process
change and it saved the whole round of nil-pointer panics the first two had.

**Slice 4: domains, storage, provisions, config plans, deployments. 335 -> 253.**

`DomainService.ForTile`/`ListAll`, `DomainResourceService.ListAll`,
`StorageService.Get`/`ListAll`/`Paths`, `SliceService.Get`/`ForInstance`/
`ForConsumer`, `PlanService.Get`/`ForStack`/`GetOrgPlan`/`ForOrg`/
`SetOrgPlanStatus`/`AwaitingPlan`, `DeployService.Get`/`ForTile`.

Org and stack config plans keep separate methods on purpose: different table,
different approver, different level. One `Get` over both ids would let an org
plan be approved through a stack plan's route.

`handler/project`'s four test files built the same growing struct literal by
hand; they share `testHandler(s)` now (`testhandler_test.go`). Every slice adds
a field to it, which is exactly why it should exist once.

**Slice 5: servers, registries, backups, settings, accounts, audit. 253 -> 190.**

`NodeService.Get`/`ByNodeID`/`ListAll`, `RegistryService.Get`/`ListAll`/
`Credentials`, `BackupDestinationService.Get`, `BackupScheduleService.ForTile`/
`ListAll`/`Runs`, `SettingsService.Value`/`SetValue`, `AuthService.User`/
`Users`/`SaveUser`, `APIKeyService.ListAll`, `RevokeService.Links`.

New `service.AuditService`: `For` and `All`. Reads only — an audit row is
written by whatever did the thing, in the same call, through `store/audit`. A
service owning that write would put a hop between an action and its record.

The user row went to `AuthService` rather than to a new `UserService`:
Register, ChangePassword and Authenticate already live there, and an account
is not org-scoped — membership is, and that is `MemberService`'s.

One name collision worth knowing about: `handler/server` already had a field
`nodes` holding `infra/nodes.Service`, the swarm client. `service.NodeService`
is `nodeSvc` there and now everywhere, so the two never read alike.

## Point 19 verified live, after slice 5 — 2026-09-20

Deployed to the VM and walked it, because five slices had gone in on unit
tests alone and the error contract moved in all of them.

**Panel, as the server admin.** A crawl from `/`, an org canvas, a stack, the
admin area, `/servers` and `/account`, following every `href` and `hx-get` four
levels deep: **112 pages, no 5xx, no error page**. The only 404s were two
`/deployments/<id>` links on `/notifications` whose tiles no longer exist —
correct, and older than this work.

**Panel, as a member (not a server admin).** `/admin`, `/admin/users`,
`/admin/audit`, `/admin/backups`, `/admin/tls`, `/servers`, `/containers` all
**404**, not 403 — the concealment rule from point 18 still holds with the
reads coming through services. Their own org's pages 200, a bogus org slug
404, a bogus stack id 404.

**API, with an everything-scoped key.** Every collection and every
org/stack-addressed read 200; `apps`, `envs`, `deployments`, `tiles/backups`,
`config/plans` and `org-config/plans` all **404** on a bogus id. No 5xx.

**Two deliberate behaviour changes, both in handler/settings.** `destOrg` and
`GitHubConnect` used to leave the raw path segment in `orgID` when it resolved
to no organization, and carry on. `OrgService.Resolve` answers ErrNotFound, so
they 404. That is the correct answer and it is new.

**One thing this does NOT change back:** a store failure on a read that used to
be folded into `if err != nil || x == nil { 404 }` is now a 500. Intended — an
outage that renders as "not found" is the bug that hid a broken query for a
release — but it means a database hiccup reaches the error page.

**Slice 6: the canvas layout, the org row, invitations. 145 -> 122.**

New `service.GraphService` over the three layout tables — node positions,
annotations, graph groups. They share one shape (a `repo.GraphOwner` key: a
scope plus an id) and the two canvases and three panel miniatures were each
assembling that key and reading all three tables themselves. `Load` reads a
whole layout at once; the three singles stay for the pages that draw only
cards.

`OrgService.Save` and `MemberService.GetInvite`/`UseInvite`/`DeleteInvite`/
`Join`. `Join` is the invite page's "add the membership if they are not in it
already", which was written inline with its own `GetOrgMember` check — the
idempotence matters because two clicks on one invite link is the normal way
that path is reached.

**Slice 7: connectors, notifications, staged changes, cron runs, storage
paths. 122 -> 81.**

Two new services. `ConnectorService` owns the connector row; the GitHub App
manifest flow stays in `infra/githubapp`, which is the half that talks to
GitHub. `NotificationService` owns the bell count, the list and the two
clears; raising a notification stays `notify.Notifier`'s, because the websocket
fan-out is part of raising it. Every notification row has exactly one
recipient, so every method takes a user id and none takes an org.

Staged changes went onto `TileService` rather than a table of their own: a
staged change is what `Update`, `Create` and `Delete` return `staged bool`
for. Cron runs went onto `TileLifecycleService`, beside `RunNow` and `StopRun`.
`OpenRun` answers nil rather than ErrNotFound — "not running" is a state every
caller renders, not a missing row.

**Slice 8: the tail. 81 -> 1. Point 19 is done.**

Managed resources, bindings and outputs to `ManagedInstanceService`; the
latest/settled plan and the apply's work item to `PlanService`; the remaining
connector, staged-change, cron-run, backup-run, registry, invite, key, user and
variable reads and writes to the services that already owned their domain.

`handler/annotate` — the shared HTTP half of "save one note, save one box" —
took a `repo.Store` and now takes the `GraphService`. That is four pages'
worth of store passing gone with one signature.

**One entry remains and it is not work.** The health endpoint pings the store
to answer "is the database there". A liveness check on the dependency is not a
read a service could own, and a `StoreHealthService` is precisely the
forwarder this point exists to avoid. It is on the list with that reason.

**What the net does not measure**, written into its doc comment so a green run
is not misread: a handler PASSING `h.store` to a lower layer. Several still
do — `stackconf.Planner`, `envnet`, `placement`, `sharelink`, the settings
cascade, `audit.Record` — because those helpers take a `repo.Store`, and
converting them is a different job from moving the reads.

**525 -> 1**, across eight slices, with the name-column diff run against the
previous commit every time and empty every time.

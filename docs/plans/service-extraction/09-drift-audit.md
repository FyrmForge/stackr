# Drift audit — after points 1-20

Measured 2026-09-20 against the tree at `6b60ae1`. Nothing was changed; this is
a findings list.

The question asked: what drifted, what is left over, and what still has two
implementations of one rule.

---

## Part 1 — drifts and leftovers

### D-1. The store guard had a hole, and something was already in it — FIXED 2026-09-20

`handlers/web/storefree_test.go` banned the CALL `x.store.Method(...)`. Its AST
walk matched a `SelectorExpr` whose inner selector is named `store`. A function
that took the store as a **free parameter** and called `store.GetX(ctx, ...)`
did not match: the walk filed it under the local-helper edge, where it resolved
to nothing and disappeared.

Three functions were through it, all in a package the guard does police:

- `handler/settings.LoadOrgConnectors` — `ListConnectorsByOrg`
- `handler/settings.connectorUsers` — `GetOrg`, `ListStacksByOrg`,
  `ListTilesByStack`
- `handler/settings.DeleteConnector` — the same three, through `connectorUsers`

None was on `stillStoreReading`, so the test's "1 handler function still
reading the store" was not true.

**Fixed.** The walk now collects the receiver and parameter names declared
`repo.Store` per function and treats a call on one of those identifiers as the
banned call rather than a local edge. Widening it turned the three up
immediately, which is the proof it works — the list was 1, went to 3, and is
back to 1.

The three reads moved: `ConnectorService.Users` is new and owns "what still
points at this connector" (the delete confirmation's question, which is the
connector's own), and `LoadOrgConnectors` now takes the `*ConnectorService`
instead of the store and goes through `ForOrg`. Both handlers already held the
service. `stillStoreReading` is back to the health ping, and now honestly.

Known remaining gap in the walk, smaller than the one it closes: a file that
imports `repo` under an alias would read as a local type and slip through.

### D-2. Second hole: which packages the guard considers a handler

`handlerPkg()` only counts a path containing `/handler/` or `/api/v1`. Not
counted, and reading the store today (counts are direct `store.X(` calls per
file):

| file | reads | note |
| --- | --- | --- |
| `handlers/middleware/orgctx.go` | 9 | exempt by design — the gate resolves tenancy |
| `handlers/web/server.go` | 8 | `tilePage`, `redirectTile` — page dispatch |
| `handlers/web/registrytoken.go` | 5 | |
| `handlers/web/components/panel.go` | 3 | `EnvColor` — a *view component* assembling a read |
| `handlers/middleware/setupgate.go` | 3 | exempt by design |
| `handlers/web/setuppending.go` | 1 | |

The middleware exemption is written down and fine. The component and the
router's own page-dispatch helpers are not covered by that reason.

### D-3. Every handler package still holds the store

15 of 15 handler packages keep a `store repo.Store` field. **None** of them
calls a method on it any more — the field exists purely to pass down to helpers
that take a store: `stackconf.Planner`, `envnet`, `placement`, `sharelink`,
`envops.Ops`, `audit.Record`, the settings cascade.

64 pass-down sites: project 25, org 13, app 11, sharepub 5, settings 3,
prhook 3, db 2, server 1, backups 1.

The consequence: the field is a live handle, so the only thing stopping the
next handler line from reading the store is D-1's test, which has a hole.

### D-4. `config/` is a write surface the plan never placed

D7 put `infra/` below the line. `config/` was never given a position, and it is
the third writer of concepts the services own:

| file | raw writes |
| --- | --- |
| `config/stackconf/apply.go` | 16 |
| `config/stackconf/backups.go` | 2 |
| `config/staging/staging.go` | 2 |
| `config/stackconf/slices.go`, `job.go` | 1 each |
| `config/sharelink/sharelink.go` | 1 |
| `config/envops/envops.go` | 1 |

Tiles are the half that was fixed: `apply.go:1257` and `:1478` call
`TileService.Validate` before writing, with a comment naming exactly the drift
it closed. Backups go through `BackupScheduleService.Adopt`
(`backups.go:236`). The rest did not.

### D-5. Domains: the rules exist twice, in two languages

**Corrected from the first pass of this audit.** The config path is not
unguarded — it has its own parallel implementation of most of the same rules,
in the planner rather than the applier, and `Apply` re-plans before it writes
(`runner.go:320`), so plan-time checks are effectively apply-time checks. What
is wrong is that they are a *second copy*.

| rule | service | config-as-code |
| --- | --- | --- |
| anti-squat on another org's slug | `service.CheckOrgSquat` (`domainresource.go:61`), live `GetOrgBySlug` | `plan.go:354`, precomputed `opts.ForeignOrgSlugs` map |
| host+path has one owner | `store.GetDomainByHostPath` (`domain.go:145`) | `Plan.claimHost` / `p.hostOwner` (`plan.go:916`) |
| middleware ref must exist | `DomainService.checkMiddlewares` (`domain.go:167`) | `middlewares.go:177` |
| wildcard HTTPS needs a DNS provider | `domain.go:139` | **nothing** |
| a non-redirect domain needs a port | `domain.go:112` | **nothing** |
| managed engine must speak HTTP | `domain.go:107` | n/a (`type: service` only) |

Two things to take from it:

1. **`CheckOrgSquat`'s own doc comment is already false.** It says "Every path
   that accepts a hostname goes through here: org domain resources, tile-level
   custom domains and config-as-code domain blocks." Config-as-code does not
   go through it. That comment is the drift, already written down as if it
   were not.
2. **Two real gaps.** A stack file may declare `*.example.com` with HTTPS and
   no DNS provider configured — the certificate silently never issues. And a
   `type: service` tile with a domain and no `port:` anywhere writes
   `ContainerPort: 0`, because `syncDomains` (`apply.go:1666-1671`) only
   defaults a port for a redirect, and `TileService.Validate` does not require
   a port either (it only checks `AllowsIngress`, `tilevalidate.go:124`). The
   route renders as `http://alias:0` — the exact bug the plan records the API
   having had before point 4.

Separately: the update branch (`apply.go:1655-1662`) does delete-then-create
rather than an update, so a domain id changes on every port or TLS edit.
**Still open.**

#### D-5's two gaps — CLOSED 2026-09-20

Both rules became pure functions in `service/domainresource.go`, next to
`CheckOrgSquat`, and both call sites use them:

- `service.DomainPort(specPort, tilePort, redirectTo)` — replaces the inline
  block in `DomainService.plan`, and `stackconf/apply.go`'s own port
  resolution in `syncDomains`, so the applier cannot drift back.
- `service.CheckWildcardHTTPS(host, https, dnsConfigured)` — replaces the
  inline block in `DomainService.plan`. The config path gets the bool from a
  new `DiffOpts.DNSProvider`, filled once in `Planner.loadDomainContext`
  (`runner.go`), which both `Opts` and `StagingOpts` go through.

Both are enforced at **plan** time by `Plan.checkDomainRules`
(`plan.go`), called from `claimHosts` (tiles the plan creates) and
`diffDomains` (tiles it updates). Apply hard-aborts on a non-empty
`plan.Errors` (`apply.go:315`, `:684`), and `Apply` re-plans before writing
(`runner.go:320`), so a plan error is an apply refusal.

**This is a behaviour break, deliberately.** A file that used to apply now
fails its plan if it has a `type: service` tile with a domain and no `port:`
anywhere, or a wildcard domain on an install with no DNS provider configured.
Both were writing routes that cannot work. `domainres_test.go`'s own fixture
had to gain a `port:`, which is the proof the first case exists in the wild.

Tests: `stackconf/domainrules_test.go` (both rules, both walks — the update
walk verified by deleting the `diffDomains` call and watching it fail) and
`TestAttachStillAppliesThePortAndWildcardRules` in `service/domain_test.go`,
which is the wiring neither pure-function test would catch.

Verified live on the VM (deployed, then driven over the API as the admin, on a
`type: service` tile with no container port):

- no port on the tile and none on the domain → 400,
  `container_port: container port required`
- same tile, `redirect_to` set → 201, `container_port: 80` (not 0)
- `*.wild.example.com` with no DNS provider configured → 400,
  `host: wildcard HTTPS needs a DNS provider`
- same wildcard with `https: false` → 201; a plain host with an explicit port
  → 201

The **config-as-code** half was driven end to end on the VM, against the real
GitHub connector.

> Correction: `04-progress.md` says the box has had no GitHub connector since
> the pave, and this document repeated it. That is stale. The box has one
> (`5d48ed7d…`, org `test-org`) and three stacks bound through it. Point 17's
> owed rig pass in D-9 is **not** blocked the way both documents claim.

First, `plan-preview` with one file declaring both mistakes — on the VM
against a bound stack, and on the local dev server:

- `noport` (a new tile — the `claimHosts` walk) →
  `env production: tile noport: domain noport.example.com: container_port:
  container port required`
- `wild` with `*.wild.example.com` (a tile already in the store — the
  `diffDomains` walk, confirmed by the `update wild` row in the same plan) →
  `env production: tile wild: domain *.wild.example.com: host: wildcard HTTPS
  needs a DNS provider`
- the corrected file (a `port:`, and `https: false` on the wildcard) → no
  errors, same two changes
- setting `dns_provider` in settings and re-posting the **original** file →
  only the port error remains, which is what proves `DiffOpts.DNSProvider` is
  really filled from the setting by `loadDomainContext` and not just in tests

**The apply refusal was driven too**, through the staged-apply route
(`POST /projects/:id/staging/:envID/apply` → `ApplyStaged` → `ApplyResolved`,
the gate at `apply.go:684`), which reaches a real apply with no git fetch. The
fixture is the case that actually matters: a **legacy row already in the
store** — a `type: service` tile with `container_port = 0` and a domain with
`container_port = 0`, which is what the pre-point-4 API and the old config
path both used to write — plus one unrelated staged edit.

- apply → refused, and the panel flash says why:
  `Apply failed: plan has errors: env production: tile web: domain
  legacy.example.com: container_port: container port required`
- nothing partially applied: the tile's image was still the old one and the
  staged row was still there
- set the domain's port, apply again → `Changes applied to Production.`, the
  image moved, the staged row cleared

And `ApplyPlan` (`apply.go:238`, the config-as-code apply, which fetches the
reviewed commit from GitHub) was driven on the VM for real — a branch pushed
to the rig repo `FyrmForge/stackr-test`, a stack bound to it, plan, approve:

- bad commit → plan `pending`, `1 to add, 1 error`, the port error on it →
  approve → **refused**: the plan stays `pending`, carries
  `plan has errors: … container port required`, and no tile was created
- push the same file with a `port:`, re-plan → `1 to add`, no errors →
  approve → `applied`, the tile exists and goes to `building`

Rig branch and stack removed afterwards.

#### No migration needed

A pre-existing `container_port = 0` domain row would block every apply on its
stack until someone set the port — the refusal lands on an unrelated change.
Moot: confirmed with the dev 2026-09-20 that no stackr install exists yet,
beta included. Nothing has data to migrate. If that changes before anyone is
running for real, the rows are:

```sql
SELECT d.id, d.host, t.slug
FROM domains d JOIN tiles t ON t.id = d.tile_id
WHERE d.container_port = 0 AND d.redirect_to = '' AND t.container_port = 0;
```

---

### D-6. Other concepts on the config path with no service in them

Same file, same shape, smaller blast radius — each needs a yes/no on whether
the service's rules apply here at all:

- `CreateEnvironment` (`apply.go:951`, `:1049`) vs `EnvironmentService.Adopt`,
  which exists for exactly this and is used by `prhook` and
  `StackService.Create` but not by the config applier.
- `UpdateStack` (`apply.go:602`, `:963`) vs `StackService.Update`/`Reslug`.
- `CreateDomainResource` / `UpdateDomainResource` / `DeleteDomainResource`
  (`apply.go:439-475`) vs `DomainResourceService.Create`, which owns
  `checkOwner` and the ACME account. `SetACME` is called through the service
  (`:465`); the create is not.
- `SetIntended` (`apply.go:1196`) — no service owns intended vars at all.

### D-7. The nil service guards were dead code — FIXED 2026-09-20

`a.Ops.Tiles != nil` (3 sites), `a.Instances != nil` (3), `a.Ops.Resources
!= nil` (1). `envops.Ops` documented the nil fallback as "which is what tests
want" (`envops.go:44`), and this audit's first pass repeated it: the config
apply tests exercise the raw row delete, not the service path.

**That was wrong, and the truth is worse.** Measured:

- every production construction site sets them —
  `cmd/stackrd/main.go:578-587`, `handlers/web/server.go:769`,
  `handler/project/handler.go:2384`, `api/v1/envs.go:134`; the fifth,
  `main.go:828`, is `writeOpenAPISpec`, which no request reaches
- `go tool cover`: `createTile` **0.0%**, `deleteTile` **0.0%**
- a `panic()` planted in each body did not fire once across the package's 220
  tests

So no test took the fallback either. Every apply test seeds the tile it then
updates, so the create and delete branches of the walk were never taken at
all. The guards protected nothing in either direction.

**Fixed.** The three `Ops.Tiles` guards and their fallbacks are deleted —
`Validate` on create, `Validate` on update, `TearDown` on delete now run
unconditionally — and `stackconf/createtile_test.go` is the coverage that
makes their absence mean something:

- a file declaring a new tile creates it (`createTile` 0% -> 59%)
- a file declaring a tile with `limits.cpu: -1` is **refused**, and the row is
  not written — a rule `TileService` has and the file schema does not, which
  is the point of wiring the service in rather than copying the rule
- a file that drops a tile tears it down through the service
  (`deleteTile` 0% -> 56%)

Left in place: the `a.Instances != nil` and `a.Ops.Resources != nil` guards.
Same dead-code argument applies, but covering the managed-instance path needs
a real `managedtiles.Service`, and deleting a guard whose absence nothing
proves is how the first version of this section got written.

### D-8. Share links are minted outside the service that revokes them

`config/sharelink/sharelink.go:62` creates a secret link directly.
`service.RevokeService` owns revoking them. One concept, two owners, and the
mint side carries no rules.

### D-9. Owed from the plan, still owed

Carried forward from `04-progress.md` "What is still owed" and
`06-points-18-20.md`; none of it closed since:

- Point 15: scope grants at mint time still bind against the active cookie
  org; demoting a user narrows nothing.
- Point 15: revocation on demote/move.
- Point 17 has never had a rig pass. The stated blocker — "the VM has no
  GitHub connector since the pave" — is **wrong**: checked 2026-09-20, the box
  has a connector and three bound stacks, and a plan/approve cycle was driven
  through it while verifying D-5. Nothing blocks this but doing it.
- Five rig cases never driven: promote force, volume attach to a stopped
  service, member-minted org registry credential, deactivating a signed-in
  user, a config apply of a tile the validator refuses.
- The personal routes (`/account/*`, `/notifications/*`, `/orgs/switch`,
  `/cli/authorize`) are outside the verb table, so the route walk can never
  answer for them.
- 10 mutating handlers remain body-gated, 10 GET routes ungateable; both lists
  carry reasons.

---

## Part 2 — surfaces, placed

- **CLI** — covered. `internal/cli` is an HTTP client of `/api/v1` and imports
  neither store nor service. Whatever the API enforces, the CLI inherits.
- **`prhook`** (GitHub webhooks) — covered. Wired with `WithEnvironments`,
  `WithStacks`, `WithTiles`, `WithOrgs`, `WithSettings`, and calls
  `PREnvService.Adopt` / `EnvironmentService.Adopt`.
- **`infra/deploy`** — below the line and staying there. Its 17 writes are
  deployment rows and `UpdateTileStatus`; state only, never config.
- **`infra/*` generally** — below the line per D7: managedtiles, netpool,
  metrics, workqueue, backup, registry, placement all write state or their own
  rows.
- **`config/stackconf` + `config/orgconf`** — **not placed.** This is D-4/D-5.
- **`handlers/web/components`, `handlers/middleware`** — not placed either.
  D-2.

---

## Part 3 — what has no service at all

`repo.Store` has 236 methods. **67 are never called from anywhere under
`service/`.** Dropping the ones that are correctly below the line (work queue,
metrics, deployment rows, provisions/resources, cron runs, tile state setters,
audit), the remainder cluster into owners that were never created:

| concept | store methods with no service | who writes them today |
| --- | --- | --- |
| **connectors** | `CreateConnector`, `UpdateConnector` | `handler/settings`, `githubapp`. `ConnectorService` does exist (`service/connector.go`) — `04-progress.md`'s "no ConnectorService" is stale — but it is `Get`/`ForOrg`/`ListAll`/`Delete` only, so the two writes have no owner |
| **share links** | `CreateSecretLink`, `DeleteSecretLink`, `GetSecretLinkByHash`, `TouchSecretLink`, `BurnDropLink` | `config/sharelink` (D-8) |
| **config plans** | `CreateConfigPlan`, `SetConfigPlanError`, `SupersedePendingPlans` + the three `OrgConfigPlan` twins | `config/stackconf`, `config/orgconf` — `PlanService` exists but owns only reads |
| **node join keys** | `CreateJoinKey`, `GetJoinKey`, `LatestJoinKey`, `BurnJoinKey` | node enrollment handlers; `NodeService` does not own them, nor `CreateServer` |
| **resource bindings** | `CreateBinding`, `DeleteBinding`, `ListBindingsByResource` | `stackconf.syncBindings` only |
| **intended vars** | `SetIntended`, `DeleteIntended`, `ClearDeclaredIntended` | `stackconf` — `VariableService` owns declared vars but not intended ones |
| **environment lifecycle** | `DeleteEnvironment`, `RenameEnvironment`, `SetEnvironmentNetwork`, `SetEnvironmentProxy`, `EnvironmentsWithoutNetwork` | `envops` / `envnet` — `EnvironmentService` exists and does not own its own table's writes |
| **tile vars bulk** | `ReplaceTileVars` | `stackconf`; `VariableService` owns every other write to that table |
| **staged changes** | `CreateStagedChange`, `ListStagedByStack` | `config/staging` — reads were moved into a service, the writes were not |

`EnvironmentService` and `VariableService` not owning writes to their own
tables is the sharpest line in this table: those two services exist, and the
config path goes around them.

---

## What to do, in the order the cost says

1. ~~**D-5's two real gaps**~~ — done 2026-09-20, see D-5. What is left of D-5
   is the delete-then-create id churn on a domain edit.
2. ~~**D-1.**~~ — done 2026-09-20, see D-1.
3. ~~**D-7.**~~ — done 2026-09-20, see D-7. The `Instances`/`Resources`
   guards remain, and need a fake `managedtiles.Service` to close.
4. **Part 3, the two services that already exist** — `EnvironmentService` and
   `VariableService` taking the writes to their own tables. This is the change
   that makes "call the service again" true rather than aspirational.
5. **D-6, and the rest of Part 3.** Concept by concept, each a decision rather
   than a refactor: does config-as-code get to be exempt from that service's
   rules or not.
6. **D-3.** Drop the store field from the 15 handler packages. Biggest, least
   urgent — 64 call sites of lower-layer plumbing, and D-1's test already
   guards the thing that matters.
7. **D-2, D-8.** One file each.
8. **D-9.** Nothing new; the rig work and point 15's leftovers.

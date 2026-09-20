# Total extraction — every store write goes through a service

Agreed 2026-09-20. Follows `09-drift-audit.md`, which found that the handler
surfaces were extracted and the *config* and *infra* surfaces never were.

## The rule being enforced

> Nothing outside `internal/stackrd/service/` writes to `repo.Store`.

Not "handlers don't". Everything. `config/`, `infra/`, `cmd/`, the lot. The
dev's call, made explicitly: "yes everything".

## Why a rule and not a cleanup

A cleanup rots. The audit exists because point 19 moved the handler *reads*
and nothing stopped the next writer from appearing somewhere else. So the
first commit of this plan is the **guard**, not the refactor:
`internal/stackrd/writefree_test.go`, an AST walk in the shape of
`handlers/web/storefree_test.go`, with a seeded allowlist that shrinks and
never grows.

Every wave below is then measurable as "N lines came off the allowlist".

## The measurement

Scanned 2026-09-20 at `6b60ae1`. Raw store writes outside `service/` and
`store/`, excluding `_test.go`:

| tree | sites |
| --- | --- |
| `infra/` | ~90 |
| `config/stackconf` | 33 |
| `config/orgconf` | 20 |
| `config/envops` | 10 |
| `config/sharelink` | 7 |
| `config/staging`, `config/varref` | 2 each |
| `cmd/stackrd` | 5 |
| `handlers/web/registrytoken.go` | 1 |

~190 sites, 42 files.

## Waves

Each wave is one or more commits on `service-extraction`. One PR at the end
of wave 7. Waves are ordered so that each one compiles and is green alone.

### W1 — the guard
`writefree_test.go` + allowlist seeded from the scan. No behaviour change.

### W2 — services that exist and do not own their own table
The sharpest line in the audit. `EnvironmentService` and `VariableService`
both exist; the config path writes `environments` and `variables` around them.

- `EnvironmentService`: `CreateEnvironment`, `UpdateEnvironment`,
  `DeleteEnvironment`, `RenameEnvironment`, `SetEnvironmentNetwork`,
  `SetEnvironmentProxy`
- `VariableService`: `UpsertVariable`, `DeleteVariable`, `ReplaceTileVars`,
  `SetIntended`, `ClearDeclaredIntended`

Callers: `stackconf`, `orgconf`, `envops`, `infra/envnet`.

### W3 — the rest of D-6, config path
`StackService`, `OrgService`, `TileService`, `DomainService`,
`DomainResourceService`, `StorageService` already own these tables. The
config appliers write them raw.

### W4 — concepts with no service at all
- `ConnectorService` gains `Create`/`Update` (9 sites in `stackconf` alone)
- `ShareLinkService` — mint/claim/touch/burn, joining `RevokeService`, which
  already owns revoking them (D-8)
- `PlanService` gains its writes: `CreateConfigPlan`, `SetConfigPlanStatus`,
  `SetConfigPlanError`, `SupersedePendingPlans` + the three org twins
- staged changes fold into `TileService`, which already owns the table
- resource bindings route through `ManagedInstanceService`, which owns them

### W5 — infra/
Included by explicit instruction. Several of these have no rule to enforce and
will come out as forwarders; each one is marked as such in this doc so it can
be deleted later if the dev disagrees.

`DeploymentService` (deployment rows, tile status, home node, digest, shared
net) · `WorkQueueService` · `MetricService` · `NodeService` +servers/join keys
· `RegistryService` +registries/credentials · `SliceService` +provisions ·
`ManagedInstanceService` +resources/outputs/bindings · `StorageService`
+paths · `BackupScheduleService` +runs · `TileLifecycleService` +cron runs ·
`SettingsService` +`SetSetting` · `OrgService` +`CreateOrg`.

### W6 — leftovers
`cmd/stackrd/seed.go`, `cmd/stackrd/main.go`,
`handlers/web/registrytoken.go`.

### W7 — rig
Deploy to the test rig, drive the surfaces, then raise the PR.

## Drift policy for this work

Drift found mid-wave is **recorded here** and resolved by copying how
`service/` already does it. Where there is no precedent, best guess, recorded.
No stopping to ask.

## After the PR: the store tree

Separate stacked PRs, one per top-level accessor — `repo.Users()`,
`repo.Orgs()`, `repo.Tiles()`, … Split by table first, sub-split only where a
table's method set clearly fans out. Method names standardised on the way
(`GetByID`, `List`, `Create`, `Update`, `Delete`).

The flat methods stay as delegates during the migration; the last PR deletes
the shim. `storefree_test.go` and `writefree_test.go` are keyed on call
syntax, so each tree PR must rewrite its guard in the same commit — a guard
that silently stops matching is worse than no guard.

## Drift log

(appended as found)

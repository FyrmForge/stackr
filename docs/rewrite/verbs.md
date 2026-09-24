# Orchestrator verbs

Every v1 scope bullet in REWRITE.md maps to `*service.Orchestrator` methods
below (files under `internal/service/`). Step 4 handlers call these and
nothing else. Signatures: `go doc -all ./internal/service Orchestrator`.

## Conventions for step 4

- **Authorization is the middleware's, never a handler's.** Verbs take ids
  and trust them. The one middleware (step 1) does all of this: load a
  `Principal` (`SessionPrincipal` / `KeyPrincipal`), resolve the route with
  `Resolve(org, stack, env, tile)`, check `authz`, then call the verb with
  the resolved ids. Two exceptions: `MintKey` reads the minter's live role
  (B36), and `Params(scope, secrets)` only reads secrets when the caller
  passes `secrets=true` (B37).
- **Container work returns a `Job`.** Deploy, promote, restart, backup and
  the rest queue a job row and return at once (B25). Handlers answer 202
  with the job id. `GetJob` / `PollJob(id, offset)` follow it, and
  `CancelJob` stops it.
- **Verdicts come back in results.** `PlanPromote` returns
  `PromotePlan{Plan, CanDeploy}`. `CanDeploy` is false exactly when
  `Plan.Blockers` is non-empty. A `Promote` of a blocked plan fails its job
  with the same blocker text (B20).
- **Updates are read-modify-write in the service.** `UpdateTile(id,
  edit func(*Tile) error)` applies only what the handler sets (B1, B21).
- **Errors are `errs`.** `ErrNotFound`, `Invalid` (with a field),
  `Conflict`, `Refused` and `ErrBusy` map to HTTP codes. `Webhook` also
  returns `service.ErrBadSignature` (401) and `service.ErrBadPayload` (400).

## By v1 scope bullet

| v1 scope | Methods |
|---|---|
| Deploy from git and from an image; image watch, digest and tag-policy modes | `CreateTile`, `UpdateTile`, `RenameTile`, `DeleteTile`, `Deploy`, `TileStatus`, `Tiles`, `CheckImages`, `Images`, `TileJobs`; watch sweep on the scheduler |
| Environments, promote, rollback, releases | `Envs`, `Ladder`, `CreateEnv`, `RenameEnv`, `SetEnvFrom`, `SetEnvColor`, `SetEnvSettings`, `ReorderEnvs`, `DeleteEnv`, `Releases`, `Release`, `PlanPromote`, `Promote`, `Rollback` |
| Config-as-code (`stackr-compose.yml`) | `SetConfigRepo`, `Webhook` (push → release → auto promote) |
| PR environments | `Webhook` (pull_request opened/reopened/synchronize/closed) |
| Param store | `Params`, `SetParams` (merge, B4/B35), `DeleteParam` |
| Domains, automatic TLS, admin extra Caddy config | `Domains`, `AttachDomain`, `UpdateDomain`, `DetachDomain` (`DomainSpec.RawCaddy` is admin-only), `SetReservations`, `SyncProxy` |
| Orgs, users, the two roles, API keys | `Register`, `Login`, `Logout`, `SessionPrincipal`, `KeyPrincipal`, `Resolve`, `Orgs`, `AllOrgs`, `CreateOrg`, `FinishOrg`, `Members`, `SetRole`, `RemoveMember`, `Invite`, `Invites`, `LookupInvite`, `AcceptInvite`, `RegisterInvited`, `Users`, `SetAdmin`, `DisableUser`, `ChangePassword`, `SetPassword`, `MintKey`, `Keys`, `RevokeKey` |
| Volumes | `Volumes`, `DeclareVolume`, `DeleteVolume` |
| Managed tiles: Postgres and S3, slices, bindings | `CreateManagedTile`, `ManagedInstances`, `SetInstanceScope`, `Slices`, `AttachSlice`, `DetachSlice` (slices also come from the stack file on promote) |
| Git connectors (GitHub App) | `Connectors`, `BeginConnector`, `CompleteConnector`, `RenameConnector`, `DeleteConnector`, `Webhook` |
| Registry credentials | `Credentials`, `CreateCredential`, `UpdateCredential`, `DeleteCredential` |
| Container logs and restart | `Logs`, `FollowLogs`, `Terminal`, `RestartTile`, `StopTile`, `StartTile` |
| Cron and function tiles (step 3b) | `RunTile` (row first, then the job), `PauseTile`, `Runs`, `Run`, `StopRun`, `RunLog`, `FollowRunLog`; `TileStatus` adds `LastRun`, `NextRun`, `Paused`; `StopTile` on a cron pauses, on a function refuses; `RestartTile` and `StartTile` refuse both; cron ticks on the scheduler |
| Zero-downtime deploys and replicas | `Deploy` (overlap rollout and health gate in flow/deploy; `Tile.Replicas`) |
| Backups | `BackupDests`, `CreateBackupDest`, `UpdateBackupDest`, `DeleteBackupDest`, `BackupMethods`, `BackupSchedules`, `AddBackupSchedule`, `UpdateBackupSchedule`, `DeleteBackupSchedule`, `BackupNow`, `BackupRuns`, `RestoreBackup` (cross-volume), `PanelBackups`, `PanelBackupNow`; scheduled runs and orphan retention on the scheduler |
| Installer and self-upgrade | `Version`, `CheckUpgrade`, `Upgrade`; `service.RunPanelSwap` (`stackrd upgrade-swap`); `service.ProxyAdmin` (`stackrd proxy`) |
| API and CLI first | all of the above |
| Settings: one catalogue | `Settings` (the catalogue), `Setting`, `SetSetting`, `SettingDefaults`, `SetSettingDefaults`; the cascade rungs are `SetStackSettings`, `SetEnvSettings`, and tile fields through `UpdateTile` |
| Org delete and rename rules | `RenameOrg` (squat check over every domain), `DeleteOrg` (has stacks, last org) |
| Tile-to-tile traffic (step 3c) | `Traffic(env) []Edge{From, To, BPS}` (the env's lanes at the last sample; ends are tile ids, slice (provision) ids, `proxy`, `internet`), `TrafficSeq` (sample counter the env events stream watches); the 5 s conntrack sample runs inline on the scheduler, never as a job (a read) |
| (stacks) | `Stacks`, `CreateStack`, `RenameStack`, `DeleteStack` |
| (jobs) | `GetJob`, `PollJob`, `Jobs`, `TileJobs`, `CancelJob` |
| (health) | `Ping`, `Sessions` |

## Job kinds and lock sets

A newer job of the same kind supersedes a queued older one when the older
lock set is inside the newer one. Any overlap between lock sets
serialises the jobs.

| Kind | Queued by | Lock set |
|---|---|---|
| deploy | `Deploy`, redeploy after an edit, param or settings change | tile |
| promote | `Promote`, `Rollback`, push/watch auto | `env:<id>` + the env's tiles |
| push | `Webhook` push | `push:<stack>`, `push:<stack>:<repo>@<branch>` |
| pr | `Webhook` pull_request | `push:<stack>`, `pr:<stack>:<n>` |
| delete | `DeleteTile` | tile |
| restart, stop, start | tile verbs | tile |
| attach, detach | slice verbs | consumer (+ instance tile) |
| backup | `BackupNow`, schedules | `volume:<id>` |
| restore | `RestoreBackup` | `volume:<target>` |
| orphans | daily cron | `orphans` |
| panel-backup | `PanelBackupNow` | `panel-backup` |
| upgrade | `Upgrade` | `panel` |
| run | `RunTile`, a cron tick, a deploy or promote of an on_deploy function | tile, `run:<id>` (never supersedes; no 30-minute cap, the tile's `timeout_minutes` instead) |
| imagewatch | `CheckImages`, the interval sweep | `imagewatch`, `imagewatch:<stack>/<tile>` (`imagewatch:all` for the sweep) |

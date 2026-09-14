# Feature: Backups v2

> **Status (2026-08-18): a simpler system than this design shipped.**
> Live today in `internal/stackrd/infra/backup`: per-engine logical dumps +
> volume tar (stop-then-tar or live), **S3 destinations only**
> (`backup_destinations` / `backups` / `backup_runs` tables — in
> `001_initial` after the migration squash; `024_drop_backups` no longer
> exists), schedules with CRON_TZ, last-N retention (`KeepLatest`), restore
> incl. fresh-volume swap, and every route gated through
> `infra/backup/authz.go ResolveDestination` — the ownership check whose
> absence killed v1. UI: per-tile backups + `/admin/backups`.
> **Still unbuilt from this design:** local primary store + free-space
> guard, rsync/add-only sync tier, age encryption, sha256/verify, cascading
> settings, last-N-days retention, per-tile lock, the `system` unit and
> `stackr restore-system`. The rest of this doc is the roadmap for that half.

Ground-up redesign. The previous implementation (`internal/backup/`, `s3_destinations`,
db-panel backup UI) was removed for a cross-tenant restore hole — its routes never
checked tile ownership. This design replaces it entirely.

## Summary

Backup and restore for managed tiles (databases) and volumes, plus stackr itself.
Local-first storage with optional sync to remote targets, per-unit encryption,
cascading settings, all routes authed and org-scoped.

## Model

### Backup modes (exactly two)

| Mode | Applies to | How |
|---|---|---|
| Logical dump | postgres, mysql, mariadb, mongo | engine dump tool via `runtime.ExecStream`, streamed to store; DB stays up |
| Volume tar | volume tiles, redis, s3/RustFS tiles | `tar -c` of the docker volume via the helper-container pattern (`internal/stackrd/infra/runtime/runtime.go` volume file ops) |

Pinned engine commands (consistency is in the flags — do not improvise):

| Engine | Backup | Restore |
|---|---|---|
| postgres | `pg_dump --clean --if-exists` | `psql --single-transaction -v ON_ERROR_STOP=1` |
| mysql | `mysqldump --single-transaction --routines --triggers --events` | `mysql` |
| mariadb | `mariadb-dump --single-transaction --routines --triggers --events` | `mariadb` |
| mongo | `mongodump --archive` | `mongorestore --archive --drop` |

Tar-mode consistency:

- **redis**: `BGSAVE`, poll `LASTSAVE` until complete, then tar. Never tar mid-write.
- **other running engines / volumes**: per-tile toggle — **stop-then-tar** (brief
  downtime, clean snapshot) or **live tar** (no downtime, crash-consistent only;
  labelled as such in the UI). Default: stop-then-tar for s3/RustFS tiles, live
  for plain volume tiles.

### Units (top-level namespaces in the store)

- one unit per **org**
- **`shared`** — cross-org singleton/managed tiles (future; reserved now)
- **`system`** — stackr itself:
  - sqlite DB via `VACUUM INTO` (safe online snapshot)
  - the **`internal/stackrd/config/secrets` master key** — without it a restored DB is full of
    undecryptable credentials; it ships inside the encrypted system backup
  - any other files needed to come back from zero (enumerated at implementation)

Store layout: `<unit-id>/<stack-id>/<env-id>/<tile-id>/<timestamp>.<dump|tar>.age`
(system unit: `system/<component>/<timestamp>...`). **Every path segment is an ID**,
never a name — renames at any level must not orphan history. Names appear only in UI.

### Primary store (choose one per install)

- **Local path** — default `/var/lib/stackr/backups`
- **S3** — any endpoint (internal RustFS or external), one S3 client library

Local store guard: before each run, check free space (configurable minimum,
default e.g. 10%); refuse and alert rather than fill the disk the databases
live on. Store size is surfaced in the UI.

### Sync tier (local primary only)

Replicates the local store outward. Targets:

- **rsync over ssh** — stackr generates the keypair, user installs the public key remotely
- **S3** — same client code as the S3 primary store

**Sync is add-only: it never deletes on the remote.** Each sync target has its
own retention setting (e.g. keep 30 days remote vs 7 local); stackr prunes the
remote separately by that policy. A wiped local store therefore cannot cascade
to the offsite copy.

NFS/SMB: no code — mount the share, point a path at it (documented, not implemented).

### Encryption

`filippo.io/age` (one dep, no CGO). Configured per **unit**:

- **keypair** (X25519) — public key encrypts at backup time; private key shown
  once at setup for offline storage, required on restore
- **passphrase** (scrypt recipient) — stored server-side encrypted via
  `internal/stackrd/config/secrets`, so backup *and* restore are unattended. Explicit posture:
  this protects the offsite/remote copy, not the VM itself — the server can
  decrypt. Keypair mode is the stronger option.
- **off**

Same streaming wrapper for both modes.

### Integrity

- `backup_runs` records **sha256 + size** for every artifact, computed while streaming
- Restore verifies checksum before touching anything
- Optional periodic verify job: re-read newest artifact per tile from the store
  (and spot-check sync targets), compare checksums, alert on mismatch

### Settings cascade

Schedule, retention, encryption, tar consistency toggle, enabled — set at unit
level, overridable at stack → env → tile. Most specific wins.

### Scheduling

Through `internal/stackrd/infra/jobs/jobs.go` (existing cron machinery: validation, overlap
guard, CRON_TZ, run history, failure notifications). No second scheduler.
**Sync, remote-prune, and verify failures notify through the same path** as
backup failures — no silent legs.

### Retention

One choice per unit/tile: **last N backups** OR **last N days**. Prune after each
successful backup. **The newest successful backup is never pruned**, regardless
of policy — N days of failed backups must not delete the last good one.

### Concurrency

One **per-tile lock** spans backup, restore, prune, and verify; sync takes a
store-level read snapshot (syncs completed artifacts only, skips in-progress).
A manual restore cannot interleave with a scheduled backup on the same tile.

### Restore

- **Dump**: stream into the engine restore cmd (flags above); DB stays up.
  Postgres terminates other connections to the target DB first
  (`pg_terminate_backend`) so `--clean` doesn't deadlock on locks.
- **Tar**: download → decrypt → checksum-verify → untar into a **fresh volume**
  → stop container(s) → swap mount → start. The old volume is kept until the
  swap succeeds (then removed), so a corrupt archive or wrong key can never
  destroy the live data.
- Restore only from the same tile's history; no cross-tile restore (yet)
- **Disaster recovery**: a CLI bootstrap command —
  `stackr restore-system --store <path|s3-url> --key <age-key>` — runs with no
  server and no DB: fetches the system artifact, decrypts, restores sqlite +
  secrets master key into place. Then start stackr and restore tiles from the UI.

### Security

- All backup routes behind existing auth middleware; **every route resolves the
  tile/unit and checks the caller's org membership** (the old implementation's
  hole); `shared`/`system` units admin-only
- A route-audit test asserts no backup route is reachable unauthenticated
- All credentials (S3 keys, ssh private key, passphrases, held age keys)
  encrypted at rest via `internal/stackrd/config/secrets` (same as `tiles.env`)

## Acceptance Criteria

- [ ] Dump backup + restore for postgres/mysql/mariadb/mongo with the pinned flags
- [ ] Tar backup + restore for volume tiles, redis (BGSAVE-gated), s3 tiles (stop-then-tar)
- [ ] Tar restore swaps volumes; live data survives a corrupt archive / wrong key
- [ ] Local path primary store (default `/var/lib/stackr/backups`) with free-space guard; S3 primary alternative
- [ ] Add-only sync to rsync-over-ssh and S3 targets, independent remote retention
- [ ] age encryption per unit (keypair / stored passphrase / off)
- [ ] sha256 + size recorded per run; restore refuses on checksum mismatch
- [ ] Cascading settings unit → stack → env → tile
- [ ] Schedules via `internal/stackrd/infra/jobs`; backup, sync, prune, verify failures all notify
- [ ] Retention: last-N or last-N-days; newest successful backup never pruned
- [ ] Per-tile lock across backup/restore/prune/verify
- [ ] `system` unit includes sqlite snapshot + secrets master key; `stackr restore-system` brings up a fresh VM from store + age key alone
- [ ] Zero unauthenticated backup routes; every route checks tile/unit ownership (route-audit test)

## Technical Notes

- Store interface (put/get/list/delete) with local-fs and S3 impls; sync
  runner; age stream wrapper; checksum tee — extending the shipped
  `internal/stackrd/infra/backup`.
- Engine dump/restore commands stay with the engines in
  `internal/stackrd/infra/managedtiles` (redis gains tar-mode restore, s3
  tiles gain tar-mode backup — via mode selection, not new engine commands).
- New migration (shipped tables differ — see status header): `backup_settings`
  (scope-keyed cascade rows), sha256 + size columns on `backup_runs`,
  `backup_targets` (sync targets + per-target retention).
- Repo layer: follow `internal/stackrd/store/repo/sqlite` generic
  `get[T]`/`list[T]` pattern.
- Handlers: extend `internal/stackrd/handlers/web/handler/backups/` with the
  unit drill-down views.
- CLI: `restore-system` subcommand in `cmd/stackr`.

## UI/UX

- Admin: primary store config (+ size / free-space display), sync targets with
  per-target retention, `system`/`shared` unit settings
- Org settings: unit-level schedule/retention/encryption
- Drill-down: unit → stack → env → tile, each level shows effective (inherited)
  settings + override control
- Tile panel: backup history (status, size, checksum), run-now, restore buttons;
  live-tar tiles labelled crash-consistent

## Testing

- Unit: store impls (local/S3), age wrapper round-trip, cascade resolution,
  retention pruning (incl. never-prune-last-good), checksum verify, per-tile lock
- E2E: backup+restore round-trip per engine against real containers; tar restore
  volume-swap incl. corrupt-archive abort; redis BGSAVE gate; route auth audit;
  `restore-system` from a scratch data dir

# Disaster recovery: where stackr stands (investigation only)

Status: investigated 2026-10-07, parked by darthvader ("we won't be doing
recovery just yet"). Nothing here is planned or built. [C] traced in code,
[P] plausible.

## Today, step by step
1. Archive: panel archives go to the local dest only, manual or pre-upgrade,
   keep 14 (`internal/service/backup.go`). The box dies, the archive dies.
   Local-dest volume backups too; only S3-dest runs survive. [C] (ready-blitz
   R1 fixes the panel half.)
2. Restore needs a prior install (`cmd/stackr-install/restore.go:42`), so a
   full install with a throwaway admin first. With no `--passphrase` it
   decrypts with the NEW box's key and fails; the archive is sealed with the
   OLD master key (stackrd never sets a separate passphrase). `install.json`
   and the DNS-01 token are not in the archive. No fetch from S3. Restore
   leaves the fresh proxy with an empty `stackr-caddy`. [C]
3. Panel boot: no reconcile. `ReopenIngress` and a proxy push only; every
   tile stays not running until deployed. Queued and waiting jobs from the
   archive run on the new box. No "redeploy everything" verb; re-promoting
   an env to its own release redeploys pinned tiles but skips managed and
   slice tiles (no pin). [C]
4. Images: git-built images are local only; `Images.Ensure` returns "" for
   built rows without checking the box, so deploy fails "No such image".
   No rebuild path except a new push. Pulled images re-pull by digest. [C]
5. Data: one restore job per volume, names match (from the DB). A dump
   restore needs the engine running, so consumers can migrate an empty DB
   first; nothing orders it. Runs newer than the archive are invisible (no
   bucket rescan). [C]
6. Top hazard: `sched.Boot` loads the restored schedules, so backups of
   EMPTY volumes run and `Prune` keeps the newest N, pushing good archives
   out. [C]
7. Shares re-mount by themselves; host paths are silently created empty if
   missing (legacy Binds). Host grants return with the DB. [C]
8. DNS stays on the dead IP (no record automation); Caddy tries every
   certificate at once until it moves. GitHub App works once DNS moves;
   pushes during the outage are lost. [C]/[P]
9. Secrets: master key in the archive unseals params, connector secrets,
   registry creds, dest keys. The archive's image must exist on ghcr. [C]

Roughly 8 fixed steps plus about 4 per env and 2 per volume; 60 to 80
manual actions for three stacks with two envs, with easy data mistakes.

## Smallest feature set, in order
0. Server file: `stackr server export` from a healthy box (or the bound
   repo's `stackr-server.yml`) is the config half of the rebuild; on a fresh
   box `stackr server apply <file>` recreates settings, routes, destinations,
   connector shares and orgs, then each org file rebinds.
1. Recovery hold: a restored panel boots with the scheduler held until
   converge finishes or an admin releases it. (`restore.go`,
   `orchestrator.go`, `flow/schedule`, `admin.go`)
2. `stackr-install restore --from <presigned URL> --fresh` (no prior
   install, answers from flags, `--passphrase` as the key, starts the proxy
   with the archived caddy tree).
3. One-shot Converge job: per env with a release, in ladder order, managed
   tiles first, restore each volume's latest run before its holder starts,
   provision slices, then the not-deployed-yet rollout including managed and
   slice tiles; then release the hold.
4. Rebuild on missing image at the pin's commit into the same ref and row.
5. VIP rebuild at boot (live bug, below).
6. Backup correctness (live bug, below) and a bucket rescan.
7. Optional Cloudflare A-record flip; recommend keeping DNS manual.

## Warm standby on top
A passive second box that pulls and restores the newest archive every N
minutes with the scheduler held, images pre-pulled; promotion = release the
hold, converge, flip DNS. Must stay fully passive: `InstallID` is always
"default", so a live standby would write to and prune the primary's prefixes.

## Live bugs found on the way (not DR-only)
- VIP rules are never rebuilt at boot: `vip.Rebuild` has no caller though
  `leaf/tile/world.go:224` says stackrd runs it. After a panel restart or
  upgrade the first tile deploy redeclares `STACKR-VIP` and wipes every other
  tile's VIP jump; after a host reboot all VIP rules are gone until each
  tile redeploys. Service names then hit a pause container with nothing
  behind it. [C]
- Managed Postgres `dump` backups (the default method) run `pg_dump` on the
  admin DB only (`flow/managed/postgres.go:268-272`); slices are separate
  databases, so the app data is not in the dump. Only the volume method is
  a real backup today. [C]

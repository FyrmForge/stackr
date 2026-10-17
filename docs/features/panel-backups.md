# Feature: Panel backups on a schedule

## Summary
The panel archives itself on a cron schedule and can send the archive off the box.
Admin drawer, Backups tab: schedule, keep and destination, plus the last run.

## Settings
- `panel_backup_enabled`: on by default; the switch for the schedule.
- `panel_backup_schedule`: five-field cron, `CRON_TZ=` allowed, default `0 3 * * *`. Empty means the default time.
- `panel_backup_keep`: scheduled archives kept at the destination, default 14.
- `panel_backup_dest`: id of a global backup destination; empty means local. A destination that
  was deleted falls back to local with a warning in the job log; deleting the one named here is refused.

Each archive sits under a prefix for what started it: `scheduled`, `manual` (the button) and `upgrade`
(taken before a self-upgrade). Only `scheduled` follows `panel_backup_keep` and the destination; `manual` and
`upgrade` stay local and keep the newest 14 each, so a scheduled run never deletes the pre-upgrade archive.
The runs list marks a scheduled archive `schedule`. A scheduled and a manual run do not cancel each other, and
neither runs during an upgrade.

The install id (random, made by the installer, kept in `install.json` and set as `STACKR_INSTALL_ID` on the
panel) keeps two boxes on one bucket apart. An install made before it keeps the id `default`.

## What is in the archive
`stackr.db`, `keys/master.key`, `VERSION`, and `caddy.tar.gz`: the proxy volume
(`stackr-caddy`: certificates, their keys, ACME accounts). The proxy volume is there so a restore on
a new box does not re-issue every certificate (Let's Encrypt rate limits). If the volume cannot be
read, the archive is still written without it and the job log says so.

## Restore
The archive is encrypted with the recovery passphrase (default: the box's master key).

1. Local archives are under `<data dir>/backups/stackr/_panel/<install id>/<scheduled|manual|upgrade>/`. On S3,
   the object key is `stackr/_panel/<install id>/<scheduled|manual|upgrade>/<utc timestamp>-panel.tar.gz.age`.
2. On the host: `stackr-install restore <archive | s3://bucket/key>`. An S3 key ending in `/` is a prefix: the
   newest archive under it (by its timestamp, across trigger dirs). The S3 keys come from
   `STACKR_BACKUP_ENDPOINT`, `STACKR_BACKUP_REGION` (default `us-east-1`), `STACKR_BACKUP_ACCESS_KEY` and
   `STACKR_BACKUP_SECRET_KEY`; the download sits in the data dir (0600) and is deleted after.
3. The passphrase: `STACKR_RECOVERY_PASSPHRASE`, or the first line of stdin with `--passphrase-stdin`, or
   `--passphrase P` (shows in `ps`). With none, the box's master key; when that does not open the archive and
   stdin is a terminal, a prompt asks for it.

Restore stops the proxy, keeps a copy of the current `stackr-caddy` tree, wipes and refills it from the
archive (the copy goes back if the untar fails), starts the proxy, then starts the panel on the archived
build. The replaced database is kept as `<data dir>/stackr.db.before-restore-<time>`; the staging dir, which
holds unencrypted certificate keys, is deleted once the restore went through. An older archive without
`caddy.tar.gz` restores as before.

Restore leaves `<data dir>/restored`. The panel's next boot queues a normal deploy job for every tile of an env
on the controller (its placement's node too) that has no container and whose newest finished deploy, promote,
env sync, start or restart is newer than any stop, then removes the file. A worker's tiles, stopped tiles,
never deployed ones and envs mid move are left alone; crons come back through the scheduler.

Those deploys wait for the env's volumes (managed instances' too) to come back from their newest finished
backup run. Only a volume that holds no data (gone or measured empty) is restored, so one that survived is
never rolled back. A tar backup is restored first, holding every tile to deploy; a dump backup is loaded
right after its instance's deploy, holding the instance's consumers. A volume with no backup starts empty:
each deploy's job log opens with `warning: no backup for <volume>; it starts empty`, and an `env.restore`
row in the security log names it. Not covered: a backup on the old controller's local destination (it went
with the box; S3 destinations work, their keys are in the database), writes after the newest backup, and
volumes of envs whose tiles are all stopped or never deployed.

When the panel runs this installer's own build, restore mints a new API key for the oldest active stackr admin
(`stackrd admin-key`, key name `restore`) and writes `<data dir>/cli.json` with it, as the install does. The
old key lived in the replaced database's world, and a new box has no cli.json; it is never in the archive.
The admin's earlier `installer` and `restore` keys are revoked in the same transaction, a `key.revoke` row each.

At boot the panel clears `<data dir>/backups/scratch`, where a crash can
leave unencrypted spool files.

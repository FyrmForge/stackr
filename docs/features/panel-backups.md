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

1. Fetch the archive. Local: `<data dir>/backups/stackr/_panel/<install id>/<scheduled|manual|upgrade>/`. On S3,
   the object key is `stackr/_panel/<install id>/<scheduled|manual|upgrade>/<utc timestamp>-panel.tar.gz.age`;
   download the newest, for example
   `aws s3 cp --endpoint-url <endpoint> s3://<bucket>/<key> panel.tar.gz.age`.
2. On the host: `stackr-install restore [--passphrase P] panel.tar.gz.age`.

Restore stops the proxy, keeps a copy of the current `stackr-caddy` tree, wipes and refills it from the
archive (the copy goes back if the untar fails), starts the proxy, then starts the panel on the archived
build. The replaced database is kept as `<data dir>/stackr.db.before-restore-<time>`; the staging dir, which
holds unencrypted certificate keys, is deleted once the restore went through. An older archive without
`caddy.tar.gz` restores as before. At boot the panel clears `<data dir>/backups/scratch`, where a crash can
leave unencrypted spool files.

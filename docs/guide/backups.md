# Backups

Two things are backed up: the panel itself, and the data volumes of your apps.

## Destinations

With no destination, archives stay on the controller under its data dir (`/var/lib/stackr/backups`). That is the same box. Add an S3 destination to get them off it.

1. Admin, Backups, add a destination. It takes an endpoint, region, bucket and keys. Saving writes and deletes a test object, so a wrong endpoint or key is refused at once.
2. Or from the CLI, which prompts for the keys:

   ```bash
   stackr dest add offsite --server --shared --endpoint https://s3.example.com --region eu --bucket stackr-backups
   ```

An org's destination on a private address (a LAN bucket) is saved but waits. Backups to it fail with "Waiting for the server admin to approve LAN access." until an admin approves its host and port under Admin, Host access (or `stackr admin dest-grant-approve <dest>`). Changing the endpoint asks again. An admin can revoke it there too (or `stackr admin dest-grant-revoke <dest>`). A destination a server admin saves is approved on the spot, and so is a tile a server admin deploys.

## The panel

The panel archives its database, master key and proxy certificates on a schedule. The default is daily at 03:00, keeping 14.

1. Admin, Backups, Stackr's own database. Set the schedule, how many to keep and the destination.
2. To run one now, pick its destination next to Back up now (the scheduled one is the default), or:

   ```bash
   stackr admin panel-backup --dest offsite
   stackr admin panel-backups
   ```

3. A copy is also taken before every upgrade.

The pre-upgrade copy stays on the controller, so copy one off the box yourself before you wipe it. The history and `panel-backups` name where each run went.

The archive is encrypted to the recovery key. The installer shows it once, and the box keeps only its public half, so keep the key off the box. If a panel boots without one, it makes one and Admin, Backups shows it once behind your password, with an "I've saved it" button. The warning stays until you confirm.

To change the key, use Rotate the key in Admin, Backups. It asks for your password and shows the new key once. Older archives still need the key they were made with. The archive's name says which: `...-panel-g2.tar.gz.age` needs generation 2.

### What the archive holds

The database, the master key, the cluster and controller keys and the proxy certificates. It leaves out on purpose:

- `install.json`: a restore starts from an install on the new box, with that box's own answers (addresses, bridge subnet).
- Registry images: a tile that builds from its repo is built again when the restored panel redeploys it. An image you pushed to the registry yourself has to be pushed again.
- Job logs, metrics and logs: history, not state. They start empty.
- Local backups: they are volume data on the old disk. Use an S3 destination for anything that has to outlive the box.

The recovery key is never in the archive; keep it off the box.

### Restore the panel

1. On a new box, install first with the same flags as the old one (see [Install](install.md)). Restore needs that install's answers.
2. On the controller, as root, from a local archive (they are under `/var/lib/stackr/backups/stackr/_panel/`):

   ```bash
   read -rs STACKR_RECOVERY_KEY && export STACKR_RECOVERY_KEY
   stackr-install restore panel.tar.gz.age
   ```

3. Or straight from S3. A key ending in `/` takes the newest archive under it. Name the old install's id (it is in the old box's `install.json`), or stop at `_panel/`: restore takes the only install there, or lists the ids to choose from:

   ```bash
   export STACKR_BACKUP_ENDPOINT=https://s3.example.com STACKR_BACKUP_REGION=eu
   read -rs STACKR_BACKUP_ACCESS_KEY && export STACKR_BACKUP_ACCESS_KEY
   read -rs STACKR_BACKUP_SECRET_KEY && export STACKR_BACKUP_SECRET_KEY
   stackr-install restore s3://stackr-backups/stackr/_panel/<old install id>/
   ```

The recovery key can also come on stdin with `--key-stdin`, or at the prompt in a terminal. There is no flag for it, other users on the box would see it in `ps`. If the key does not open the archive, restore says which key generation the archive needs.

After a restore the key you restored with stays the box's recovery key, so it opens every new backup too. The key the fresh install printed is dropped. A box that rotated its key after the archive was made keeps its newer one, and restore says so. Restore also brings back the old install's id, so new backups land under the same `_panel/<install id>/` prefix and old scheduled ones keep being pruned. It removes a leftover `master.key.next`.

The old database is kept as `stackr.db.before-restore-<time>`, with its `-wal` file beside it. Workers reconnect by themselves and keep their tiles running.

On its first boot the restored panel deploys again every tile on the controller that ran before and has no container now. Before those deploys it restores each of their env's empty volumes, managed databases too, from the newest finished backup. The restore jobs run first and the deploys wait for them, so no app starts on an empty volume. Tiles you had stopped stay stopped.

The newest backup has to be reachable from the new box. A backup on an S3 destination is, since the destination's keys come back with the database. A backup on the old controller's local disk went with it. A volume with no backup starts empty, and its deploy log and the security log say `no backup for <volume>; it starts empty`. Anything written after the newest backup is lost. A volume that still holds data is left alone.

The CLI on the controller is logged in as the admin again: restore makes a new API key and writes `/var/lib/stackr/cli.json`, on the admin's first org (the oldest one they are a member of, else the oldest org). The admin's older `installer` and `restore` keys are revoked, and the security log records each one.

## Volumes

Back up one volume now, list its runs, and restore one:

```bash
stackr backup run <volume-id>
stackr backup runs <volume-id>
stackr backup restore <volume-id> --run <run-id>
```

`stackr backup ls <volume-id>` lists its schedules.

## Point-in-time recovery

A postgres instance can keep its write-ahead log (WAL) and a weekly base backup, so the archive covers every moment of the last N days, not only the nightly dump. It is on (7 days) for a new instance when the org can use a destination that is not this server's disk, the rule the daily backup follows; otherwise it is off and opt-in. With only the local disk the archive shares the box with the data: it covers a bad migration, not a lost server.

Postgres writes each finished WAL segment (at least one a minute while the database is busy) to a second volume, `<instance>-wal`. The panel ships what is there every minute, encrypted with the destination's key, to the destination of the instance's weekly base backup, and then deletes it from the box.

```bash
stackr tile pitr pg --days 7     # on; restarts the database once
stackr tile pitr pg --days off   # off; archived objects stay on the destination
stackr tile backups pg           # restorable range, last base backup, archive size
```

In the panel, the instance's Backups tab shows the same and has the switch. Turning it on adds a weekly `base` schedule next to the daily dump (edit its time like any schedule) and takes the first base backup as soon as the instance is up. Base backups and WAL sit under `stackr/<org>/<data volume id>/pitr/` on the destination. After each good base backup, bases older than the window are removed, keeping the newest one outside it, with the WAL only they needed. You cannot restore to before PITR was turned on.

In a stack file the window is `pitr_days` on the managed tile (`0` is off; leave it out and the file does not touch it), and `stackr stack export` writes it. Changing it restarts the instance once.

### Restore to a time

A time is RFC3339 with an offset (`2026-11-07T10:42:00+01:00`, or `Z`) or `latest`. It must fall between the oldest restorable moment and the last shipped WAL; the error names both. Restoring takes the newest base backup finished before the time and replays the shipped WAL up to it, so it needs free disk on the instance's node of 1.2 times the base and the WAL it reads, and refuses with the numbers when there is less.

```bash
stackr tile restore pg --at 2026-11-07T10:42:00Z   # a new instance pg-restored-20261107-1042; pg is not touched
stackr tile promote-restore pg-restored-20261107-1042
stackr slice branch shop-db --at 2026-11-07T10:42:00Z   # one slice as it was then
```

`tile restore` makes a real instance tile next to the source (same env and image, its own volume, no slices, no bindings, no backups, no archive) and keeps it: look at it with `logs`, `exec` and `forward`, delete it when done. In the panel it is the **Restore to a time** form on the Backups tab. It needs write access on the source's stack.

`tile promote-restore` puts the copy's data under the source: the source stops, its data volume now holds the copy and restarts with the same address and passwords, every slice and binding is applied again (what existed at the restored time is untouched, what was made after it comes back empty with its current login), the copy's tile goes, the archive state is reset and a base backup is queued. The old data stays as an orphaned volume until orphan retention ends, so a promote can be undone by restoring from it. A slice database dropped after the restored time reappears in the instance with no slice tile for it. The Promote button is on the copy's Backups tab.

`slice branch --at` restores the whole instance to the time in a scratch copy, copies the one slice's database out of it into the new branch, and deletes the scratch copy and its volume, all in one job. A time within two minutes of now is refused: branch without `--at`. Retention never prunes an archive while a restore reads it.

Archiving refuses to start into a destination path that already holds objects the panel has no backup of, so a copy of an instance never writes into the history of the one it came from. The Problems banner shows `pitr.lag` and `pitr.disk` (see [Alerts](alerts.md)).

## Back up a node

This backs up every volume on a worker, one env at a time, as one group.

1. Admin, Nodes, open the node, **Back up node**. Or:

   ```bash
   stackr node backup vps-fsn1
   ```

2. Each env's apps stop while their volumes are copied, then start again. Plan for that downtime.
3. List the results:

   ```bash
   stackr node backups vps-fsn1
   ```

   It shows each group, its status, its destination and every volume's result.

Set a destination first. With none, the archives stay on the controller.

# Backups

Two things are backed up: the panel itself, and the data volumes of your apps.

## Destinations

With no destination, archives stay on the controller under its data dir (`/var/lib/stackr/backups`). That is the same box. Add an S3 destination to get them off it.

1. Admin, Backups, add a destination. It takes an endpoint, region, bucket and keys. Saving writes and deletes a test object, so a wrong endpoint or key is refused at once.
2. Or from the CLI, which prompts for the keys:

   ```bash
   stackr dest add offsite --server --shared --endpoint https://s3.example.com --region eu --bucket stackr-backups
   ```

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

The old database is kept as `stackr.db.before-restore-<time>`. Workers reconnect by themselves and keep their tiles running.

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

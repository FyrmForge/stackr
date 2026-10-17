# Backups

Two things are backed up: the panel itself, and the data volumes of your apps.

## Destinations

With no destination, archives stay on the controller under its data dir (`/var/lib/stackr/backups`). That is the same box. Add an S3 destination to get them off it.

1. Admin, Backups, add a destination. It takes an endpoint, region, bucket and keys.
2. Or from the CLI, which prompts for the keys:

   ```bash
   stackr dest add offsite --server --shared --endpoint https://s3.example.com --region eu --bucket stackr-backups
   ```

## The panel

The panel archives its database, master key and proxy certificates on a schedule. The default is daily at 03:00, keeping 14.

1. Admin, Backups, Stackr's own database. Set the schedule, how many to keep and the destination.
2. To run one now:

   ```bash
   stackr admin panel-backup
   stackr admin panel-backups
   ```

3. A copy is also taken before every upgrade.

Only the scheduled archives go to the destination. An archive you run now and the pre-upgrade copy stay on the controller, so copy one off the box yourself before you wipe it.

The archive is encrypted with the recovery passphrase the installer showed. Keep that passphrase off the box.

### Restore the panel

1. On a new box, install first with the same flags as the old one (see [Install](install.md)). Restore needs that install's answers.
2. On the controller, as root, from a local archive (they are under `/var/lib/stackr/backups/stackr/_panel/`):

   ```bash
   read -rs STACKR_RECOVERY_PASSPHRASE && export STACKR_RECOVERY_PASSPHRASE
   stackr-install restore panel.tar.gz.age
   ```

3. Or straight from S3. A key ending in `/` takes the newest archive under it. Use the old install's id, it is in the old box's `install.json`:

   ```bash
   export STACKR_BACKUP_ENDPOINT=https://s3.example.com STACKR_BACKUP_REGION=eu
   read -rs STACKR_BACKUP_ACCESS_KEY && export STACKR_BACKUP_ACCESS_KEY
   read -rs STACKR_BACKUP_SECRET_KEY && export STACKR_BACKUP_SECRET_KEY
   stackr-install restore s3://stackr-backups/stackr/_panel/<old install id>/
   ```

The passphrase can also come on stdin with `--passphrase-stdin`. `--passphrase` works too, but other users on the box see it in `ps`. With none, restore tries this box's master key. On a new box that is the new install's key, so in a terminal it asks for the passphrase.

The old database is kept as `stackr.db.before-restore-<time>`. Workers reconnect by themselves and keep their tiles running.

On its first boot the restored panel deploys again every tile on the controller that ran before and has no container now. They show as deploy jobs in the job list. Tiles you had stopped stay stopped. Their volumes start empty on a new box: restore those from their own backups (see Volumes below).

The CLI on the controller is logged in as the admin again: restore makes a new API key and writes `/var/lib/stackr/cli.json`.

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

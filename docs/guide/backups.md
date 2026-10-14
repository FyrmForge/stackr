# Backups

Two things are backed up: the panel itself, and the data volumes of your apps.

## Destinations

With no destination, archives stay on the controller under its data dir (`/var/lib/stackr/backups`). That is the same box. Add an S3 destination to get them off it.

1. Admin, Backups, add a destination. It takes an endpoint, region, bucket and keys.
2. Or from the CLI, which prompts for the keys:

   ```bash
   stackr dest add offsite --endpoint https://s3.example.com --region eu --bucket stackr-backups --shared
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

The archive is encrypted with the recovery passphrase the installer showed. Keep that passphrase off the box.

### Restore the panel

1. Get the archive. Local ones are under `/var/lib/stackr/backups/stackr/_panel/`. For S3, download the newest object under `stackr/_panel/<install id>/`.
2. On the controller, as root:

   ```bash
   sudo stackr-install restore --passphrase '<recovery passphrase>' panel.tar.gz.age
   ```

   Without `--passphrase` it uses this box's master key. On a new box you need the passphrase.

The old database is kept as `stackr.db.before-restore-<time>`.

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

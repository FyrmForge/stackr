# Troubleshooting

Start on the box that runs the panel, as root. The panel is the Docker container `stackr`, the data is in `/var/lib/stackr`, and the proxy is `stackr-proxy`. A worker runs as the systemd unit `stackr-worker`.

## The controller is down

Running apps keep running and workers keep their tiles. Nothing can deploy, and the panel does not answer, until it is back.

1. Look at the container:

   ```bash
   docker ps -a --filter label=stackr.role=panel
   docker logs --tail 100 stackr
   ```

2. Not running and not crash looping: `docker start stackr`. It restarts by itself unless it was stopped.
3. Crash looping, the log ends in `migrate: ... Dirty database version N`: see [A migration failed](#a-migration-failed).
4. Crash looping on anything else: the last log line is the cause. A full disk (`df -h /var/lib/stackr`) and a missing or wrong master key (`/var/lib/stackr/keys/master.key`) are the usual two.
5. The panel is up but the site does not answer: check `docker ps` for `stackr-proxy` and `docker logs --tail 100 stackr-proxy`. Ports 80 and 443 must be free.
6. The box is gone or the database is lost: [restore from a backup](#restore-from-a-backup).

Workers reconnect by themselves when the panel returns.

## A worker is unreachable

The node shows offline in Admin, Nodes, and the alerts webhook fires `node.offline` after about a minute.

1. On the worker:

   ```bash
   systemctl status stackr-worker
   journalctl -u stackr-worker --since "1 hour ago"
   ```

2. Not running: `systemctl restart stackr-worker`.
3. Running but offline: the mesh needs UDP 51871 and TCP 7443 and 7444 open between the worker and the controller. `stkr node show <name>` on the controller has the state and the last error.
4. The log says `certificate expired` or `not rejoined`, or the box was reinstalled: [rejoin it](nodes.md#rejoin-a-broken-worker).
5. A clock more than 30 seconds off the controller's fires `clock.skew` (see [Alerts](alerts.md)). Nothing is refused, but certificates and tokens misbehave. Fix time sync.

Apps on an offline worker keep running. Deploys to it wait.

## The panel does not start and the database is on a node disk

`docker logs stackr` says `panel database: disk missing: the database is on node disk <id> and it is not mounted. Not starting, so no empty database is made`. The panel exits on purpose and Docker restarts it in a loop. The empty folder under the mount is immutable, so nothing can make a database there. Attach the disk, then run `stackr-install disk-check` on the node. The restart loop brings the panel up, no reboot needed.

If it says the disk has no marker or belongs to another install, it is the wrong disk: do not force it. If it says a move is in progress, run `stackr-install` on the node (no arguments): it reads `/var/lib/stackr/move.journal` and finishes or rolls the move back. The log line names the phase.

If the disk is gone or failing for good, restore from a backup on this box without it: `stackr-install restore --abandon-disk <archive>` (see [Backups](backups.md)). It gives the disk up and restores onto the system disk. Or restore on a new box. Both need the recovery key, as always.

## A failed upgrade or rollback

**Panel.** The upgrade archives the database, starts the new image next to the old one and waits for it to be healthy. If it is not, the new one is removed, the database goes back to the copy taken before the swap and the old panel starts again. The Update page says the upgrade failed. Check `docker logs stackr` for the old panel and the Update page for the reason, fix it, then try again. The pre-upgrade archive is listed in Backups.

If the old panel did not come back, the swap's own error names the copy of the database it could not restore. Do not start the panel until the database is right. Restore the pre-upgrade archive ([below](#restore-from-a-backup)) or stop the panel, delete `db/stackr.db-wal` and `db/stackr.db-shm`, and move the copy from `/var/lib/stackr/swap/` back over `/var/lib/stackr/db/stackr.db`. The panel database is always `/var/lib/stackr/db/stackr.db`. The old path `/var/lib/stackr/stackr.db` is a directory with a note in it.

**Workers.** The rollout goes one worker at a time. A worker that does not come back on the new build rolls itself back and the rollout stops. The Workers list on the Update page shows `<node> failed to upgrade, rolled back` and the error. Fix the node, then press **Retry** or run `stkr admin upgrade-workers`. A worker that is offline is upgraded when it reconnects.

A worker stuck between builds: `systemctl status stackr-worker` and `journalctl -u stackr-worker`. If it will not start, [rejoin it](nodes.md#rejoin-a-broken-worker).

The installer refuses an older version than the one running, since migrations may not reverse. Restore the pre-upgrade archive instead, or pass `--allow-downgrade`.

## A migration failed

A migration that fails leaves the database marked dirty at that version. Every start then refuses it with `Dirty database version N`. The migration ran in a transaction, so its changes were rolled back. A normal upgrade puts the database back by itself; this is for a panel that was restarted or swapped by hand.

1. Stop the panel: `docker stop stackr`.
2. Copy the database with its journal files: `mkdir /var/lib/stackr/before-force && cp /var/lib/stackr/db/stackr.db* /var/lib/stackr/before-force/`.
3. Read the state. This runs the panel image once and touches nothing:

   ```bash
   IMG=$(docker inspect -f '{{.Config.Image}}' stackr)
   docker run --rm -v /var/lib/stackr:/var/lib/stackr \
     -e DATA_DIR=/var/lib/stackr -e DATABASE_PATH=/var/lib/stackr/db/stackr.db \
     "$IMG" migrate-force
   ```

   It prints `schema version N, dirty true`.
4. Force the version before the one that failed, then start the panel:

   ```bash
   docker run --rm -v /var/lib/stackr:/var/lib/stackr \
     -e DATA_DIR=/var/lib/stackr -e DATABASE_PATH=/var/lib/stackr/db/stackr.db \
     "$IMG" migrate-force $((N-1))
   docker start stackr
   ```

   It sets the version without running anything, so the start runs migration N again. If it fails again, the log has the SQL error. That is a bug in the release: go back to the previous image (the installer with `--allow-downgrade`, with the files from step 2 put back) and report it.

Forcing the wrong version skips or repeats a migration. When unsure, restore the last backup instead.

## Restore from a backup

Use it when the box or its database is lost. The full steps are in [Backups](backups.md#restore-the-panel). In short:

1. On the new box, install with the same flags as the old one.
2. Have the recovery key, the one the installer showed once.
3. As root, from a local archive or from S3:

   ```bash
   read -rs STACKR_RECOVERY_KEY && export STACKR_RECOVERY_KEY
   stackr-install restore /var/lib/stackr/backups/stackr/_panel/<install id>/manual/<file>
   ```

4. Workers reconnect by themselves. Tiles that ran on the controller are deployed again, and their empty volumes are restored from the newest backup first.

The replaced database stays in `/var/lib/stackr/swap/` as `stackr.db.before-restore-<time>` with its `-wal` file. If the restored panel is wrong, stop it, delete the new `db/stackr.db-wal` and `db/stackr.db-shm`, and move both old files back to `db/stackr.db`.

A volume that has no reachable backup starts empty. A backup kept only on the old controller's disk went with it, so keep an S3 destination (see [Backups](backups.md#destinations)).

## The NAS refused access to a share

A tile that mounts an NFS share fails with "The NAS refused access to share <name> (<source>)". The export maps root to nobody, so the node cannot read it. On the NAS, give Other read and execute on the export's top folder, or set maproot (no_root_squash). Then use Test on the share row in the org settings to check it before you deploy again.

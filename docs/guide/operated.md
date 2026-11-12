# Operated databases

An operated tile is a database instance stackr runs for an env: `postgres` or `s3`. Apps never use the instance directly. They use a slice tile, a database (or bucket) cut from it, and each app that names the slice gets its own login on it.

## Make one

Create does not deploy. The tile exists after Create, and nothing runs until you deploy it.

1. Open the env, New tile, source Operated, pick the engine, Create.
2. Deploy the tile. Postgres is ready when its deploy finishes.
3. Add a slice tile (source Slice) that provisions from it, `<stack>:<env>:<tile>`, and deploy that too. The slice is cut at its deploy.

CLI (`stackr operated` has the alias `opr`):

```bash
stackr operated create pg --engine postgres --env dev
stackr deploy pg --env dev
stackr slice add pg-db --from shop:dev:pg --env dev
stackr deploy pg-db --env dev
```

`--major 14` to `18` picks the postgres major; the default is the newest still supported. `stackr operated ls` lists the env's operated tiles.

## What apps read

An app reads its slice with refs in its env, and picks its own variable names:

```yaml
env:
  DATABASE_URL: ${{ tile.pg-db.url }}
  S3_ENDPOINT: ${{ tile.files.endpoint }}
```

Postgres slice outputs:

| Output | Value |
| --- | --- |
| `url`, `host`, `port` | the connection the slice's `url` setting picks |
| `url_direct`, `host_direct`, `port_direct` | the instance itself |
| `url_pooled`, `host_pooled`, `port_pooled` | the instance's PgBouncer pooler (transaction pooling) |
| `url_read`, `host_read`, `port_read` (and the `_direct` and `_pooled` forms) | where reads go; the primary's values until standbys exist. Each standby has its own PgBouncer, so the `_pooled` forms pool on a consumer's node when a standby is in reach there |
| `database`, `user`, `password` | this app's own login (`password` is secret) |

Set `url: pooled` on the slice tile (stack file, `stackr slice url <slice> pooled`, or the slice drawer) to make `url`, `host` and `port` go through the pooler. The default is `direct`. An app on another node than the instance gets the direct values whatever the setting, because the pooler has no address there yet.

Bucket slice outputs: `endpoint`, `bucket`, `region`, `access_key`, `secret_key` (secret) and `public_url` when the bucket is public. A ref to a name a slice does not publish fails the plan and lists the valid ones.

## Start from a dump

A new postgres tile can start with your data in it.

1. Open the env, New tile, source Operated, engine postgres.
2. Pick the file under Start from a dump, then Create.

Or from the CLI, with a file or `-` for stdin:

```bash
stackr operated create pg --engine postgres --env dev --from-dump shop.dump
pg_dump -Fc "$OLD_DATABASE_URL" | stackr operated create pg --engine postgres --env dev --from-dump -
```

This makes two tiles, the instance `pg` and a slice `pg-db`, then runs one job. It deploys the instance, waits until postgres answers, deploys the slice and loads the dump into the slice's database. Apps that use `pg-db` see the data. Name the slice with `--slice`.

The CLI follows the job and exits non-zero if the load fails. `--no-wait` prints the job and returns. In the panel the job shows under the env's jobs.

What it loads:

- `pg_dump -Fc` (custom format) goes through `pg_restore`.
- Plain SQL (`pg_dump` with no `-F`) goes through `psql`.
- A gzipped dump is refused. Unpack it first: `gunzip -c shop.sql.gz | stackr operated create ... --from-dump -`.
- Tar dumps, `pg_dumpall` and dumps made with `-C` are not loaded.

The source's roles do not matter. Owners and grants in the dump are skipped, and everything is made by the slice's owner, so every app bound to the slice reaches it.

The load is one transaction. If any statement fails, nothing is kept: the job fails with the end of postgres's error output, and the tiles stay with an empty database. Fix the dump, remove both tiles and create them again.

The upload streams to the panel's disk, under its data dir, until the job loads it, then it is deleted. Leave room there for one copy of the dump.

Only a new tile loads a dump. Loading into an existing tile, and dumps in the stack file, are not supported. A stack run from its stack file refuses `--from-dump`.

## Standbys

A standby is a second postgres copy on another node. It copies the primary, then streams from it. At most 2 per instance.

In the instance drawer, Overview tab, the Members list shows the primary and its standbys with lag and slot size. Add one with "Add a standby on" (pick a node), then Add standby. Each standby row has Fence or Unfence and Remove, with a confirm; Rebuild shows only on a standby that needs it. The CLI mirror:

```bash
stackr operated standby add pg --node n2
stackr operated standby ls pg
stackr operated standby rebuild pg n2
stackr operated standby rm pg n2
```

Rebuild is for a standby that needs it; it copies the data again from the primary. Remove drops its container, volume and slot.

### Fence

Fence a standby to keep it down: it stops, leaves the read pool and is not started until you unfence it. In the Members list use the Fence or Unfence button, or:

```bash
stackr operated standby fence <tile> <node|id>
stackr operated standby unfence <tile> <node|id>
```

A standby that needs a rewind or a rebuild cannot be fenced or unfenced; it rejoins by itself, or rebuild it. The primary is never fenced by hand.

### Synchronous replication

Drawer, Overview tab, Synchronous replication section.

- Sync commits: a commit waits for a standby before it counts.
- Wait for standbys closer than (ms): a standby in auto mode is waited for only while its round trip is under this (default 10).
- When no standby answers: "Carry on without waiting" (preferred, the default) or "Freeze writes" (required, writes hang until a standby is back).
- Per standby: Auto, Always wait or Never wait. Auto waits while the standby is close, Always always, Never not at all.

If the panel is down and the standby is down too, writes hang until the panel or a standby returns, in preferred mode as well as required.

CLI:

```bash
stackr operated sync on pg [--limit-ms 10] [--preferred | --required]
stackr operated sync off pg
stackr operated sync status pg
stackr operated standby sync pg n2 always     # auto, always or never
```

In the stack file these are `sync`, `sync_required`, `sync_limit_ms` and each standby's `sync`. `stackr stack export` writes them, but the file is applied through the org config repo (a `stacks:` entry in `stackr-org.yml`, then `org plan` and `org approve`) or a push or promote, never directly.

### Switchover

Moves the primary to a standby while both run: the primary shuts down cleanly and the standby is promoted. Clients reconnect once.

Panel: Members section, "Switch the primary to" (pick the standby), Switch over. CLI:

```bash
stackr operated switchover pg [--to n2]
```

With no `--to` the standby that replayed the most is picked.

A switchover or a promote is a job: the checks answer before anything changes, then the steps (stopping, waiting for catch-up, promoting, rerouting, rejoining) are written to the job log, which the drawer shows under Last job and the CLI follows (`--no-wait` prints the job and returns). While the drawer is open its pooler, members, failover and sync sections update by themselves.

### Abort

A switchover that is stuck before the standby is promoted (the standby never replays the shutdown checkpoint, say) can be given up: Abort switchover in the Members section, or `stackr operated switchover pg --abort`. The old primary is unfenced and started again. Once the promote step is reached, or the standby is out of recovery, the abort is refused and the switchover has to finish.

### Failover

For a primary that is down. Panel: Members section, "Promote the standby on" (pick one), Promote.

CLI:

```bash
stackr operated failover pg --to n2
```

The old primary must be known to be stopped. If its node answers and the container is not running, that is enough. If the node does not answer, confirm it is gone: type the node's name in the panel, or pass `--node-gone`. Then the panel needs a recorded lease on the old primary and waits for it to run out. With no such record, say you powered the node off by typing `I powered the node off` in the panel's "To promote over a node you powered off" field, or `--powered-off "I powered the node off"`. The phrase must match exactly; it replaces the lease proof and the wait, and is logged.

A standby the primary did not wait for may lack the last commits. While one it did wait for is usable, promoting the other is refused unless you tick "Promote even if it may lack the last writes" or pass `--allow-async-target`.

If the primary is running, failover is refused: switch over instead.

## Settings

Drawer, Settings tab. Rows: Name, Allow, Env pairs, Postgres, Delete.

Postgres row: memory limit, the main settings, Advanced, and "Another setting" with Value for any other name. Save settings reloads, or restarts the instance when a setting needs it (the row then says "needs a restart"). stackr manages a few settings itself and does not list them. CLI:

```bash
stackr operated settings get pg
stackr operated settings set pg work_mem=64MB
stackr operated settings unset pg work_mem
```

### Allow list

Who outside the env may cut slices from this tile. Allow row: Add a pattern, `org:stack:env:tile`. Patterns must start with the tile's own org. A trailing `*` stands for the rest, so `acme:*` is anyone in the org `acme` and `acme:shop:*` is any env of `shop`. With no pattern only slice tiles in this env may. Only an org owner changes the list.

```bash
stackr operated allow pg --add acme:shop:*
stackr operated allow pg --rm acme:shop:*
stackr operated allow pg --set 'acme:*'     # --set '' empties it
```

### Env pairs

Which of this stack's envs a slice lands in, by the env the consumer asks for. Env pairs row: Consumer env, This stack's env, Add. With no pair a slice lands in the env it names.

```bash
stackr operated env-pairs pg staging=prod    # prints the diff, then asks
stackr operated env-pairs pg --clear
```

The call replaces the whole map.

## Versions

Overview tab shows the image the instance runs.

- Update (minor): when one is out, a banner says "Update available" with Apply. It moves to the current minor of its major and restarts. `stackr operated version pg` shows the version and whether an update is out; `stackr operated update pg` applies it.
- Upgrade (major, down too): Overview tab, "Postgres version" (Upgrade to N or Downgrade to N), Check, then Change version. Check only asks whether the data can move. The database stops while the data moves and the old data is kept for 7 days.

```bash
stackr operated upgrade pg --to 17 --check
stackr operated upgrade pg --to 17
```

- Rollback: while the old data is kept, the Overview tab offers Roll back. Type the instance's name to confirm. It returns to the previous major with the data it had then; what was written since is lost.

```bash
stackr operated rollback-upgrade pg
```

## Health

Drawer, Database tab: connections, cache hit ratio, oldest open transaction, database size, standby lag and the costliest statements. The Overview tab shows the pooler.

```bash
stackr operated status pg [--top]
```

## Point-in-time recovery

Drawer, Backups tab, the point-in-time panel. Off: set Days to keep and Turn on. On: it shows the restorable range, the last base backup and the archive. Turning it on or off, or changing the days, restarts the database once.

```bash
stackr operated pitr set pg --days 7      # or --days off
stackr operated pitr status pg
```

### Restore to a time

Backups tab: pick Time (UTC), then Restore, or Latest. It makes a new instance, `<tile>-restored-YYYYMMDD-HHMM`; the source is not touched. With a gap in the archive (a failover lost WAL) only the listed ranges work.

```bash
stackr operated restore pg --at 2026-11-07T10:42:00Z
stackr operated restore pg --at latest
```

### Accept the restore

Open the restored copy's drawer, Backups tab, Promote. It replaces the source instance's data with the copy and restarts it. The source's apps keep their address and login. The old data stays as an orphaned volume until retention ends.

```bash
stackr operated accept-restore pg-restored-20261107-1042
```

## Branch a slice

Slice drawer, Branch row: a copy of the database as of now, or as of a past time (As of (UTC), needs point-in-time recovery), with its own login. Optional Env, Instance and Expires (`3d`, `36h`). The copy is a new slice tile `<slice>-br-xxxx`.

```bash
stackr slice branch pg-db [--at TIME] [--expires 3d] [--to-env prod] [--instance stack:env:tile]
```

## Old archive epochs

A major change or its rollback starts a new point-in-time archive. The old archive stays while the change can be rolled back (7 days), then it is deleted from the backup destination and from the box.

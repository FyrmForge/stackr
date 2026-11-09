# Operated databases

An operated tile is a database instance stackr runs for an env: `postgres` or `s3`. Apps never use the instance directly. They use a slice tile, a database (or bucket) cut from it, and each app that names the slice gets its own login on it.

## Make one

1. Open the env, New tile, source Operated, pick the engine, Create.
2. Deploy the tile. Postgres is ready when its deploy finishes.
3. Add a slice tile (source Slice) that provisions from it, `<stack>:<env>:<tile>`.

Or from the CLI:

```bash
stackr operated create pg --engine postgres --env dev
stackr slice add pg-db --from shop:dev:pg --env dev
```

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
| `url_read`, `host_read`, `port_read` (and the `_direct` and `_pooled` forms) | where reads go; the primary's values until replicas exist |
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

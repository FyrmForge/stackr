# Step 1: groundwork

Read first: `docs/rewrite/PROGRESS.md`, then REWRITE.md "Rules from the first
commit", "Service tree and repo layout", "Job queue", "Promote and releases",
"Param store and refs", "Volumes", "Auth", "Backups" (the schema parts only),
"Build order" row 1. Extracts for this step:
`docs/rewrite/extracts/{001_initial,002_api_key_org,svcerr-scheduler,
workqueue,deploystate,secrets,netaddr,access-revoke}.md`.

Design first, then schema, then code. Nothing here talks to Docker for real;
the fake is enough. Commit per task.

## Tasks

1. **Write the schema design note** `docs/rewrite/schema.md` before any SQL:
   one table per leaf (org, stack, user, environment, tile, image, params,
   settings, volume, domain, credential, connector, managed, release, job,
   backup), plus `org_members`, `invites`, `api_keys`, hamr's sessions.
   Columns start from the `001_initial` extract (keep what v1 needs, as is)
   and the plan overrides below. Every table names its owning leaf; a table
   with no leaf is a plan bug, raise it.
   Done when: the note lists every table, its leaf, and its plan overrides.

2. **Migration A, the design tables first:** `releases` (id, stack_id,
   number per stack, created_at, created_by), `release_tiles` (release_id,
   slug, repo, branch, commit, image_id nullable, digest), `environments`
   (as 001 plus `position` integer, `release_id` nullable, `from_kind`
   `branch|promote`, `from_branch`, `auto` bool, `base_env_id` nullable for
   PR envs), `jobs` (id, kind, state `queued|running|waiting|done|failed|
   superseded|cancelled`, `release_id` nullable, `lock_set` JSON of tile ids,
   `waiting_param` nullable, `log_path`, `error`, created/started/finished,
   30-minute cap enforced by the worker not the schema). No `deployments`
   table. No `status` column on tiles.
   Done when: migration applies on a fresh SQLite and the four tables match
   the note.

3. **Migration B, everything else:** tiles (kind `service|image|managed`,
   `env_json` for the literal-or-ref env block, `update_policy`,
   `tag_policy`, `replicas`, health path, `git_url`, `watch_paths`), images
   (built images + the watch cache: last digest, last error, checked_at),
   `params` (scope_kind `org|stack|env`, scope_id, collection, name, kind
   `param|secret`, value encrypted, unique on scope+collection+name),
   `settings`, `volumes` (env_id or owning instance, slug, `orphaned_at`
   nullable), `domains` (tile_id, host, redirect flag, `proxy_json` named
   extras, `raw_caddy` admin-only), `credentials` (registry pull creds),
   `connectors` (one GitHub App per org, host), `managed_instances`
   (engine, scope_kind, scope_id, admin creds encrypted, endpoint) +
   `provisions` (slice per consumer tile), `backup_destinations` (kind
   `local|s3`, key encrypted, org or admin-global + shared flag),
   `backup_schedules` (volume_id, method, dest_id nullable, cron, keep,
   mode), `backup_runs` (`volume_id` nullable + `kind` so the panel
   self-backup has a row), `api_keys` with `org_id` (extract 002),
   `invites` (email, org, expires_at, used_at). `credentials` get an
   `org_id` (the old `registries` table had none). `acme_email` is a
   settings key. Foreign keys on every parent id, including the managed
   tables (the old ones had none), cascades declared in SQL,
   `PRAGMA foreign_keys` checked in the harness.
   Done when: migration applies; a delete on an org cascades in a test
   without any Go code doing it.

4. **Store package** `service/internal/store`: one file per table, one small
   interface per table, one `Tx` type so a flow writes several tables in
   one transaction, sqlx. CRUD and type mapping only: no defaults, no
   ORDER BY policy, no status logic. Encrypted columns go through the
   secrets helper (task 5). Joins only in a flow's own query file with a
   one-line reason.
   Done when: every table has create/get/list/update/delete as the leaf
   needs, and a round-trip test per table writes then reads back every
   field (B3).

5. **Secrets at rest** `service/internal/secrets`: AES-GCM from the
   `secrets` extract, key from config, used by the store for param secrets,
   credentials, instance creds, destination keys.
   The old `Encrypt` failed open (no key loaded = plaintext stored,
   `Decrypt` returned it with nil error). The new one refuses: no key is a
   startup error, never a silent plaintext write.
   Done when: encrypt/decrypt round-trips, a wrong key fails typed, and a
   missing key fails `service.New`.

6. **Typed errors** `service/errs` (exported: handlers map them to HTTP):
   from the `svcerr` extract. `NotFound`, `Conflict`, `Invalid` (with field),
   `Refused` (a rule said no, with the reason), `Unset` (a param ref that
   parks a job), `Busy`. Nothing anywhere branches on an error string.
   Done when: the package exists and `errors.Is`/`As` tests pass.

7. **Docker interface and fake.** `service/docker.go` exports `type Docker
   interface` with the container, network, volume, image, build, log and
   exec methods the wrapper will implement in step 2 (signatures from the
   `runtime` extract's `ContainerSpec`), and `WithDocker(d Docker)` option.
   `service/internal/docker` gets a compile-only stub that satisfies it.
   `service/internal/dockerfake` records calls and returns scripted answers.
   Done when: `service.New(cfg, service.WithDocker(fake))` compiles and the
   fake is used by task 11.

8. **`service.New` and the orchestrator.** `Config` (data dir, DB path,
   secrets key, worker count, image-watch interval, orphan retention days),
   `New` opens the store, runs migrations, builds the Docker client (or
   takes the fake), builds every leaf and flow once (B0), returns
   `*Orchestrator`. Verb methods are added per step; this step ships `Ping`
   and the job verbs from task 10. `main` in `cmd/stackrd` calls `New`
   once.
   Done when: `main` builds nothing but `service.New` and the routers; a
   test asserts each constructor runs once.

9. **`authz` and the middleware.** `authz.can(user, verb, resource)` with
   two roles (stackr admin, org owner) from the `access-revoke` extract, and
   one middleware used by both routers: resolve `/:org/:stack/:env/:tile`
   in order, 404 early, load role + memberships once per request, put the
   loaded rows in the context so handlers never re-fetch. Session auth and
   API-key auth both land in the same middleware. Keys are org-bound and
   carry the user's live abilities. Include the revoke rule: demote or
   disable closes sessions and keys (B16), one function.
   Done when: table tests cover admin, member, non-member, disabled user,
   demoted key for each verb; both routers mount the same middleware.

10. **`flow/jobs`** `service/internal/flow/jobs` + `leaf/job`: durable
    queue over the `jobs` table. N workers (setting, default 2). Lock set
    computed by one function. Pick the oldest runnable job whose lock set
    is free. Railway rule: newer job on the same tile supersedes a waiting
    one (`superseded`), cancels a running one in build phase, waits for one
    in swap phase; never abort mid-swap. `waiting` jobs re-polled every few
    seconds and re-run when their param resolves. Each job runs under its
    own context with a 30-minute cap, detached from the request. Rebuild
    the runnable set from the table on start (crashed `running` jobs become
    `failed`). Log path per job. Jobs run flows with no user.
    Wiring (DECIDE 12, option a): `flow/jobs` imports no other flow and
    no flow imports `flow/jobs`. `jobs.New(store, handlers map[Kind]Handler)`
    takes the flow functions injected by `service.New`; the orchestrator
    is the only thing that enqueues. depguard stays as step 0 left it.
    Done when: tests cover lock-set exclusion, disjoint parallelism,
    supersede in each phase, waiting → runnable, cap, restart recovery
    (B25).

11. **Test harness** `service/testing` (or `internal/testutil`): real
    SQLite in `t.TempDir()`, migrations applied, `dockerfake` wired, a
    helper that returns an `*Orchestrator` per test. Leaf rules test as
    near-pure functions.
    Done when: every test above uses it; `make test` passes in under a
    minute.

12. **Settings catalogue** `leaf/settings`: one list of knobs (worker count,
    image-watch interval, orphan retention, panel domain, …) with type,
    default, scope, from the `settings` extract's `keys.go` and cascade
    (server → org → stack → env → tile) `Resolve`/`Check`/`Merge`. Every
    surface enumerates this one list. A typo in a number field is refused,
    never read as unset (B24).
    Done when: `Resolve` tests cover the cascade and B24.

13. **Done gate.** `make build`, `make lint`, `make test` pass; PROGRESS.md
    ticked and committed; stacked PR onto the step 0 branch.

## Not in this step

No real Docker calls, no deploy flow, no handlers beyond what the middleware
test needs, no UI.

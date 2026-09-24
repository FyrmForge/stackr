# Step 3: services

Read first: `docs/rewrite/PROGRESS.md`, then all of REWRITE.md from "Rules
from the first commit" to "v1 scope", and "Spec and tests from the old repo".
Every extract in `docs/rewrite/extracts/` is relevant here; each task names
its own. Rule 9: a domain rule not written in the plan behaves as the
extract shows; a Swarm mechanic not written in the plan is not built.

Leaves first (one row kind + its world object, own table only, facts in as
arguments), then flows (sequence leaves, compute and pass down, every
container op through `flow/jobs`), then orchestrator verbs. Each leaf ships
with its tests before the next starts. Commit per task.

## Leaves (`service/internal/leaf/<name>`)

1. **`leaf/org`.** Create, rename, delete with the rules from the
   `org-rules` extract (has stacks, last org, squat check), memberships,
   invites (create, accept atomically burning the invite, 7-day expiry, B14
   B15), API keys (mint bound to org, never more than the minter holds,
   B36).
   Done when: rule tests incl. B14, B15, B36.

2. **`leaf/user`.** First account becomes stackr admin; passwords (min
   length, change needs current, admin can set); disable and demote share
   one "loses powers" rule that closes sessions and keys (B16, from the
   `access-revoke` extract).
   Done when: B16 test and first-admin test.

3. **`leaf/stack`.** Create/rename/delete, config repo pointer, stack-level
   domain reservations, `defaults` cascade slot.
   Done when: round-trip tests.

4. **`leaf/environment`.** Row + its Docker network. `position` ordering
   ("is X above Y", the ladder), `from`/`auto` knobs, `base_env` for PR
   envs, clone and teardown from the `envops` extract, `Network(env)`
   ensures the network. `Delete(env, tileCount)` refuses non-empty.
   Done when: ladder tests, network ensured through the fake.

5. **`leaf/tile`.** Row + its containers: replicas and the pause container
   (network alias = tile name), VIP rules through `internal/vip`,
   `Start(spec)`, `Stop`, `Remove`, `Logs`, `Exec` (engine commands),
   `State()` from Docker first then the last job row, no `status` column.
   Create/update/delete and the per-field refusal vocabulary from the
   `tilelifecycle` extract; one whitelist of what each kind may carry from
   the `runpolicy` extract (B26); the one guard over start/stop/remove/
   terminal from the `container-guard` extract; "which keys moved earns
   which side effect" (restart vs redeploy vs route rewrite) from the
   `release-tilediff` extract. Health gate: poll inspect, healthy (or
   running past 10s grace when no HEALTHCHECK) AND restart count zero,
   deadline 60s + start period. Env keys validated as POSIX names.
   Done when: tests for B26, the guard, the side-effect table, the gate
   (healthy, crash-loop, timeout), replicas>1 refused with a mount.

6. **`leaf/image`.** Built images on the box and the watch cache (digest,
   error, checked_at). Cleanup keeps every image a release references.
   Done when: cleanup test.

7. **`leaf/params`.** The store: collections, `param|secret`, scopes org/
   stack/env, lower overrides; the ref grammar `${{ }}` rewritten from the
   `varref` extract against "Param store and refs": `params.` (env → stack,
   stops at stack), `org.params.`, `self.`, `tile.<slug>.`, `stack.<slug>.`,
   `org.<slug>.`, `stackr.`, `org.backups.`; where refs are allowed; unset
   param → `errs.Unset`, anything else a hard error; declassify refused;
   merge verb; secrets never loaded for readers without the permission;
   random secret generation.
   Done when: B4, B5, B35, B37 tests plus a grammar table test.

8. **`leaf/volume`.** Row + Docker volume, env-scoped or instance-owned,
   `Ensure`, the only `HoldsData`, orphan flag + re-adopt on same slug,
   explicit delete.
   Done when: orphan/re-adopt tests.

9. **`leaf/domain`.** Row + Caddy route + the tile's ingress network
   (created with the first domain, removed with the last). Builds the Caddy
   JSON for a tile: upstreams = replica names on the ingress network,
   Caddy health checks, redirect, the named `proxy:` extras (`basic_auth`,
   `websockets`, `max_body`, `timeouts`, `headers`, `methods`,
   `strip_prefix`), admin-only raw snippets, ACME, trusted proxies from the
   `proxy-ref` extract. `FullConfig()` for the re-push on proxy start. One
   nil-vs-false convention for HTTPS (B31).
   Done when: config golden tests per extra; ingress network lifecycle
   through the fake.

10. **`leaf/credential`.** Registry pull credentials, encrypted.
11. **`leaf/connector`.** One GitHub App per org, resolved by `git_url`
    host; webhook registration; PR env create/remove requests come out as
    facts for the flow. No connector for a host = typed error.
12. **`leaf/managed`.** `managed_instances` + `provisions`, scope rules from
    the `managedinstance` extract, slice naming from the `managedtiles`
    extract.
13. **`leaf/release`.** `releases` + `release_tiles`: next number per
    stack, snapshot copy with one tile swapped, diff of two releases (tiles
    created/changed/orphaned, images moved), digest pinning.
    Done when: diff and copy tests.
14. **`leaf/job`.** Already exists from step 1; add list/poll views.
15. **`leaf/backup`.** Destinations (local default on every install, S3
    extras, per-destination random key, visibility rules), schedules on
    volumes with `method`, runs, keep-last-N prune, `restorable` and
    `VolumeFor` from the `backup-infra` extract's leftovers.
    Done when: prune and visibility tests.

## Flows (`service/internal/flow/<name>`)

16. **`flow/deploy`.** One deploy path for every kind. Resolve params
    (`leaf/params`), network (`leaf/environment`), volumes (`leaf/volume`),
    managed definition or slice (`flow/managed`), build the resolved spec
    (plain struct in the flow), pull by digest, then the rollout: mount-free
    tile = overlapping (start new on env + ingress networks, gate, add to
    VIP and Caddy upstreams, remove old, stop); tile with a mount or any
    managed instance = stop-then-start, one replica. Unset param parks the
    job as `waiting`. Redeploy on a settings/param change uses the env's
    current release image, never the branch head (B34). Same gate rule for
    deploy and redeploy (B29).
    Done when: fake-Docker tests for both rollouts, park, B34, B29, gate
    failure keeps the old container.

17. **`flow/managed`.** `ManagedTile` interface (`Definition`, `Ready`,
    `Provision`, `Drop`, `Bindings`, `Backup(method)`, `Restore(method,
    target)`), registry map, `postgres.go`, `s3.go` from the
    `managedtiles` extract; engines produce commands, run through
    `leaf/tile.Exec()`; one slice per consumer, slice fate when the
    consumer goes; outputs written to provisions. Never starts a container.
    `// ponytail:` slot for scaling.
    Done when: provision/drop tests with a fake exec.

18. **`flow/promote`.** Stack file grammar + include merge + env overlays +
    plan diff from the `stackconf` extract (no apply.go job wiring, no
    staging, no moved, no apply policy, no traefik_override); the applier
    becomes reconcile. `Promote(env, release, dryRun)`: diff release vs live
    state, return the plan (tiles create/update/delete, domains, slices,
    volumes orphaned, images moved) or reconcile it, then call
    `flow/deploy` in-process per changed tile, one job locking the env's
    tiles. Build-on-push: connector push event → build changed tiles
    (`watch_paths`, default build context) → image reuse for the rest →
    write the release → land in every env `from` that branch; `auto`
    promotes up. Rollback = promote previous; per-tile rollback = copy
    release with one image swapped, promote. "What blocks this promote" has
    one answer (B20). Refs in domains take `params.` only.
    Done when: grammar tests from the extract, plan-diff tests, B20, B2
    (API and panel share the rule), dry run returns the same plan the real
    run applies.

19. **`flow/backup`.** Run a schedule: method by engine (`pg_dump`, pause/
    stop + tar, `live`), `age`-encrypt with the destination key between
    gzip and upload, run row, prune. Restore as a job with the volume lock:
    pre-restore backup, download, verify before wipe, stop, wipe, untar or
    `DROP SCHEMA` + psql, start; input is a struct (source run, target
    volume) so cross-volume restore is the same call. Orphan retention job
    uses this path. Panel self-backup admin-only with the recovery
    passphrase.
    Done when: local-destination round-trip test with fake exec; restore
    order asserted.

20. **`flow/container`.** Restart, stop, logs stream, exec stream, each a
    job where it touches a container.

21. **`flow/imagewatch`.** The ten steps from "Image watch": dedupe scan
    list, timer (setting, default 60 min) + check now, registry call per
    unique image with the tile's credential, cache on the image row, new
    release per direct env with the digest swapped, chip flag, `auto`
    promotes through the normal job. Semver tag policy.
    Done when: tests for both modes and "no update on registry error".

22. **`flow/upgrade`.** Self-upgrade from the `admin-upgrade` extract:
    check newer tag, pre-upgrade backup, swap the panel container via the
    installer spec, "already running" guard. Also `stackrd proxy` mode:
    Caddy as a library, admin API on the container network, named volume
    for data/config, no Docker socket; stackrd re-pushes the full route
    config whenever the proxy container starts.
    Done when: `stackrd proxy` starts in a container and accepts a push.

23. **Scheduler.** Cron runner from the `svcerr-scheduler` extract driving
    backup schedules, the orphan retention daily job, image watch.

## Orchestrator

24. **One method per verb** in `service/*.go` (tile.go, org.go, env.go,
    stack.go, params.go, volume.go, domain.go, release.go, backup.go,
    managed.go, admin.go). Passthroughs are one line. Verdict fields
    (`CanDeploy`, `Blockers`) come back in results for handlers to render.
    `settings.List()` is the one catalogue.
    Done when: every v1 scope bullet in REWRITE.md maps to at least one
    orchestrator method, listed in `docs/rewrite/verbs.md`.

25. **Done gate.** `make build`, `make lint` (depguard clean), `make test`
    pass; the bug-register table in REWRITE.md has a test per row that
    belongs to this step; PROGRESS.md ticked; stacked PR.

## Not in this step

No HTTP handlers, no CLI, no templates. Webhook receive is a handler (step
4); the push → release flow it calls is here.

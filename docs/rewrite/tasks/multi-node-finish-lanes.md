# Multi-node finish: lanes

Status: planned 2026-10-14 on `multi-node` (head b3805dd, UI round merged). Items 3 to 10 and 7b of `multi-node-finish.md`, plus `worker-upgrades.md`. There are 6 lanes, all Sonnet. The decisions in those two docs are final; this plan only places them in the code.

## Rules

- **Never hand-edit generated files.** These are `*_templ.go`, `ui/static/js/elements/*`, `ui/static/css/output.css`, `docs/openapi.json` and `internal/ui/components/staticmanifest.go`.
  - Regenerate them with `templ generate`, `cd ui && npm run css:build`, `cd ui && npm run ts:build`, `hamr gen static` and `go run ./cmd/stackrd --dump-openapi > docs/openapi.json`, then commit the result. CI checks that it matches.
- **Generated-file merge conflicts:** never merge them by hand. Take either side, regenerate, and commit. Any lane that adds a Tailwind class changes `output.css` and the manifest hash; that is expected.
- **`ui/css/input.css` is not edited this round.** Style with Tailwind classes in your own templ files.
- **Test first.** For Go, write the failing test, then the code. TS has no harness and no TS change is planned; if one turns out to be needed, pin the markup with a templ-render Go test and note a Playwright check on `hamr dev` in the commit body.
- **Migrations:**
  - One new migration, `025_node_lifecycle`, owned by lane R and merged before anything else needs it.
  - Changes are additive, following the 010 and 024 table-rebuild precedent: SQLite cannot widen a CHECK.
  - AGENTS.md's "the next one is 007" is stale; 024 is the latest.
  - Never edit 001 to 024.
- **Finish every lane with:**
  - `go build ./... && go vet ./...`
  - the tests of the touched packages
  - `golangci-lint run` (2.14) clean
  - `make templint` if templ changed
  - the openapi diff check if a route changed
- **Commits:**
  - `fix:` for fixes and polish, `feat:` for new capability.
  - One line, no AI attribution.
  - Add files by name (`git add path ...`), never `-A`.
- **Never print secrets.** That covers keys, tokens, join commands and rendered `:template` file contents, in logs and in test output.
- **UI copy:** no em dashes, terse, one short sentence per hint.
- **Style:** match the neighbouring code. Mark a deliberate shortcut with a `ponytail:` comment naming its ceiling.
- **Layering stays as AGENTS.md says:**
  - Handlers bind input, call one orchestrator method and render.
  - Decisions (behind, too old, offline, cert note, DNS verdicts) are made in `internal/service`; `.templ` files only display them.
  - Every container op is a job (rule 10). Remove node and Back up node are jobs.
  - No new flow and no new flow-to-flow edge: the node backup is sequenced in the service, calling `flow/backup` and `flow/container`.

## Lanes and file ownership

A file belongs to exactly one lane. Where another lane needs it, the Order section says when.

| Lane | Items | Owns |
|---|---|---|
| **R lifecycle** | schema (first commit), 5, 7 (panel side), 7b, 8 | new `internal/db/migrations/025_node_lifecycle.{up,down}.sql`, new `internal/db/node_lifecycle_test.go`, `internal/db/cascade_test.go`, `internal/service/internal/store/{volumes.go,backups.go,store.go}`, new `store/backup_groups.go`, `leaf/volume/volume.go`, `leaf/backup/backup.go`, `flow/backup/backup.go`, `internal/service/{backup.go,nodeadmin.go,defaults.go}`, new `internal/service/{noderemove.go,nodebackup.go,ingressswitch.go}`, `internal/ui/dialog/delete_level.go`, `internal/api/routes.go` (first), `internal/api/handler/v1/nodes.go`, `internal/service/nodeapi.go` (first), `cmd/stackr/{node.go (first),placement.go}`, `docs/openapi.json` (first), `internal/service/jobs.go` (after G) |
| **U upgrades** | 3 (all but display), 7 (box side) | `cmd/stackrd/{main.go,Dockerfile}`, new `cmd/stackrd/hosthelper.go`, `cmd/stackr-install/{main.go,join.go}`, new `cmd/stackr-install/uninstall.go`, `internal/worker/recover.go`, new `internal/service/workerupgrade.go`, `internal/service/{systemimages.go,join.go,admin.go}`, `internal/service/internal/flow/upgrade/*`, `internal/service/nodeclients.go` (after G), `internal/service/jobs.go` (after G and R) |
| **F files and logs** | 4, 6 (log cap) | `internal/service/internal/docker/{docker.go,containers.go,volumes.go,integration_test.go}`, `internal/service/internal/dockerfake/fake.go`, `internal/service/docker.go`, `flow/deploy/{mount_files.go,deploy.go}`, `leaf/tile/world.go`, new `leaf/tile/files.go`, `internal/installspec/{installspec.go,registry.go}`, `cmd/stackr-install/hostcheck.go` |
| **G gates** | 6 (disk), 10 (backend) | `internal/service/{dockerrouter.go,nodeclients.go (first),orchestrator.go,jobs.go (first),tile.go,envsync.go,run.go,wiring.go}`, new `internal/service/nodegate.go`, `flow/jobs/jobs.go`, `leaf/job/job.go`, `internal/worker/extras.go`, `internal/middleware/access.go` |
| **D1 admin drawer** | display of 3, 7, 7b, 8, 9 in the admin drawer | `internal/ui/drawer/admin/{nodes.templ,admin.templ,nodes_test.go}`, `internal/web/handler/admin/{nodes.go,handler.go}` and their tests, `internal/service/nodelive.go` |
| **D2 map, banner, read surfaces** | display of 3 and 9 on the map, 10 banner, skipped state, API and CLI reads | `internal/ui/cluster/*`, `internal/web/handler/cluster/*`, `internal/service/cluster.go`, `internal/ui/components/{shell.go,layout.templ}`, `internal/web/render/render.go`, `internal/web/server.go`, `internal/ui/drawer/tile/tile.templ`, `internal/web/handler/setup/handler.go`, `internal/service/nodeapi.go` (after R), `cmd/stackr/{node.go (after R),cmds.go}`, `internal/api/routes.go` (after R), `internal/api/handler/v1/admin.go` |

### Order

1. **Phase 0, merged first, both small:**
   - **R commit 1:** migration 025, the store changes and their tests. G's `skipped` state, R's groups and the item 5 orphan rows all sit on it.
   - **F commit 1:** the Docker wrapper seams: `ContainerSpec.Mounts`, `ContainerSpec.PidHost` and the log cap in `Create` and `toolContainer`. U's helper needs `PidHost`.
2. **Phase 1, in parallel:**
   - U: everything except its `nodeclients.go` hook and its `jobs.go` lines. Write the rollout against a test seam and wire it in phase 2.
   - F: everything.
   - G: everything.
   - R: everything except its `jobs.go` lines. Test the job bodies (`runRemoveNode`, `runNodeBackup`) by calling them directly.
3. **Phase 2:**
   - `jobs.go`: R adds its two kinds after G merges, then U adds its kind after R merges.
   - U adds its reconnect hook in `nodeclients.go` and uses G's `allowOld(ctx)` after G merges.
4. **Phase 3:** D1 and D2 start once R, U and G have merged. They run in parallel; their files are disjoint.
   - D2's edits to `nodeapi.go`, `cmd/stackr/node.go` and `routes.go` come after R's.

### Cross-lane contracts (names fixed here so lanes can work blind)

- **Settings keys:**
  - `panel_version` and `panel_prev_version`: U writes them at boot. When `panel_version` differs from `cfg.Version`, the old value moves to `panel_prev_version`.
  - `worker_rollout_target`: the version an admin started a rollout to. U writes it on the Update click (release path) and on "Update workers".
  - `worker_rollout_stopped`: holds a failure message; empty means not stopped.
  - `worker_upgrade.<nodeID>`: JSON `{version, state, error, at}`, where state is one of `waiting`, `upgrading`, `done`, `failed`, `skipped`.
  - `node.seen.<nodeID>`: unix seconds. G writes it when a node flips to offline.
- **G exports, inside package service:**
  - `allowOld(ctx) context.Context`: router calls carrying it skip the too-old refusal.
  - `o.lastSeen(id) time.Time`.
  - `o.nodeUp(ctx, nodeID) error`.
  - `o.EnvNodeDown(ctx, envID) (NodeDown, bool)`, where `NodeDown{Name string; LastSeen time.Time}`.
- **U exports:**
  - `o.workerBehind(n store.Node, f *worker.Facts) bool`.
  - `o.WorkerRollout(ctx) (Rollout, error)` and `o.UpdateWorkers(ctx, by) (Job, error)`.
  - The helper commands below.
- **Host helper (U builds, R and U run it):** a container from `localhost:5000/stackr.system/stackr:<tag>` on the node, with:
  - `Name: "stackr-host-helper"`, label `stackr.role=host-helper`
  - `Privileged`, `PidHost`, `HostNetwork`
  - `Volumes: ["/:/host"]`, `Restart: "no"`
  - It runs one of two commands:
    - `stackrd worker-swap --deadline 150s`
    - `stackrd host-run uninstall --yes`
- **R exports:**
  - `o.BackupNode(ctx, by, id) (Job, error)` and `o.BackupEveryNode(ctx, by) ([]Job, error)`.
  - `o.NodeBackups(ctx, id) ([]BackupGroupView, error)`.
  - `o.RemoveNode(ctx, by, id) (Job, error)`.
  - `o.IngressSwitchPlan(ctx, by, ref) (IngressPlan, error)` and `o.SetDefaultIngress(ctx, by, ref, confirm)`.

---

## Migration 025_node_lifecycle (lane R, commit 1)

One file pair; the up is three blocks.

1. **Item 5, orphaned volumes:**
   - Add `ALTER TABLE volumes ADD COLUMN node_id TEXT NOT NULL DEFAULT ''`. This is where an orphan's data lives once its env row is gone.
   - Drop and recreate `orgs_scope_cascade`, `stacks_scope_cascade` and `environments_scope_cascade` (current text in `010_env_tiers.up.sql`, lines 43 to 63). Replace `DELETE FROM volumes ...` with:
     ```sql
     DELETE FROM backup_schedules WHERE volume_id IN
         (SELECT id FROM volumes WHERE scope_kind = 'env' AND scope_id = old.id);
     UPDATE volumes SET orphaned_at = COALESCE(orphaned_at, CURRENT_TIMESTAMP),
         node_id = CASE WHEN node_id = '' THEN old.node_id ELSE node_id END
     WHERE scope_kind = 'env' AND scope_id = old.id;
     ```
   - For org and stack, use `node_id` `''`, meaning the controller (`scopeNode` already sends those scopes there).
   - The schedules go because `volumeFacts` walks to an org that no longer exists. The orphan sweep takes the last backup.
2. **Item 7b, backup groups:**
   ```sql
   CREATE TABLE backup_groups (
       id          TEXT     PRIMARY KEY,
       node_id     TEXT     NOT NULL,  -- no FK: groups outlive a removed node
       node_name   TEXT     NOT NULL,
       label       TEXT     NOT NULL,  -- "node h3, 14 Oct"
       reason      TEXT     NOT NULL CHECK (reason IN ('manual', 'remove')),
       dest_id     TEXT     NOT NULL,
       warning     TEXT     NOT NULL,  -- "" or the local-fallback warning
       status      TEXT     NOT NULL,
       error       TEXT     NOT NULL,
       created_at  DATETIME NOT NULL,
       finished_at DATETIME
   );
   ALTER TABLE backup_runs ADD COLUMN group_id TEXT REFERENCES backup_groups (id) ON DELETE SET NULL;
   CREATE INDEX backup_runs_group ON backup_runs (group_id);
   ```
3. **Item 10, the `skipped` job state:** rebuild `jobs` with `state IN (..., 'cancelled', 'skipped')`, copy the rows and recreate `idx_jobs_state`. No table references `jobs`, so no triggers are involved.
4. **Down:**
   - `UPDATE jobs SET state = 'cancelled' WHERE state = 'skipped'` before the rebuild back, or the copy fails the old CHECK.
   - Drop `backup_runs.group_id` (as `011_nodes.down.sql` drops FK columns), then `backup_groups`.
   - Restore the three triggers' `DELETE FROM volumes` text from 010, then drop `volumes.node_id`.
5. **Test first:** `internal/db/node_lifecycle_test.go`, following `node_exits_test.go`:
   - At 024, seed an org, stack, env with `node_id='n1'`, an env volume with a schedule, and a stack volume. Migrate one step up.
   - Deleting the env leaves the volume row with `orphaned_at` set, `node_id='n1'` and no schedule.
   - Read through `servicetest.Store`, `store.Volumes.Get` parses `OrphanedAt` (CURRENT_TIMESTAMP's format) and `volume.Leaf.Expired` returns it after the retention.
   - A `skipped` job row inserts.
   - A backup run with a `group_id` inserts, and deleting the group nulls it.
   - Down then up round-trips with a `skipped` row present.
   - `cascade_test.go`: its positional `INSERT INTO volumes VALUES (...)` gains the column. Org, stack and env deletes now orphan volume rows instead of dropping them.
6. **Store:**
   - `Volume.NodeID string db:"node_id"`.
   - `BackupRun.GroupID *string db:"group_id"`, plus `BackupRunStore.ListByGroup`.
   - New `BackupGroupStore` (Create, Get, Update, ListByNode) in `backup_groups.go`, registered in `store.go` and `Tx`.
   - `VolumeStore.ListByNode(nodeID)` (orphans with that `node_id`).

**R commit 1:** `feat: deleted envs orphan their volumes; backup groups; skipped job state (migration 025)`

---

## Lane U: worker upgrades (item 3) and the box side of item 7

### What exists
- `flow/upgrade.Upgrade` pulls, archives and launches `stackr-upgrader` (`panel.Launch`, `stackrd upgrade-swap`). The job dies with the old container, so nothing after the swap can run in `kindUpgrade`.
- `publishSystemImages` pushes `stackr.system/stackr:<tag>` at every boot, and `markStackrPublished` fires once it is in.
- Workers pull system images from `localhost:5000` through their registry front (`stackrSystemRef`). h3 gets it over the tunnel through h4.
- `Facts.Version` is reported, but empty from workers built before the UI round.
- `stackr-install join` already has `--version`, but `CreateJoinToken`'s `Command` (`join.go`, `mintJoin`) never passes it. Rejoin goes through `mintJoin` too.
- `fetchWorker` extracts only `/stackrd`. The image has no `/stackr-install` (`cmd/stackrd/Dockerfile`).
- Dev builds: `Upgradable()` is false and `parseTag` rejects `-dev.N`. The rig moves the controller with `scripts/rig.sh upgrade`.

### Design
- **The swap runs on the node, through the Docker pass-through every worker already has.** No new worker endpoint, so today's workers upgrade too.
  - The helper container (contract above) runs `stackrd worker-swap`, which:
    1. Copies its own `/stackrd` and `/stackr-install` to `/host/usr/local/bin/*.new`.
    2. Keeps the current ones as `*.prev` and renames the new ones in.
    3. Runs `chroot /host systemctl restart stackr-worker.service`. `PidHost` is needed: systemctl ignores requests when it detects a chroot unless `/proc/1/root` is the host's.
    4. Sleeps `--deadline`, counted from the restart.
    5. If it is still alive at the deadline, puts `*.prev` back, restarts and exits 1.
  - **Commit is the controller removing the helper.** The controller removes it once the node answers `Facts` over the tunnel with `Version == target` (the worker only listens on its tunnel address, so an answer means the tunnel is up).
  - The controller gives up at 120 s. The helper deadline is 150 s, so the two never race.
- **Rollout, in `workerupgrade.go`, as job `kindWorkerRollout` (lock `worker-rollout`):**
  - Order: nodes that others are reached through (`mesh.Via`) go last, then by name. On the rig that is ct110, h3, vm106, h4: h3 is upgraded while h4 is still up.
  - Skip the controller. Skip offline nodes (state `skipped`, "offline, upgrades when it reconnects").
  - Refuse a node whose `Facts.Arch` differs from the controller's `runtime.GOARCH`: "h9 is arm64; the stackr image is amd64 only". Mark it `ponytail:` (single-arch image).
  - Per node:
    1. Pull the image on the node (router `Pull` with `allowOld(ctx)`).
    2. Remove any leftover helper.
    3. Run the helper and poll `nodeExtras(id).Facts` every 5 s for 120 s.
    4. On success, StopRemove the helper.
    5. On failure, `Wait` for the helper's exit, mark `failed` and set `worker_rollout_stopped` to "<node> failed to upgrade, rolled back". Stop the rollout.
  - Under `cfg.FakeHost` the rollout is a no-op that says "fake workers are not upgraded". The helper bind-mounts `/` of whatever host runs it.
- **Triggers:**
  - `Upgrade(tag)` (admin.go) sets `worker_rollout_target = tag` before queueing `kindUpgrade`. On boot, `markStackrPublished` enqueues the rollout when the target equals `cfg.Version` and the rollout is not stopped.
  - `UpdateWorkers(ctx, by)` is the Update tab button. It clears `worker_rollout_stopped`, sets the target to `cfg.Version` and enqueues. This is the dev and rig path, and Retry.
  - Reconnect: `nodeclients.go` (after G) calls `o.workerBack(ctx, n)` when a node flips to online. It enqueues the rollout when the target equals `cfg.Version`, the rollout is not stopped and the node is behind.
- **Version rules (no ordering needed, so dev tags just work):**
  - `behind`: a worker whose `Facts.Version != cfg.Version`.
  - `too old`: neither `cfg.Version` nor `panel_prev_version`. An empty version counts as too old. G enforces this in the router; U only writes the keys.
- **Join:**
  - `mintJoin`'s command appends `--version <cfg.Version without v>` unless the build is plain `dev`.
  - `JoinStatus` gains `NodeName`. `join.go` (installer) writes `<state-dir>/node.name` and also extracts `/stackr-install` to `/usr/local/bin/stackr-install`.
  - The Dockerfile builds and copies `/stackr-install`.
  - The first join still pulls from ghcr.
- **Box side of item 7:**
  - New `cmd/stackr-install/uninstall.go`, `stackr-install uninstall [--purge] [--yes]`. It:
    - stops and disables `stackr-worker.service` and removes the unit;
    - removes containers labelled `stackr.managed=true` plus `stackr-proxy`;
    - removes networks labelled `stackr.managed=true`;
    - removes every iptables rule and chain matching `STACKR-` across the filter, nat, raw and mangle tables (parse `iptables-save`), including the jumps from `DOCKER-USER` and `PREROUTING`;
    - deletes `stackr0` and the exit WireGuard links (names from the saved mesh state) and their route tables.
  - Volumes and images stay.
  - `--purge` also removes stackr volumes (labelled, plus `stackr-caddy`), stackr images and the state dir. It asks for the node name read from `node.name`, falling back to the node id in `node.crt`'s CN for boxes joined before this change. `--yes` never skips the purge prompt.
  - It refuses on a controller (`/var/lib/stackr/install.json` present and no worker state). See open question 4.
  - `stackrd host-run uninstall --yes` (in `hosthelper.go`) copies its `/stackr-install` to `/host/usr/local/bin/` and runs `chroot /host systemd-run --collect --unit stackr-uninstall /usr/local/bin/stackr-install uninstall --yes`. The transient unit outlives the helper container, which uninstall then removes.
- **A refused worker gives up (`internal/worker/recover.go`):**
  - `recoverOnce` returns a typed error on HTTP 403 (`RecoverNode`'s `errs.Refusedf` maps to 403 in `middleware.HTTPError`). Branch on the status, never the text.
  - `recoverLoop` keeps the first refusal time. After 24 h it logs "removed from cluster, run stackr-install uninstall" once and stops. A 200 resets it.

### Test first
- `cmd/stackrd/hosthelper_test.go`: `worker-swap` against a temp dir standing in for `/host`, with the systemctl runner injected:
  - The binaries are swapped, and `.prev` holds the old ones.
  - Restart is called once.
  - The deadline elapses: the old binaries are back and restart is called twice.
  - Cancelled before the deadline (the helper was removed): nothing is restored.
- `internal/service/workerupgrade_test.go` (servicetest, fake Docker, fake extras facts):
  - Order puts the via node last.
  - Offline gives `skipped`.
  - Arch mismatch is refused.
  - When the version shows up, the helper is removed and the node is `done`.
  - When it never shows up, the node is `failed`, `worker_rollout_stopped` is set and later nodes stay `waiting`.
  - `UpdateWorkers` clears the stop.
  - Boot with the target equal to the version enqueues; a different target does not.
  - The `panel_prev_version` bookkeeping across two boots.
- `internal/service/join_test.go`: the command carries `--version 0.6.0-dev.3` for that build and no flag for `dev`; `JoinStatus.NodeName` is set.
- `cmd/stackr-install/uninstall_test.go`, with the runner in dry mode:
  - The command list (systemctl, docker filters, the iptables-restore payload built from a canned `iptables-save` holding STACKR and non-STACKR rules, where only STACKR goes).
  - `--purge` refuses a wrong name.
  - It refuses on a controller.
- `cmd/stackr-install/join_test.go`: `node.name` written; `/stackr-install` extracted.
- `internal/worker/recover_test.go`: 403 for 24 h of fake clock stops the loop and logs once; a 500 does not count.

**U commits:**
- `feat: workers upgrade from the cluster registry, one at a time, rolling back on the node`
- `feat: join and rejoin commands carry the panel's version`
- `feat: stackr-install uninstall, and the panel can run it on a node`
- `fix: a worker removed from the cluster stops asking after a day`

---

## Lane F: config files in volumes (item 4), log cap (item 6)

### What exists
- `flow/deploy/mount_files.go` `fileBinds` writes `<DataDir>/files/<tile>/<key>/<n>` on the controller and returns host binds. On a worker the bind names a path that does not exist there, which is the bug.
- `pruneFolders` and `pruneFiles` clean those folders.
- `docker.ContainerSpec` has only `Volumes []string` (binds). Subpath needs `mount.VolumeOptions.Subpath` (SDK v28.5.2 has it; API 1.45, Docker 26).
- `UntarVolume(vol, gzipped tar)` already exists.
- `Create` sets no `LogConfig`. `toolContainer` calls `ContainerCreate` directly.
- `minDocker = "25.0"` in `cmd/stackr-install/hostcheck.go`.

### Changes
- **Commit 1 (phase 0), in `docker.go` and `containers.go`:**
  - `ContainerSpec.Mounts []Mount{Volume, Subpath, Target string; ReadOnly bool}`, mapped to `HostConfig.Mounts` (type volume).
  - `ContainerSpec.PidHost bool`, mapped to `PidMode: "host"`.
  - `HostConfig.LogConfig{Type: "json-file", Config: {"max-size": "10m", "max-file": "3"}}` in `Create` and `toolContainer`. Add the comment `// ponytail: one cap for every container stackr creates; the buildx builder container keeps the daemon default.`
  - `dockerfake` records Mounts, PidHost and LogConfig.
- **Files (new `leaf/tile/files.go`; the tile leaf owns the tile's world objects):**
  - `WriteFiles(ctx, t, key string, tgz []byte) (vol string, err error)`:
    - The name is `stackr-files-<first 8 of tile id>-<first 12 of sha256(key)>`, with label `stackr.files.tile=<id>`.
    - It refuses when `HostInfo().Version < 26`: "files: needs Docker 26 or newer on this env's node (it runs 25.0.3)".
    - An existing volume counts as written only if it holds `/.stackr-done`, checked with a one-line tool container. Otherwise it runs `UntarVolume`; the tar's last member is `.stackr-done`.
  - `PruneFiles(ctx, t, keep string)`: removes every labelled volume but `keep` and ignores in-use refusals. `Teardown` calls it with `""`.
  - The tile leaf's `Docker` interface gains `HostInfo`, `CreateVolume`, `UntarVolume`, `RemoveVolume`, `ListVolumes` and `InspectVolume`. The router already has them all.
- **`fileBinds` becomes `fileMounts`:**
  - Builds the same slot layout (`<n>/...`) as an in-memory gzipped tar. Template expansion is unchanged.
  - Calls `Tiles.WriteFiles` and returns `[]docker.Mount`: `Subpath "<n>"` for a folder line, `"<n>/<base>"` for a file line, `ReadOnly: true`.
  - `pruneFolders` calls `PruneFiles(keep)` and `os.RemoveAll(<DataDir>/files/<tile>)`. Add `// ponytail: drops the pre-volume folder; delete this line a release later.`
- **`deploy.Flow` gets `Preflight func(ctx context.Context, e store.Environment, log io.Writer) error`.** `Run` calls it first, and nil means none. G wires it in `orchestrator.go`.
- **Installer:**
  - `minDocker = "26.0"` (comment: volume subpath).
  - `installspec.Container` gains `LogOpts []string` (`max-size=10m`, `max-file=3`), set by `Panel`, `Proxy`, `NodeProxy` and `Registry` and emitted by `RunArgs` as `--log-opt`. It is in the hash, so the proxy and registry are recreated once at the next upgrade (seconds of blip, as for any spec change).

### Test first
- `internal/service/internal/docker/integration_test.go` (build tag `integration`): a file subpath (`0/app.conf`) and a folder subpath mount read-only on the local daemon. Write this first: file lines depend on it.
- `flow/deploy` test with the fake:
  - A tile with a file line and a folder line gets two `Mounts` with the right subpaths and `ReadOnly`, and no `<DataDir>/files` path in `Volumes`.
  - A second deploy with the same key writes nothing.
  - A param change makes a new volume and prunes the old one.
  - The template output never appears in the fake's log.
- `leaf/tile` test: Docker `25.0.3` is refused, and the message names the version.
- `docker` unit test: a `Create` spec carries the json-file cap.
- `installspec_test.go`: `RunArgs` has `--log-opt max-size=10m --log-opt max-file=3`.
- `hostcheck_test.go`: 25.0 is too old.

**F commits:**
- `feat: containers get a 3 x 10 MB json-file log cap; specs carry volume mounts and host pid`
- `fix: config files reach tiles on worker nodes, in read-only volumes on the env's node`
- `feat: installer requires Docker 26 and caps the panel's own container logs`

---

## Lane G: gates (item 6 disk, item 10 backend)

### What exists
- `DockerRouter.pick` and `nodeExtras` answer `"%w: no connection to node <id>"` (`errs.ErrBusy`). `wiring.go:141` does the same. `middleware.HTTPError` turns `ErrBusy` into a bare 503 with no message.
- `meshSvc.at` (last seen) is in memory only, zeroed on every panel restart.
- `refreshNodeClients` flips status after `failsBeforeOffline = 2` misses, which is about 60 s.
- `Facts.DiskFree` exists and `DiskTotal` does not.
  - On the controller, the local worker runs inside the panel container, where `Statfs(DockerRootDir)` fails.
  - With no mesh, `controllerFacts` has no disk at all.
- Scheduled backups and crons enqueue in `orchestrator.go` (schedule drivers `Backup`, `Cron` through `queueRun`).
- The jobs table has no `skipped` state until 025.

### Changes
- **New `nodegate.go`:**
  - `type NodeDownError struct{ Name string; LastSeen time.Time }`. It `Is(errs.ErrBusy)`. The message is "h3 is offline, last seen 14 min ago" or "h3 is offline, never seen".
  - `o.lastSeen(id)`: `mesh.seenAt`, else the `node.seen.<id>` setting.
  - `o.nodeUp(ctx, nodeID)`: `NodeDownError` for a worker that is not online.
  - `o.diskCheck(ctx, nodeID) (warn string, err error)`:
    - Under 1 GB free: `errs.Conflictf("%s has %s free on its Docker disk; deploys need 1 GB. Free space and retry.", name, size)`.
    - Under 10% of `DiskTotal`: a warning "warning: h3 has 8% (3.1 GB) free on its Docker disk".
    - Free or total of 0: "disk not checked: h3 does not report its disk". Never a refusal.
  - `o.EnvNodeDown(ctx, envID)`.
- **Choke points:**
  - The router gets `Gate func(ctx context.Context, nodeID string, connected bool) error`, called in `pick`. Not connected gives `NodeDownError`.
  - Connected but too old (G reads `Facts.Version` from the probe in `refreshNodeClients` and keeps a `tooOld` map) gives `errs.Conflictf("%s runs stackr %s, too old for this panel (%s); upgrade it from the Update tab.")`, unless `allowOld(ctx)`.
  - `nodeExtras` and `wiring.go` return `NodeDownError`. Extras and mesh calls are never refused for age: tunnels must keep working.
  - `HTTPError`: `errors.As(err, *NodeDownError)` gives 503 with the message.
- **Refuse up front:**
  - `enqueuePromote` (this covers Promote, Rollback, and the auto-promotes in push and image watch, which join the error into their job log), `tile.go` Deploy, Restart, Start and Stop, `envsync.go`, and manual `queueRun`.
  - They call `nodeUp`, then `diskCheck`'s refusal, before any row is written.
  - `RestoreLostEnv` (R) targets a node; R calls `nodeUp` there after G merges if it does not enqueue through `enqueuePromote`.
- **At deploy:** wire `deploy.Flow.Preflight` to log `diskCheck`'s warning and return its refusal. Use `onceLog`-style dedupe so a 10-tile promote warns once.
- **Skipped:**
  - `job.Skipped = "skipped"` (Terminal for free).
  - `jobs.Runner.Skip(ctx, kind, lock, payload, reason)` is `Create` plus `Finish(skipped, reason)`.
  - The schedule drivers `Backup` (volume's node through `volume.Leaf`'s node) and `queueRun` with trigger schedule record "skipped: h3 offline" instead of enqueueing.
- **Last seen persisted:** when `refreshNodeClients` flips a node to offline, it writes `node.seen.<id>` with `mesh.seenAt(id)` (or now).
- **`worker/extras.go`:**
  - `Facts.DiskTotal` (`Blocks * Bsize`).
  - When `Statfs(DockerRootDir)` fails, fall back to `Statfs(StateDir)`. Add `// ponytail: the controller's worker runs in the panel container; its data dir is the docker root's disk on a default install.`
  - G's `diskCheck` also statfs's `cfg.DataDir` for a controller with no mesh facts.

### Test first
- `nodegate_test.go` (servicetest):
  - An offline worker: Promote, tile Deploy and EnvSync are refused with "h3 is offline, last seen", and no job row is written.
  - Free 0.8 GB is refused, naming the node. 5% passes and the job log has the warning. Unknown disk passes with "disk not checked".
  - A scheduled backup and a scheduled cron on an offline node write one `skipped` job with error "skipped: h3 offline".
  - Last seen survives a new orchestrator on the same store.
- `dockerrouter` test: too old refuses `Run` and `allowOld(ctx)` passes. Version `""` is too old; `panel_prev_version` is allowed.
- `middleware` test: `NodeDownError` gives 503 with its message.
- `worker` test: `/v1/facts` has `disk_total`.

**G commits:**
- `feat: deploys to an offline node are refused up front, naming the node and when it was last seen`
- `feat: scheduled jobs on an offline node are recorded as skipped`
- `feat: deploys warn under 10% free disk and refuse under 1 GB`
- `feat: workers more than one version behind are refused new calls`

---

## Lane R: node lifecycle (items 5, 7 panel side, 7b, 8)

### Item 5: env delete orphans its volumes
- The migration does the marking. Add a `volume.Leaf.at`: `v.NodeID != ""` wins over the scope lookup, so `HoldsData`, `Tar` and `Delete` reach the node after the env row is gone.
- `flow/backup.Orphans` is unchanged: it sweeps after `orphan_retention_days`, takes a last backup and deletes.
- `dialog.DeleteEnv`, `DeleteStack` and `DeleteOrg`: `Kept` gains "its volumes, for the orphan retention; a last backup is taken before they go".
- **Test first:** servicetest: DeleteEnv on a worker env keeps the volume row orphaned with its node. `Orphans` with retention 0 backs it up on that node (the fake records the node) and deletes it.

### Item 7: remove becomes a stop, back up, verify, uninstall job
- **`RemoveNode(ctx, by, id) (Job, error)`.** The refusals:
  - envs left (as today);
  - default exit rung (as today);
  - default ingress: "h4 is the default ingress; make another node the default ingress first.";
  - offline: `nodeUp` (open question 3).
  - It then enqueues `kindRemoveNode`, lock `node:<id>`.
- **`runRemoveNode` (`noderemove.go`):**
  1. List containers labelled `stackr.managed=true` on the node. Stop the running ones and remember them.
  2. Back up every Docker volume labelled `stackr.managed=true` there, except `stackr.files.tile` ones. Use one group (`reason remove`, label "node h3, 14 Oct, before remove"). A volume with a row (by name, including item 5 orphans) runs as that row; a move leftover has no row.
  3. Check that every run in the group is `done` with size > 0.
  4. On any failure, start the remembered containers and fail the job with "backup of <vol> failed; h3 not removed".
  5. Otherwise run the host helper `host-run uninstall --yes`. It must run before `meshSvc.RemoveNode`, which empties the spec and drops the tunnel.
  6. Delete the node's orphan rows (`ListByNode`), whose archives are in the group, then call `dropNode` as today.
- `backup.Leaf.Start`: a volume run may carry a nil volume id when a group is set. Add `NodePrefix(nodeName, groupID)`, never pruned.
- API: `DELETE /admin/nodes/:node` answers a Job (`Job(...)` like rotate). `stackr node rm` follows it. `nodeapi.go` `RemoveNodeRef` follows the signature.
- **Test first:**
  - A node with two labelled volumes (one orphan row, one row-less): the group has two `done` runs, the helper ran `host-run uninstall --yes` before the mesh removal, and the node row and orphan row are gone.
  - A failing tar: the containers are started again, the job fails with the volume's name, and the node is still there.
  - The default ingress refusal.

### Item 7b: Back up node
- **`BackupNode(ctx, by, id) (Job, error)`** enqueues `kindNodeBackup`. The lock is `node:<id>` plus every tile id on the node, so deploys there wait.
- **`runNodeBackup` (`nodebackup.go`), in the service:**
  - Create a group: `reason manual`, label `node <name>, <2 Jan>`.
  - Destination: the first global non-local destination, else `localDest` with warning "No backup destination set; archives stay on the controller in <data>/backups." (open question 5).
  - Per env on the node, one at a time:
    1. Back up instance volumes with their dump method through `o.backup.Backup`.
    2. `o.container.Stop` each tile.
    3. Tar each plain volume (`Mode live`, nothing writes).
    4. `o.container.Start` each tile. This also runs on error, through a deferred start.
  - The controller also includes stack and org scope volumes.
  - `Spec.GroupID` threads into `Backups.Start`.
- **`BackupEveryNode`:** one job per online node, for the break glass box.
- **`NodeBackups(ctx, id) ([]BackupGroupView, error)`:** groups, newest first, each with per-volume rows (volume, env path, method, status, size, error).
- API:
  - `POST /admin/nodes/:node/backup` (op `admin.node-backup`, `node.manage`) answers a Job.
  - `GET /admin/nodes/:node/backups` (`admin.node-backups`).
  - CLI: `stackr node backup <node>`.
  - Regenerate openapi.
- **Test first:** an env with a managed postgres volume and a plain volume:
  - The dump runs before any stop.
  - The tiles stop once and start once.
  - The group has two runs with `group_id`.
  - With no global destination the group warning is set.
  - A failing tar still starts the tiles.

### Item 8: default ingress switch
- **`IngressSwitchPlan(ctx, by, ref) (IngressPlan, error)`.** It refuses:
  - an offline node (`nodeUp`);
  - a node with no public IP and no port forward (from `ClusterMap`: `Public` empty and `NoForward`): "h3 has no public IP and no port forward; the internet cannot reach it."
- The plan covers the panel domain (flagged) and the hosts of every route without an ingress pick, the same set `domain.caddy.go` routes to the default. Wildcards are checked through a probe name.
- Each host is resolved with an injected resolver (`o.resolve`, default `net.DefaultResolver.LookupIP`). Expected addresses are the node's public IPs, or its network's public address when it is a forward target.
- **`SetDefaultIngress(ctx, by, ref, confirm string)`:** the same refusals; confirm must equal the node name (`errs.Invalidf("confirm", "type %s to confirm", name)`).
- API: `GET /admin/default-ingress/plan?node=` (`admin.default-ingress-plan`); the PUT body gains `confirm`. CLI: `stackr ingress set --default h4` prints the plan and asks for the name; `--confirm h4` skips the prompt.
- **Test first:**
  - With a fake resolver, the panel host points elsewhere: listed, `OK=false`, flagged panel.
  - Offline is refused.
  - A public-less node with `NoForward` is refused.
  - The wrong confirm is refused.
  - The right confirm sets the setting.
  - `cmd/stackr` request JSON has `confirm`.

**R commits** (after commit 1):
- `feat: env, stack and org delete keep volumes as orphans for the retention`
- `feat: remove node stops, backs up, verifies and cleans the box`
- `feat: back up node, one env at a time, shown as one group`
- `feat: default ingress switch checks DNS and asks for the node name`

---

## Lane D1: admin drawer (after R, U, G)

- **Update tab** (`admin.templ` `UpdateView`, `handler.go`):
  - Per-worker rows from `WorkerRollout`: name, version, state, error.
  - "Update workers" whenever any worker is behind and no rollout is running; "Retry" after a stop. Both call `UpdateWorkers`.
  - The stop message on top: "h3 failed to upgrade, rolled back".
- **Node drawer overview** (`nodes.templ`, `nodes.go`, `nodelive.go`):
  - `NodeLive` gains `Behind bool`, `TooOld bool`, `CertExpired bool` and `CertDays int` (0 means not under 14 days). The rule lives in a pure `certNote(certs, now)` in `nodelive.go`.
  - `LastSeen` falls back to `o.lastSeen`.
  - Version row "0.6.0-dev.12, behind" or "too old".
  - Cert: "Cert expired, rejoin needed" with the Rejoin button at the top of the overview; "Cert expires in N days" in the Node card.
- **Remove:** the confirm warning is "Stops everything on h3, backs it up, checks the backups, then cleans the box and removes it. A failed backup stops here and starts the apps again." Kept: "the backups, and the volumes and images on the box". The answer shows the job.
- **Back up node:** a button in the overview, plus a Backups section listing groups ("node h3, 14 Oct", status, destination, warning) with per-volume rows in a native `<details>`.
- **Default ingress:**
  - The select posts to a preview: a host table with "points here" or "points to 1.2.3.4", and the panel domain first with "The panel moves to h4."
  - Then a `ConfirmView` with `Word` set to the node name.
  - Refusals show in the drawer's error banner.
- **Break glass:** `ConfirmView.Body` holds a pre-ticked checkbox "Back up every node first", included with `Include`. Ticked runs `BackupEveryNode` and answers "Backups queued; break glass once they finish." Unticked breaks glass as today (open question 2).
- **Test first** (`nodes_test.go`, `admin_test.go`):
  - The overview shows "Cert expired, rejoin needed" and a rejoin form for an expired cert, and "Cert expires in 9 days" at 9 days.
  - "behind" for an old version.
  - The ingress preview renders the panel host first.
  - Break glass with the box ticked queues backups and does not break glass.
  - The Update tab shows a failed row and Retry.
- **D1 commits:**
  - `feat: update tab shows worker rollout progress with retry`
  - `feat: node drawer shows version, cert expiry, backups and the remove steps`
  - `feat: default ingress switch previews DNS before the typed confirm`

## Lane D2: map, banner, read surfaces (after R, U, G)

- **`cluster.go`:** `ClusterNode` gains `Behind`, `CertExpired` and `CertDays` (using D1's `certNote` once both merge; if D2 lands first, D2 adds `certNote` and D1 rebases). `view.go` and `cluster.templ` show them on the card: "behind", "Cert expired, rejoin needed" (with a link that opens the node drawer), "Cert expires in N days".
- **Banner:**
  - A `NodeDown` middleware in `render.go`, mounted in `server.go` like `Waiting`. It leaves a lazy func, which `Shell` calls only when the URL has an env.
  - `Shell.NodeDown string` is filled from `EnvNodeDown`.
  - `layout.templ` shows "h3 is offline, last seen 14 min ago. Deploys here are refused until it is back." under the top bar, on env and tile pages.
- **Skipped state:** `tile.templ` statuses and `setup/handler.go` map `skipped` to a muted badge reading "skipped".
- **API and CLI:**
  - `NodeOut` gains `version` and `behind`; `stackr node show` prints `stackr <ver>` with "(behind)".
  - `POST /admin/upgrade/workers` (`admin.upgrade-workers`, `container.admin`) maps to `UpdateWorkers`.
  - `stackr admin upgrade-workers`.
  - Regenerate openapi.
- **Test first:**
  - `cluster_test.go`: a behind node's card says "behind", and an expired one has the rejoin text.
  - A layout render test: `Shell.NodeDown` renders the banner, and an empty one renders none.
  - `nodeapi_test.go`: `behind` is true for an old version.
  - `TestEveryRouteHasAVerb` covers the new op.
- **D2 commits:**
  - `feat: map cards show behind and cert expiry`
  - `feat: env and tile pages show a banner when their node is offline`
  - `feat: stackr admin upgrade-workers and node versions in the API`

---

## Rig proof (after release)

The rig has 5 nodes:
- 109: the controller, behind a home NAT, behind the front proxy 192.168.1.100.
- ct110 and vm106: on 109's LAN.
- h4: public.
- h3: Hetzner private network, reached through h4.

Releases are `make release RELEASE=v0.6.0-dev.N`, then `scripts/rig.sh upgrade 0.6.0-dev.N` for the controller.

1. **Bootstrap (dev.N, first build with this round):**
   - After `rig.sh upgrade`, the workers still run their old binaries.
     - The map and drawer show them "behind".
     - If they report no version, or one other than the controller's previous, they are "too old". `stackr deploy` of a tile on ct110 is then refused with "ct110 runs stackr ..., too old for this panel ...; upgrade it from the Update tab."
   - Click **Update workers**:
     - The rows go upgrading then done, one at a time, in the order ct110, h3, vm106, h4.
     - `ssh <node> /usr/local/bin/stackrd --version` prints `v0.6.0-dev.N`, and `/usr/local/bin/stackr-install` exists.
     - `docker ps -a --filter name=stackr-host-helper` is empty on every node.
     - Every tunnel is green, and a tile on each node answers.
2. **dev.N to dev.N+1 with the panel button:**
   - `rig.sh upgrade 0.6.0-dev.N+1`. The workers are behind but not too old, so a deploy on ct110 works.
   - Click **Update workers**; all four reach dev.N+1.
   - h3's helper pulled `localhost:5000/stackr.system/stackr:0.6.0-dev.N+1` over the tunnel through h4 (`docker images` on h3).
3. **Offline catch-up (dev.N+2):**
   - `ssh vm106 systemctl stop stackr-worker`, then `rig.sh upgrade 0.6.0-dev.N+2` and click Update workers.
   - vm106 shows "skipped: offline"; the others are done.
   - `systemctl start stackr-worker` on vm106. Within about 60 s it flips online, the rollout runs again by itself, and vm106 reaches dev.N+2.
4. **Forced failure and rollback (dev.N+3):**
   1. `rig.sh upgrade 0.6.0-dev.N+3`.
   2. Before clicking, on h3: `until docker ps -q --filter name=stackr-host-helper | grep -q .; do sleep 1; done; iptables -I INPUT -i stackr0 -p tcp --dport 7443 -j DROP`.
   3. Click Update workers.
   4. h3's row ends "failed" and the tab says "h3 failed to upgrade, rolled back"; vm106 and h4 stay waiting.
   5. About 150 s after h3's restart, `stackrd --version` on h3 prints dev.N+2.
   6. `iptables -D INPUT -i stackr0 -p tcp --dport 7443 -j DROP`.
   7. Click **Retry**: h3, vm106 and h4 reach dev.N+3.
5. **Too-old refusal (if step 1 did not show it):**
   - On ct110, while running dev.N+3, copy `stackrd` out of `localhost:5000/stackr.system/stackr:0.6.0-dev.N+1` (one of the two spares the registry keeps) into `/usr/local/bin/stackrd`, then `systemctl restart stackr-worker`.
   - Its card says "too old". A deploy there is refused with the too-old message, while extras and the tunnel stay up.
   - Update workers fixes it. If the restart flipped it offline, the reconnect catch-up fixes it first; that is fine too.
6. **Join version:** Add node and a rejoin print `--version 0.6.0-dev.N+3` (do not paste the command into notes; it holds a token).
7. **Config files (4):**
   - A tile with `files:` (a file line and a folder line) on h3 and on 109 deploys.
   - `docker volume ls --filter label=stackr.files.tile` on h3 lists one volume, and `docker exec` shows the files read-only.
   - A param change gives a new volume and the old one is gone. Deleting the tile removes them.
   - No `files/` folder grows on 109.
8. **Orphans (5):**
   - Delete an env on vm106 that has a volume. The confirm names the orphan retention.
   - The rig's sqlite (`scripts/rig.sh` py pattern) shows the row with `orphaned_at` and `node_id` set to vm106.
   - With `orphan_retention_days` at its minimum and the orphans job run, a backup run with trigger `orphan` exists and the Docker volume is gone on vm106.
9. **Logs and disk (6):**
   - On each node, `docker inspect -f '{{.HostConfig.LogConfig}}'` on a tile, on `stackr-proxy` and on the panel shows `json-file map[max-file:3 max-size:10m]`.
   - On h3, `fallocate` a file in `/var/lib/docker` down to under 10% free: a deploy logs the warning. Down to under 1 GB: it is refused naming h3. Remove the file afterwards.
   - `stackr node show 109` shows a disk total.
10. **Offline (10):**
    - `systemctl stop stackr-worker` on vm106.
    - After about 60 s, the env and tile pages there show the banner.
    - A deploy is refused with "vm106 is offline, last seen N min ago".
    - A scheduled backup of a vm106 volume (every-minute cron) shows a `skipped: vm106 offline` job.
    - `docker restart stackr` on 109: the banner still names the last-seen time.
    - Start the worker again.
11. **Back up node (7b):**
    - In h3's drawer, **Back up node**: the job backs up one env at a time, with the DB by dump (`.sql.gz.age`).
    - The env's tiles restart once.
    - The drawer shows "node h3, 14 Oct" with per-volume rows.
    - With no global destination, the warning shows and the archives sit in 109's `<data>/backups`.
    - The break glass dialog shows the box pre-ticked. Ticked, it only queues backups: do not proceed to break glass on the rig.
12. **Remove and uninstall (7) on vm106:**
    1. Move vm106's envs to ct110.
    2. With a global destination pointing at a bogus bucket, Remove: the job fails on the first volume, vm106's containers run again, and the node is still listed.
    3. Fix the destination and Remove again. The job goes stop, group backup, verify, uninstall, removed.
    4. On vm106 the service is disabled, `iptables-save | grep -c STACKR` prints 0, `ip link show stackr0` fails, and no stackr containers or networks remain. Volumes and images are still listed.
    5. `stackr-install uninstall --purge` asks for "vm106". A wrong name is refused. The right name removes the volumes, images and state.
    6. Rejoin vm106 with the printed command.
    - `stackr node rm 109` is refused (controller). The default ingress refusal is proved by the service test, because 109 is the rig's default ingress.
13. **Default ingress (8), preview only:** moving it would move the panel off 109's NAT and front proxy, so do not switch on the rig.
    - Pick h4: the preview lists the panel domain first, pointing at the home IP and not h4, plus the routes that enter by default. Cancel.
    - h3 is refused (no public IP, no forward). ct110 is refused (the forward goes to 109). vm106 is refused while its worker is stopped (offline).
14. **Cert expiry (9):** the rig's certs are fresh, so the card and drawer show nothing; the templ and service tests cover the texts. If a node shows "Cert expires in N days", note it.

## Open questions (each with a recommendation)

1. **What "more than one version behind" means.** Releases and dev builds have no ordering we can trust across a restore. **Recommend:** allowed means the panel's version or the one it ran before its last upgrade (`panel_prev_version`, written at boot). Anything else is too old, including an empty version. No version comparator is needed.
2. **Break glass with the box ticked.** Break glass makes workers distrust the controller, so backups after it would fail. **Recommend:** ticked only queues `BackupEveryNode` and says "break glass once they finish". The admin presses again unticked. A failed backup is visible in the groups and break glass does not wait on it.
3. **Removing an offline node.** It cannot be stopped, backed up or cleaned. **Recommend:** refuse with "h3 is offline (last seen ...); bring it back, or forget it if it is gone for good." Forget already covers lost nodes.
4. **`stackr-install uninstall` on a controller.** **Recommend:** refuse ("this box runs the panel; uninstall removes workers"). Removing a controller is a separate, rarer job.
5. **Node backup destination.** **Recommend:** the first global (server) destination that is not the local one, else the controller's local folder with the agreed warning. Per-org destinations are not used for a node-wide group.

## Decided (Claude, under darthvader's go 2026-10-14)

All five open questions take the recommendation: (1) allowed = panel version or panel_prev_version, else too old;
(2) the ticked break glass box only queues backups; (3) removing an offline node is refused (forget covers it);
(4) uninstall refuses on a controller; (5) the node backup goes to the first global non-local destination, else local with the warning.
Orchestration: phase 0 (R commit 1 + F commit 1) runs as one small lane, then R, F, G and U run in parallel, then the jobs.go wiring, then D1 and D2.


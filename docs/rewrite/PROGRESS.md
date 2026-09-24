# Rewrite progress

The only source of "where are we". Read this first, never ask. Tick items as
they finish, commit this file with the work. Anything needing darhvader goes
under `DECIDE:` at the bottom; keep working on what does not depend on it.

Branch: `rewrite`. Old code: worktree at `../stackr-old` (never mention it in
the new `AGENTS.md`). Plan: `REWRITE.md` at repo root, "Method" section has
the builder rules. Commits: plain, no co-author, no AI attribution, ever.

## Overnight run (Fable, planning session, 2026-09-24)

Approved by darhvader as "option A": write everything, park decisions, do not
wait. Sub-agents run on `model: "opus"`. Run finished 2026-09-24: sections
1–4 below are done; DECIDE items 1–10 wait for darhvader; build steps not
started.

### 1. Task lists → `docs/rewrite/tasks/step-N.md`

Source: REWRITE.md "Build order" row + the sections it names. One file per
step, numbered tasks, each with a "done when". Read one REWRITE.md section at
a time. Mark real decisions `DECIDE:` in the file and copy them to the block
at the bottom of this file.

- [x] step-0.md (agreed list below; task 1 and 6 re-checked against the real scaffold after the wipe)
- [x] step-1.md groundwork: schema design note first, then migrations A (releases/release_tiles/environments/jobs) and B, store, secrets, errs, Docker interface + fake, service.New, authz + middleware, flow/jobs, harness, settings catalogue
- [x] step-2.md infra wrappers: Docker (containers/networks/volumes/images/build/logs/exec), plus registry, git, vip (iptables DNAT), s3 + local destination, githubapp, Caddy admin client. Planner note: Build order row 2 says "Docker wrapper"; the other wrappers are the same rule-free kind and were put here so step 3 is only services.
- [x] step-3.md services: 15 leaves, 7 flows + scheduler, orchestrator verbs listed in `docs/rewrite/verbs.md`; includes self-upgrade flow (old `service/admin.go`, no carry-over row existed, added under rule 9) and `stackrd proxy`
- [x] step-4.md API + CLI
- [x] step-5.md installer + self-upgrade
- [x] step-6.md UI, whitelist: `log-pane`, `confirm-dialog`, `flash-toast`, `theme-toggle`. Dropped old JS: canvas/graph, metrics, xterm terminal, YAML code editor (see step-6.md "Not in v1")

Step 0 list (agreed with darhvader):
1. Rename binaries to `stackrd`, `stackr`, `stackr-install`: three `cmd/` dirs, Makefile, watch rule.
2. Fix AGENTS.md: both examples call a service, handlers check form shape only, store split by table, migrations editable until first install.
3. Layering rules into AGENTS.md: handlers see only `service.Orchestrator`, store and Docker under `service/internal/`, leaf reads own table only, flows pass facts down, auth only in middleware.
4. Handler-audit skill in `.claude/skills/`: flags store calls, Docker calls, domain rules, auth checks in a handler.
5. `depguard` in `.golangci.yml`: no sibling leaf imports, no `service/internal` outside `service/`.
6. Empty tree: `service/{leaf,flow,internal/{store,docker}}`, `ui/{components,pages,static/js}`.
7. Done when `make build`, `make lint`, `make test` pass and the skill runs.

### 2. Extracts → `docs/rewrite/extracts/<row>.md`

One Opus sub-agent per row of REWRITE.md "What comes over from the old code".
Agent reads only that row's files under `../stackr-old`. File header, five
lines: source path(s), commit `c2423f0`, what was taken, what was cut, where
each cut part belongs. Filters in order: the row (take/leave columns), the
layering rules (no store call, no Docker call outside the wrapper, no auth
check, no status decision; mark cuts `// extract: dropped X, belongs in
leaf/Y`), size (extract bigger than source = copied not filtered, redo).
Fable skims each one; darhvader skims in the morning.

- [x] extracts written, one per row: runtime, deploy, installer, secrets,
  varref, release-tilediff, stackconf, githubapp, workqueue, deploystate,
  tilelifecycle, runpolicy, access-revoke, envops, container-guard,
  org-rules, managedtiles, managedinstance, 001_initial, 002_api_key_org,
  settings, imagewatch-infra, imagewatch-service, backup-infra,
  svcerr-scheduler, netaddr, proxy-ref, admin-upgrade
- [x] gap extracts (rows whose TAKE pointed at the wrong files): tile-crud
  (`service/tile.go`), org-handler-rules (panel org handlers)
- [x] gap extracts: webhook (`/hooks/connectors/{id}`; no `installation`
  event receiver ever existed, install state came from the App setup
  callback), spec-build (engine → ContainerSpec)
- [x] gap extracts: cron-runner (both old schedulers are `robfig/cron/v3`;
  keep the library), tar-mechanics (bodies live in `infra/runtime`, not
  `infra/cluster`; `VerifyTar` is a local-file check, belongs in
  `flow/backup`)
- [x] gap extracts: cli-ref (385 lines of 6523), templ-ref (276 of 12958; what each screen shows, never how)
- [x] skimmed (Fable): headers, cut lists and sizes of all 33; every one
  under its source except the three tiny rows where the header outweighs
  the code. `stackconf.md` is 50% of its source, the grammar is the spec.
  darhvader: skim `varref`, `stackconf`, `managedtiles` yourself, they
  carry the most rewritten code.

Findings from the extracts that changed a task list (already applied):
- `secrets`: old `Encrypt` fails open (no key = plaintext stored). Step 1
  task 5 must refuse instead.
- `githubapp`: `connectorForTile` refused a connector from another org.
  Security rule, keeps its test, goes to `leaf/connector` (step 3 task 11).
- `admin-upgrade`: `CheckUpgrade` reads GitHub's latest-release API, not a
  registry tag list. Keep as today (step 3 task 22 text fixed).
- `001_initial`: FKs are enforced (hamr sqlite turns the pragma on); the
  four managed tables have no FKs at all. `registries` has no `org_id`;
  credentials get one. `acme_email` becomes a settings key. Panel
  self-backup needs `backup_runs.volume_id` nullable + a `kind`.
- `runpolicy`: the old registry gates 3 features, not per-field; B26's
  whitelist is new structure.
- `workqueue`: no per-job log file existed; `flow/jobs` log path is new.
- `imagewatch-infra`: tag listing, rate-limit handling and typed errors do
  not exist in the old client; all new work in step 2 task 4.
- `installer`: no restore-on-failed-swap script exists anywhere; step 5
  task 3 "restore script as today" is new work. `TRAEFIK_*` env names
  renamed to Caddy (no installs exist).

### 3. Wipe → Opus sub-agent, Fable verifies

Delete everything on `rewrite` except `.claude/`, `.mcp.json`, `CLAUDE.md`,
`.gitignore`, `docs/rewrite/`, `REWRITE.md`. Run `hamr new` (temp dir + move
in if it refuses a non-empty dir). Module name `stackr`.

Verify before push: `make build`, `make lint`, `make test` pass;
`git diff master --stat` = old code gone + scaffold + docs/rewrite; kept
files untouched; `docs/rewrite/` has REWRITE.md copy or link, tasks/,
extracts/, this file.

- [x] wiped and scaffolded: hamr 0.38.0 (`make install` then pinned the
  CLI to 0.38.1), command needed `--locale=false --websocket=false
  --alpine=false --stripe=false` on top of the agreed flags or it blocks on
  a TTY prompt. `.agents/skills/` (hamr's own agent skills) is gone and the
  scaffold did not recreate it; hamr docs now sit in `docs/llms.txt`.
  `.github/workflows/release.yml`, `.gitleaks.toml`, `.semrelrc`, `LICENSE`
  went with the wipe; step 5 `make release` brings release CI back.
- [x] verified: `make build` and `make test` pass (17 packages, no tests
  yet); `make lint` fails on two lines of the scaffold's `cmd/site/main.go`,
  fixed by step 0 task 1 when the file moves. Kept files byte-identical.
  `git diff master --stat`: 909 files, old code gone. `hamr dev` is NOT
  running; darhvader starts it.
- [x] committed and pushed (`git push -u origin rewrite`)

### 4. Hand-off

- [x] this file updated with a "start here" line for the Opus step 0 session

**START HERE (Opus, step 6):** branch `rewrite-step-6`, stacked on
`rewrite-step-4` (PR #11, GitHub stack #9); step 5 runs in parallel on
`rewrite-step-5` and is re-stacked after. Step 6 runs as: session A =
tasks 1–4 (foundation), then sessions B (auth/org/stack pages) and C
(env/tile/admin pages) in parallel worktrees, then session D = tasks
11–13. Read `REWRITE.md` "Method", `docs/rewrite/verbs.md`,
`docs/openapi.json`, then `docs/rewrite/tasks/step-6.md`. Do not read
`../stackr-old`; `docs/rewrite/extracts/templ-ref.md` is the only view of
the old screens.

## Build steps (one fresh Opus session + one stacked PR each)

Runner key: `[F+O]` = Fable planning session drives an Opus sub-agent,
Fable verifies the gates and launches the next step; `[O]` = a plain Opus
session started by darhvader from the START HERE line, no Fable.

- [x] [F+O] step 0 scaffold and docs
  - [x] step 0 / task 1: `cmd/stackrd`, `cmd/stackr`, `cmd/stackr-install`;
    `run() error` + `--version` fix both lint findings. `installcli` =
    `go install ./cmd/stackr`, `installer` = `bin/stackr-install`. `hamr dev`
    check still owed (not running); `./bin/stackrd` served `/` and a static
    asset with 200.
  - [x] step 0 / task 2: `AGENTS.md` examples call `*service.Orchestrator`;
    "Who validates what", store split by table, migrations editable until
    first install.
  - [x] step 0 / task 3: `AGENTS.md` "Layering rules": ten rules, service
    tree (paths under `internal/`, DECIDE 10), leaves, flows, leaf-or-flow
    test, auth in middleware.
  - [x] step 0 / task 4: `.claude/skills/handler-audit/` (`SKILL.md` +
    read-only `audit.sh`). Run on the scaffold: store 4 hits (api health
    handler holds `repo.Store`), auth 5 (login/register/logout drive the
    session manager), domain 2 (`strings.ToLower` email), templ 2 (`if
    form.GetError(...) != ""`, form plumbing, false positive). Scaffold
    handlers get rewritten in steps 4/6. `.gitignore` changed to track
    `.claude/skills/` (DECIDE 11).
  - [x] step 0 / task 5: `depguard` rules in `.golangci.yml`. Throwaway
    check: leaf → leaf, backup → deploy, promote → managed fail;
    promote → deploy, deploy → managed pass. `_test.go` files are exempt
    (`!$test`) so a package's black-box test may import itself. Throwaways
    deleted. `flow/jobs` edges not covered (DECIDE 12).
  - [x] step 0 / task 6: empty tree under `internal/` (`service/
    orchestrator.go`, `service/internal/{store,docker,proxy,git,s3,leaf,
    flow}`, `authz`, `ui/components`, `ui/pages/{org,stack,env,tile}`),
    one `doc.go` each. `frontend/` → `ui/` (static, css, npm, tailwind;
    tailwind also scans `internal/ui/`); paths fixed in `hamr.toml`,
    Makefile, `hamr.vendor.json`, `main.go`, Dockerfile, CI, `.gitignore`.
    `./bin/stackrd` served `/` and all three assets with 200; `hamr dev`
    check still owed.
  - [x] step 0 / task 7: `make build`, `make lint`, `make test` pass; skill
    runs. Still owed: `hamr dev` start check (builder may not start it).
    Known, not step 0: `make templint` fails on the scaffold's login and
    register forms (`no-native-form-actions`), and `ci.yml` calls `make
    migrate`, which is not a Makefile target, so PR CI goes red.
- [x] [F+O] step 1 groundwork
  - Order: secrets and errs landed before the store (it seals with both);
    the harness came with authz (the middleware test needs it), task 11
    added `servicetest.Store` and moved the other tests onto it.
  - Schema: surrogate ids on `org_members` and `release_tiles` (one CRUD
    shape); `commit` is `commit_sha` (reserved word). See `schema.md`.
  - `FileStorage` / `STORAGE_PATH` removed from `stackrd` (no v1 user).
    New env: `DATA_DIR`, `DATABASE_PATH`, `STACKR_MASTER_KEY`. `hamr dev`
    now needs `STACKR_MASTER_KEY` in `.env` (`openssl rand -hex 32`); the
    `hamr dev` start check is still owed to darhvader.
  - `flow/jobs` runs with no handlers and no param check wired: kinds and
    `enqueue` land with the first flow (step 3). Orchestrator exposes
    `GetJob`, `CancelJob` only.
  - `service/new_test.go` keeps its own DB: it is inside package
    `service`, the harness would be an import cycle.
  - `make templint` passes now; AGENTS.md: testify → stdlib, repo line,
    access middleware, env vars.
- [x] [F+O] step 2 docker wrapper
  - `ContainerSpec`: `Networks []NetAttach` (all joined at create, no
    default bridge; DECIDE 9 a), `Restart` is docker's string, plus
    `HostNetwork`, `CapAdd`. No 5s health-interval default (flow/deploy).
  - `Detail` gains `Running` and `Networks` (network → IP).
    `EnsureNetwork`/`CreateVolume` take labels; `ListNetworks`,
    `EnsureTool`, `ListImages`, `PruneImages` (keep list) added; `Build`
    returns the image id. Missing network/volume on remove = nil;
    missing container = typed `ErrNotFound`.
  - Tag → digest resolution lives only in `registry.Digest` (index
    digest, DECIDE 6 a); the Docker wrapper has no remote resolver.
  - `git`: token goes to git as `GIT_CONFIG_*` env, never in the URL;
    `ImageName` lives in `git` (the "tag exists → -jobID" rule is noted
    for `leaf/image`).
  - Webhook verify + decode sit in `githubapp` (from the webhook
    extract); `PullRequest` also decodes `head.sha`.
  - `s3`: `Put` takes an `io.ReadSeeker` (S3 needs the length; the
    archive is a scratch file). Single PUT, 5 GiB ceiling (ponytail).
  - `proxy`: pushes are serialized, not coalesced; coalescing needs the
    config builder, so it belongs to step 3. Every pushed config must
    carry the admin listener (Caddy drops to localhost otherwise).
  - `vip` integration test needs NET_ADMIN; passes under `unshare -rn`.
    `make test-integration` added (Docker, Docker Hub, MinIO, Caddy).
- [x] [F+O] step 3 services
  - session A (leaves 1–15) done: 2026-09-24
    - org: API key minting lives in leaf/user; invite TTL 7d; only the owner role is writable.
    - user: password minimum 8.
    - stack: new `slug` package; new `stacks.domains` column (reservations).
    - environment: PR envs stay off the ladder; network is `stackr-env-<id>`.
    - tile: volume attach/detach refusals moved to leaf/volume; ToggleCron and RunNow dropped; watcher-key wart fixed.
    - params: `ParamSet` for flow/jobs still unwired (DECIDE 17).
    - volume: max_size_mb recorded, not enforced.
    - domain: priority, rule, middlewares and custom certs gone; ACME account per email via Caddy issuers; force HTTPS is a 308; trusted-proxy parsing and the Cloudflare IP fetch left to session B; `golang.org/x/crypto` now a direct dependency.
    - connector: another org's connector is not found; no sole-connector fallback.
    - managed: env scope_id is the env id; on_remove is keep|drop.
    - release: `ReleaseTileStore.ImageIDs` added. job: `JobStore.ListTouching` added.
    - backup: `BackupRunStore.ListByPrefix` added; prefix is `stackr/<org>/<volume>/<schedule>` (was org/stack/tile/backup); `VolumeFor` not built (every schedule hangs off a volume row, leaf/volume names it); the dest-ref parser (`Resolve`) lives here; deleting a destination a schedule uses is refused.
  - session B (flows 16–25) done: 2026-09-24
    - stack file: `files:` mounts and `shared:` are refused (not built).
    - promote: no per-tile rollback helpers; a failed apply leaves done tiles on the new release.
    - backup: restore skips the old restart-cleanup steps.
    - proxy: `proxy_custom` is not fed to the builder yet; the proxy container needs `DNS_API_TOKEN` and its XDG dirs on a volume (step 5).
    - proxy: `trusted_proxies` takes IPs and CIDRs only; the `cloudflare` keyword (edge-range fetch) is not built.
    - upgrade: `Config.PanelSpec` is the installer's to supply (step 5); `stackrd upgrade-swap` runs the swap.
    - new `storetest` package, so in-package flow tests can seed a store without an import cycle.
    - promote exports `NormalizeRepo` and `Remove` for the service's webhook and delete jobs.
    - jobs get no `ParamSet` (DECIDE 17 b); DECIDE 35–39 added.
    - verb list and job lock sets: `docs/rewrite/verbs.md`.
- [x] [F+O] step 4 API + CLI
  - Routes: one table in `internal/api/routes.go` (op, verb or Self/Public); spec from `stackrd --dump-openapi` in `docs/openapi.json`, diffed by `make lint`.
  - Webhook is `POST /hooks/connectors/:connector`, not `/hooks/github/:org` (DECIDE 40).
  - CLI login: `/cli/authorize?port&state&name` (step 6 page) POSTs `/api/v1/orgs/:org/cli-codes` with session + `X-CSRF-Token`, redirects to `http://127.0.0.1:<port>/?code&state`; the CLI swaps the code at `POST /auth/exchange` (DECIDE 44).
  - Not mounted, step 6 owns it: `GET /settings/github/callback` -> `CompleteConnector`.
  - Harness: servicetest now stubs the VIP table by default and has `Healthy`, `Image`, `Connector`, `NewWith`.
  - Backup dest: an empty access key keeps the stored one (as the secret key did).
  - Config repo refuses another org's connector; domain reads blank the basic-auth password (empty + same user keeps it on update).
  - Modules: cobra, pflag, x/term moved from indirect to direct; none new.
- [x] [F+O] step 5 installer + self-upgrade — done: 2026-09-24
    - `stackr-install` (cmd/stackr-install) and `internal/installspec`: one spec renders the installer's `docker run` lines and stackrd's `Config.PanelSpec` (a test reads one back into the other).
    - answers saved to `<data>/install.json`; a re-run converges, other answers are refused; the master key lives in `<data>/keys/master.key`.
    - proxy is its own container (`stackrd proxy`, 80/443, admin on 127.0.0.1:2019, certs on volume `stackr-caddy`); the panel runs host-network, bound to the docker bridge gateway, no public port.
    - fixed from step 3: the upgrade helper's Cmd is `upgrade-swap` (the image entrypoint is stackrd), and a pull is skipped when the image is already local.
    - `stackr-install restore <archive>` puts a panel archive back (db, key, build); the upgrade job log prints this line.
    - `make release RELEASE=vX.Y.Z` plus `.github/workflows/release.yml` on a tag; image tags drop the `v`.
    - verified on the test VM: install with a Let's Encrypt cert, upgrade 0.0.1 → 0.0.2, restore back to 0.0.1, admin user kept each time.
    - waits on step 4: no API route or CLI verb starts an upgrade yet, so the VM run queued the job row by hand; the `stackr` host wrapper execs a CLI that is still the step 4 stub.
    - an upgrade swaps the panel only; the proxy stays on the image it was installed with (DECIDE 49).
    - the `cloudflare` keyword for `--proxy` is not built; the flag takes IPs and CIDRs.
- [x] [F+O] step 3b cron and function tiles — done: 2026-09-24 (branch `rewrite-step-3b`, stacked on step 5)
    - kinds `cron` and `function` on `tiles` with `schedule`, `trigger`, `paused`, `timeout_minutes`; B26 refusals verbatim from tilelifecycle.md; a run kind builds from `git_url` or runs `image:`, one of the two.
    - `leaf/run`: `runs` table (keep 50 per tile), log file `$DATA_DIR/runs/<tile>/<run>.log` capped to the last 1 MiB; runs left `running` at boot are failed.
    - `flow/run` (edge run -> deploy): row first, then the job; the container is built by `deploy.Spec`, waited on, removed; ok / exit code / timeout / stopped.
    - deploy of a run kind starts no container; a cron reloads the schedule table, an on_deploy function queues a run (after `Deploy` and after promote).
    - job kind `run`: lock set tile + `run:<id>`, no 30-minute cap (`jobs.Options.Uncapped`), the tile's timeout instead.
    - verbs `RunTile`, `PauseTile`, `Runs`, `Run`, `StopRun`, `RunLog`, `FollowRunLog`; `TileStatus` adds `last_run`, `next_run`, `paused`; Stop on a cron pauses, on a function refuses; Restart refuses both.
    - routes `POST tile/run`, `POST tile/pause`, `GET tile/runs`, `GET tile/runs/:run`, `DELETE tile/runs/:run`; `?run=` on logs and the log stream. CLI `stackr tile run|pause|resume|runs`, `logs --run`, `stop --run`, `--schedule/--trigger/--timeout`.
    - stack file: `kind: cron` + `schedule:`, `kind: function` + `trigger:`, `timeout_minutes:`; the plan prints old and new schedule, trigger and timeout.
    - no stacked PR opened (the builder was told not to push); DECIDE 52 to 63 added.
- [ ] [F+O] step 6 UI

## DECIDE:

Silent calls the planner made under rule 9 / "fix obvious gaps"; flip any
you disagree with:

1. Step 6 drops the terminal (xterm + websocket exec) and the YAML code
   editor from v1. v1 scope lists "container logs and restart" only.
   Options: (a) keep dropped, (b) add `<term-pane>` to the whitelist as a
   plan item for step 6.
2. Self-upgrade had no carry-over row. Added `service/admin.go` as extract
   `admin-upgrade.md` and a `flow/upgrade` task in step 3. Options: (a)
   keep, (b) redesign upgrade in a later round.
3. Scaffold options for `hamr new`: sqlite, sqlx, session auth, tailwind
   (as today; needs Node at build time like `tsc` does), no websockets (SSE
   chosen), migrate on startup, email mock (invites), no e2e, no alpine.
   Module path `github.com/FyrmForge/stackr` (as today).
4. Step 2 holds every infra wrapper (git, registry, vip, s3, githubapp,
   Caddy client), not only Docker, so step 3 is pure services.
10. The plan's `service/`, `authz/`, `ui/` Go packages live under
   `internal/` (the scaffold's convention; `internal/service` already
   exists). Static assets stay at `ui/static/`. Options: (a) keep, (b)
   root-level packages, delete the scaffold's `internal/` layout.
11. `.gitignore` ignored all of `.claude/`, so the handler-audit skill
   would never be committed. Step 0 changed it to `.claude/*` +
   `!.claude/skills/` (settings.local.json stays ignored). Options: (a)
   keep; (b) revert and `git add -f` skill files; (c) move the skill to
   `.agents/skills/`.

Raised by the extract agents, real decisions, not settled:

5. **Slug grammar.** REWRITE.md "Param store" says collection and param
   names are `[a-z0-9_]+`, "same validator as tile slugs". Old tile slugs
   have hyphens (`orders-db`) and a tile slug is a DNS alias, where `_` is
   not allowed. Options: (a) tile slugs `[a-z0-9-]+`, param names
   `[a-z0-9_]+`, two validators; (b) one grammar `[a-z0-9-]+` everywhere.
   Planner leans (a).
6. **Image watch digest.** Plan step 3 says "resolved to this box's arch";
   the old client deliberately compared the *index* (manifest list)
   digest. Options: (a) index digest, one call, arch-blind, as today; (b)
   arch-resolved, an extra call per image. Planner leans (a).
7. **`ManagedTile` shape.** Postgres engine returns argv for
   `leaf/tile.Exec`; the S3 engine provisions through the S3 SDK, its
   image may have no shell. One command-returning interface cannot cover
   both. Options in `extracts/managedtiles.md`: (a) engine methods take an
   `Exec` func and a `Client` and do the work themselves; (b) two
   interfaces. Planner leans (a).
8. **`leaf/tile.State()` and "deploy in flight".** A leaf may not read the
   jobs table. Settled by planner unless you object: `State()` returns
   container truth only; the orchestrator layers the last job on top.

9. **Shared-instance networks in the spec.** A stack- or org-scoped
   managed instance needs its consumer on a per-link network; the plan's
   `ContainerSpec` has one `NetworkName`. Options: (a) `Networks []` on
   the spec (planner leans (a), the pause/ingress networks need it too);
   (b) connect after start via a second wrapper call.
12. **`flow/jobs` edges vs depguard** (raised by step 0, blocks step 1
   task 10). REWRITE.md says every flow that starts work does it through
   `flow/jobs`, and step 1 says jobs run flows. Both are flow → flow edges
   the two listed edges (promote → deploy, deploy → managed) do not allow,
   and both directions at once would be an import cycle. Step 0 lint
   enforces only the two listed edges. Options: (a) the orchestrator
   enqueues, `flow/jobs` gets the flow funcs injected as handlers (no flow
   imports jobs, jobs imports no flow; lint unchanged); (b) add a
   `* → flow/jobs` exception and inject handlers into jobs; (c) move jobs
   out of `flow/` (e.g. `internal/service/internal/jobs`). Step 0 leans (a). Planner took (a) so step 1 could run; flip later if
   you want (b) or (c).

Settled silently, listed so you can flip them: label key is `stackr.tile`
(was `stackr.app`); restart policy stays the three old values, not a bool;
scheduler keeps `robfig/cron/v3`, orphan retention and image watch are
fixed entries (`@daily`, `@every Nm`) not schedule rows, backup schedules
keep their `timezone` column composed into `CRON_TZ=`; `release-tilediff` dual
field vocabulary collapses to one (config plans are gone); `ToggleCron`
pause intent has no home and cron tiles are Later anyway.

Raised by step 1 (builder took the lean; flip any):

13. **Unbound API keys.** `api_keys.org_id` is `ON DELETE CASCADE` (a key
   of a deleted org must not turn unbound), and an unbound key (no org)
   is admin-only: `authz.Can` refuses it for a non-admin. Options: (a)
   keep; (b) unbound keys act as the user across all their orgs.
14. **Master key source.** `stackrd` refuses to start without
   `STACKR_MASTER_KEY` and never generates one; the installer writes it.
   Options: (a) keep; (b) generate into `DATA_DIR` on first boot.
15. **Settings precedence.** For install-wide knobs: a `settings` row >
   the `Config` boot value (env) > catalogue default. So a UI change beats
   the env var. Options: (a) keep; (b) env pins the value, UI read-only.
16. **Supersede rule.** A newer job supersedes an older one only when
   same kind AND its lock set covers every tile of the older one; any
   other overlap queues behind. Options: (a) keep; (b) any overlap on the
   same kind supersedes.
17. **Parked jobs re-run blind.** With no param check wired, every
   `waiting` job is requeued each poll (3s) and the handler re-checks.
   Step 3 should pass `ParamSet` so they wake only when the param
   resolves. Options: (a) wire it in step 3; (b) keep the blind poll.
   Session B took (b): jobs get no `ParamSet`; a `ponytail:` in
   orchestrator.go marks it.
18. **Revoke breadth.** Losing standing in one org (removed, demoted)
   closes all of the user's sessions install-wide (sessions carry no org)
   and that org's API keys; losing stackr admin or being disabled closes
   every session and key. Options: (a) keep; (b) close sessions only on
   disable/admin loss.
19. **Schema calls.** Connector `config` encrypted (holds the App private
   key; not in the task's list); volumes carry their own
   `scope_kind`/`scope_id` instead of `env_id` or instance (a shared
   instance's volume follows its scope). Options: (a) keep; (b) revisit.

Raised by step 2 (builder took the lean; flip any):

20. **Replica spread.** A VIP spreads over its replicas with
   `-m statistic --mode random`, rule i of n taking 1/(n-i), so each
   replica gets 1/n per new connection. Options: (a) keep; (b)
   `--mode nth` round-robin (even counts, but per-rule counters reset on
   every rewrite).

Raised by step 3 session A (builder took the lean; flip any):

21. **Org roles.** `leaf/org` writes only `owner` (the plan's two roles:
   stackr admin, org owner); `authz` already ranks member/viewer. Options:
   (a) keep owner-only; (b) open member/viewer on members and invites now.
22. **Stack domain reservations.** The stack file's `domains:` (host,
   acme_email, include_env_on_default) had no home in the schema. Stored as
   a JSON column `stacks.domains`; server-wide host uniqueness is the
   flow's check against `domains`. Options: (a) keep; (b) its own table
   with a unique host index.
23. **B26 whitelist.** `leaf/tile.Carries`: service = build keys + run
   keys; image = image, update_policy auto, tag_policy + run keys; managed
   = image override, env, limits, shm_size_mb, published_ports only (the
   engine owns command, port, volumes; one replica). git_url stays
   GitHub-only (the old message). Options: (a) keep; (b) let managed rows
   carry more run keys.
24. **Where proxy pushes coalesce.** `leaf/domain.Syncer`: one run at a
   time, callers during a run collapse into exactly one follow-up, the run
   ignores the request context (2 min cap), every caller gets its run's
   error. It takes `Build` (the flow: rows + facts → `domain.Build`) and
   `Push` (`proxy.Client.Push`) as funcs, since the config needs other
   tables. Options: (a) keep in leaf/domain; (b) move to a `flow/proxy`.
25. **`proxy.methods` extra.** Caddy blocks no verb, so WebDAV/CalDAV
   already pass. Built as an allowlist (a `method` matcher: other verbs
   miss the route). Options: (a) keep allowlist; (b) drop the extra.
26. **Restoring an orphan's archive.** Runs outlive their volume
   (volume_id SET NULL), but `leaf/backup.Restorable(run, sourceVolume)`
   needs the run to belong to the source volume, so an orphan's last
   archive (prefix `stackr/_orphan/<volume>`, no org in it) cannot be
   restored from the panel. Options: (a) keep, admin restores by hand;
   (b) record the org on the run so its owner can restore it into
   another volume.

Raised by step 3 session B (builder took the lean; flip any):

27. **`files:` mounts.** Materializing repo files needs the tile's clone at
   the pinned commit, which only the build path has. flow/deploy refuses a
   tile with `files:` ("not supported yet"). Options: (a) keep refused in
   v1 (not in the v1 scope list); (b) the build job copies the files into
   `<data>/files/<tile>/<commit>` and deploy binds them read-only. Lean (a).
28. **How stackrd reaches an s3 instance.** The s3 engine speaks the S3
   API from stackrd at the instance's endpoint, else `http://<slug>:9000`,
   so stackrd must be routable to it. Options: (a) keep, stackrd joins the
   env/shared networks it manages; (b) exec an `mc` sidecar on the
   instance's network. Lean (a).
29. **What may land in a `from: promote` env.** One rule serves promote
   and rollback (B2): the release's number must not be above the one the
   env below runs, and the env below must run something. No history
   lookup, so any older release may come back. Options: (a) keep;
   (b) only releases the env below has actually run. Lean (a).
30. **`shared:` in the stack file.** Stack- and org-scoped managed
   instances from the file need scope changes on promote and ownership
   across envs. `Parse` refuses a non-empty `shared:`. Options: (a) keep
   refused in v1, share from the panel/CLI; (b) build it. Lean (a).
31. **Where stack-file keys land.** Params land in the promoted env's
   scope (secrets are declared only, a warning while unset); stack
   `defaults:` and `domains:` reservations apply only when promoting into
   the bottom rung, so a rollback higher up never rewrites them. Slices
   sit on the consumer (`slices: [db]` or `{from, name, on_remove,
   public}`). Options: (a) keep; (b) params at stack scope. Lean (a).
32. **Image-watch tag policy grammar.** `tag_policy` is `[semver]
   <constraint>`: `^1.2`, `~1.2`, a prefix `1` / `1.2`, or `*`; bare
   `semver` means `^` the ref's own tag, so a watch never jumps a major
   unasked. Pre-releases never match. A release from the watch is `Auto`
   only when every tile it swaps is `update_policy: auto`; a mixed env
   waits for the button. Options: (a) keep; (b) a regex policy too.
   Lean (a).
33. **Panel swap runs in a helper.** A process cannot gate its successor
   after stopping its own container, so `Upgrade` pulls, archives, then
   runs a one-shot `stackr-upgrader` container from the new image
   (`stackrd upgrade-swap`, spec as JSON in `STACKR_SWAP_SPEC`). It stops
   the old panel (kept), runs the new one as `stackr-<version>`, gates it
   like a tile, then removes the old or puts it back. The panel is found
   by the `stackr.role=panel` label, not its name. `upgrade_archive` is
   recorded at launch: the old panel is gone by the time the swap ends,
   and the archive restores either way. Options: (a) keep; (b) the new
   panel records the outcome on boot. Lean (a).
34. **Non-cron drivers on the cron.** Orphan retention is an `@daily`
   entry and image watch an `@every 1m` entry through the same registry;
   the watch flow's `Due` holds the real interval (`image_check_interval`,
   0 = off), so a setting change needs no reload. Backup time zones are
   composed from the column as `CRON_TZ=`. Options: (a) keep; (b) a second
   entry kind. Lean (a).
35. **PR env lifecycle.** A pull_request opens `pr-<n>` cloned from the
   lowest ladder env built from the PR's base branch, then runs a push
   for the head commit; closing removes its tiles and the env. Only
   stacks whose tiles or config repo use that repo get one.
   `pr_envs.enabled`/`against` in the stack file are not read yet.
   Options: (a) keep; (b) honour `pr_envs` in step 3. Lean (a), and
   honour it with the stack file work in step 4.
36. **Param and settings changes redeploy right away.** `SetParams`,
   `DeleteParam`, `SetStackSettings`, `SetEnvSettings` and a redeploy
   edit through `UpdateTile` queue a deploy for every running tile in
   scope. `SetSettingDefaults` redeploys every org. Options: (a) keep;
   (b) mark tiles stale and let the user deploy. Lean (a) (B34).
37. **Panel archives.** Panel self-backups go to the local dest only,
   and the newest 14 are kept. The install id comes from
   `STACKR_INSTALL_ID` (default `default`). Options: (a) keep; (b) a
   setting for the dest and count. Lean (a).
38. **Stack delete refuses while envs exist.** Options: (a) keep;
   (b) cascade-delete the envs as jobs. Lean (a).
39. **Promote has no request-time dry run.** `Promote` and `Rollback`
   queue the job at once; the job fails with the blocker text.
   `PlanPromote` is the pre-check a handler or UI calls first. Options:
   (a) keep; (b) plan inside `Promote` and refuse before queueing.
   Lean (a).
40. **Webhook path.** `POST /hooks/connectors/:connector`, not
   `/hooks/github/:org`: the App manifest registers one URL per connector
   and `Webhook` takes a connector id. Options: (a) keep; (b) per-org path
   that looks the connector up. Lean (a).
41. **API accepts the session cookie.** A browser session works on
   `/api/v1` with `X-CSRF-Token`; bearer keys skip CSRF. Options: (a) keep
   (step 6 pages call the API); (b) keys only. Lean (a).
42. **Streams.** SSE for job events, log follow and one-shot exec; streams
   detach from the 30s request timeout and skip gzip. Logs and exec take
   `?container=` (the CLI picks the first replica). Options: (a) keep;
   (b) a replica flag in the CLI. Lean (a).
43. **Slice visibility checked late.** `AttachSlice` checks the instance
   is visible to the consumer only inside the job, so a bad id is a failed
   job, not a 4xx. (Fixed in step 4 instead: `SetConfigRepo` refuses
   another org's connector as 404; domain reads blank the basic-auth
   password and an update with the same user and an empty password keeps
   the stored one.) Options: (a) keep; (b) check at request time. Lean (a).
44. **CLI login codes in memory.** One-time codes live in the process
   (2 min TTL); a restart drops pending logins. Options: (a) keep;
   (b) a table. Lean (a).
45. **CLI shape.** Tables are tabwriter (TSV when piped), not lipgloss;
   `link` takes flags, no picker; nouns are `params`, `managed`, `key`;
   the old aliases and the forward/storage/image/proxy nouns are gone;
   `tile set` prints the redeploy job's log command instead of following
   it. Options: (a) keep; (b) restore any of them. Lean (a).
46. **Child-id org check.** `:release`, `:domain`, etc. are checked
   against the org only, not the stack or env in the path; an unknown
   param fails closed (500). Options: (a) keep, the verb rejects a
   mismatch (a release of another stack is a promote blocker); (b) check
   the full path. Lean (a).

Raised by step 5 (builder took the lean; flip any):

47. (step 5) **Recovery passphrase is the master key.** Panel archives are
   age-encrypted with `Config.Passphrase`, which defaults to the master
   key, so the installer prints the key once as the recovery passphrase.
   Options: (a) keep, one secret to keep off the box; (b) a separate
   passphrase asked at install.
48. (step 5) **Panel on host networking.** The panel needs iptables in the
   host netns (VIPs) and the proxy reaches it as `stackr:8080` through
   `host-gateway`. Host networking means stackrd cannot resolve container
   names, which breaks DECIDE 28 (a) (`http://<slug>:9000`) and anything
   else that dials a tile by name. Options: (a) keep, dial by container IP
   from inspect; (b) panel on a bridge with `--network`s joined per env.
49. (step 5) **Upgrade leaves the proxy alone.** Only the panel is swapped;
   `stackr-proxy` stays on its install image, so Caddy changes in a
   release need a re-install. Options: (a) keep; (b) the helper also
   recreates the proxy after the panel gate passes.
50. (step 5) **Panel bind and trust.** The panel listens on the docker
   bridge gateway (not public) and trusts X-Forwarded-For from the RFC1918
   ranges, since the proxy's source address is a bridge address.
   Options: (a) keep; (b) pin trust to the proxy container's address.
51. (step 5) **install.json is the one source of install answers.** The
   installer, stackrd's upgrade spec and restore all read it; changing an
   answer means editing it (or a clean reinstall). Options: (a) keep;
   (b) a `stackr-install --reconfigure` that rewrites it and recreates
   both containers.

Raised by step 3b (builder took the lean; flip any):

52. (step 3b) **A cron or function may run an image.** The spec says deploy
   builds from git; the tile also takes `image:` instead (one of the two,
   like service vs image tiles), pinned by digest on deploy. Options:
   (a) keep; (b) git only. Lean (a).
53. (step 3b) **`timeout_minutes` is a fourth column.** The spec lists three
   columns but a run's timeout has to live somewhere; 0 is stored as 30, no
   upper bound (tilelifecycle.md). Options: (a) keep; (b) a setting instead
   of a column. Lean (a).
54. (step 3b) **`runs` has `job_id`, `reason`, `created_at` too.** Needed for
   StopRun (the job to cancel), the overlap and timeout wording, and order.
   Options: (a) keep; (b) trim to the spec list. Lean (a).
55. (step 3b) **A run holds its tile's lock.** The lock set is the tile plus
   `run:<id>`, so a long run makes a deploy or promote of that tile wait
   for it (never the other way round mid-run). Options: (a) keep; (b) lock
   only `run:<tile>` so a deploy may swap the image under a running run.
   Lean (a).
56. (step 3b) **Overlap is refused at queue time.** The spec says "superseded
   by the lock set"; lock-set superseding would cancel the older queued
   job instead. Built as: a second run while one is queued or running is
   written `cancelled` with "previous run still going" and gets no job.
   Options: (a) keep; (b) queue it behind the first. Lean (a).
57. (step 3b) **Stop on a cron answers an empty job.** `StopTile` pauses a
   cron with no job, so `POST tile/stop` answers 202 with a zero job and
   the CLI prints it. Options: (a) keep; (b) the handler answers the tile
   (200) for a cron. Lean (a) as built; (b) is a few lines if wanted.
58. (step 3b) **Start on a cron or function is not guarded.** It queues a
   start job that finds no replicas. Options: (a) Start on a cron resumes
   it, on a function refuses (a new refusal string, none in the
   extract); (b) keep. Lean (b) as built, until the wording is chosen.
59. (step 3b) **`kind:` and `type:` are one stack-file key.** Either spelling
   works; both given and different is a plan blocker. Options: (a) keep;
   (b) `kind:` only. Lean (a).
60. (step 3b) **Each run re-prepares like a deploy.** `deploy.Spec` runs the
   managed-slice reconcile and the image pull check before every run, so
   slices and credentials are current. Options: (a) keep; (b) reuse what
   the deploy prepared. Lean (a).
61. (step 3b) **A source edit on a cron waits for the next deploy.** The
   Redeploy effect only redeploys a tile with replicas; a run kind has
   none, so it keeps the pinned image until Deploy or a promote (as a
   stopped service does, B34). Options: (a) keep; (b) redeploy run kinds
   on a source edit. Lean (a).
62. (step 3b) **`trigger` on a non-function uses the extract's wording**
   "run_on_deploy applies to function tiles only" (verbatim rule), though
   the key is now `trigger`. Options: (a) keep; (b) "trigger applies to
   function tiles only". Lean (a): the task asks for the extract's strings
   verbatim.
63. (step 3b) **Runs cut short by a restart are failed, not retried.** A run
   left `running` at boot is closed failed "stackrd restarted while this
   run was going"; its job is not re-run. Options: (a) keep; (b) re-queue
   it. Lean (a).

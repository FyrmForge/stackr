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
    - verbs `RunTile`, `PauseTile`, `Runs`, `Run`, `StopRun`, `RunLog`, `FollowRunLog`; `TileStatus` adds `last_run`, `next_run`, `paused`; Stop on a cron pauses, on a function refuses; Restart and Start refuse both.
    - routes `POST tile/run`, `POST tile/pause`, `GET tile/runs`, `GET tile/runs/:run`, `DELETE tile/runs/:run`; `?run=` on logs and the log stream. CLI `stackr tile run|pause|resume|runs`, `logs --run`, `stop --run`, `--schedule/--trigger/--timeout`.
    - stack file: `kind: cron` + `schedule:`, `kind: function` + `trigger:`, `timeout_minutes:`; the plan prints old and new schedule, trigger and timeout.
    - no stacked PR opened (the builder was told not to push); DECIDE 52 to 62 added.
- [x] [F+O] step 3c tile-to-tile traffic — done: 2026-09-24 (branch `rewrite-step-3c`, stacked on step 3b)
    - `leaf/traffic`: memory only; `Sample(ipTile, conntrack, now)`, `Snapshot()`, `Seq()`, `Slices(pairs, bind)`; kept `flowFields` and the rate math; a tuple's first sight counts 0.
    - ends: tile ids; `proxy` = the proxy container's IPs plus every stackr network's gateway; `internet` = a tile-side connection's unknown far end; outside <-> proxy and host-only lines drop; zero lanes drop.
    - `flow/traffic`: `Tick` (read the table first, then tile leaf `Addresses`, domain leaf `ProxyAddrs`, environment leaf `Gateways`), `Edges(env)` (slice rename through the consumer's provision row, then lanes touching the env); `Check` logs one boot warning (no table / no `bytes=`), never fails boot.
    - scheduler entry `traffic`, `@every 5s`, runs inline (a read, not a job). Docker wrapper gains `Gateways(labels)`. `Config.Conntrack` (default `/proc/net/nf_conntrack`); servicetest points it at a missing file so the tick never touches the fake.
    - verb `Traffic(env) []Edge{from,to,bps}` + `TrafficSeq`; routes `GET env/traffic` (`env.traffic`), `GET env/events` (`env.events`, SSE, one `traffic` event per sample, first one at connect); `stream.PollAs`; CLI `stackr env traffic`.
    - installer writes `1` to `/proc/sys/net/netfilter/nf_conntrack_acct` and `/etc/sysctl.d/99-conntrack-acct.conf`; a refusal warns.
    - still owed: task 4's "install on the VM shows rates between two tiles" (not run); no stacked PR opened (the builder was told not to push); DECIDE 63 to 70 added.
- [ ] [F+O] step 6 UI (sessions B–D follow `docs/rewrite/ui-plan.md`:
  four canvases, drawers and dialogs, seven elements; decisions settled
  2026-09-24; session A DECIDE items are 71–75)
  - session A (tasks 1–4) done:
    - Components live in `internal/ui/components` (DECIDE 10), not `ui/components`; depguard `ui-sees-view-structs` keeps them off service/middleware.
    - templint has no allowlist; `internal/ui/components/elements_test.go` enforces the four tags (one define each, no import/fetch/XMLHttpRequest/shadow DOM/innerHTML, <300 lines).
    - TS sources in `ui/ts/`, compiled JS committed in `ui/static/js/elements/` (CI checks it is fresh); `make build` runs tsc, `make lint` runs `tsc --noEmit`; hamr watch rule `ts`.
    - Vendored htmx 2.0.4 + htmx-ext-sse 2.2.4 in `ui/static/js/vendor/`; idiomorph dropped.
    - No inline script: htmx config in `<meta name="htmx-config">`, CSRF token in `hx-headers` on `<body>`; `CSRFField` removed.
    - Placeholder page on `/:org`, `/:org/:stack`, `/:org/:stack/:env` (`org.read`) and `/:org/:stack/:env/:tile` (`tile.read`) in `internal/web/handler/scope`.
    - Gallery at `GET /dev/components` (DevMode only), `internal/web/handler/devgallery`.
  - API (package `internal/web/render`):
    - `render.Page(c echo.Context, status int, title string, body templ.Component) error`: full page when no `HX-Request` or on history restore, else the fragment; sets `Vary: HX-Request`.
    - `render.Shell(c echo.Context, title string) components.Shell`: CSRF, flash, user, nav, crumbs from the scope.
  - API (package `internal/ui/components`):
    - Shell: `Layout(s Shell, body templ.Component)`, `Fragment(s Shell, body templ.Component)` (title + OOB `#shell-header` + OOB `#flash` + body for `#main`), `NavLink(l Link)`, `ThemeToggle()`, `ErrorPage(code int, message string)`; `Shell{Title, CSRF, User string; Crumbs, Nav []Link; Flash Flash}`, `Link{Label, Href string; Active bool}`, `Flash{Message, Kind string}`.
    - Form: `Form(id, action string)` (children; hx-post, swaps `#id` outerHTML), `FormError(msg string)`, `Field(f FieldView)`, `Submit(label string)`, `FieldError(field, err string)`, `FieldErrorOOB(field, err string)`, `GetError(errors map[string]string, field string) string`, `OOBValidator(c echo.Context, field, errMsg string) error`; `FieldView{Name, Label, Type, Value, Placeholder, Help, Error, ValidateURL string; Required, Disabled bool; Why string}`.
    - Table: `Table(t TableView)`, `Text(s string)`; `TableView{Headers []string; Rows [][]templ.Component; Empty EmptyView}`.
    - `EmptyState(e EmptyView)`; `EmptyView{Title, Body string; Action Link}`.
    - `Pagination(p PageNav)`; `PageNav{Label, Prev, Next string}` ("" = no link).
    - Badges: `TileBadge(word string)`, `JobBadge(state string)`, `EnvBadge(name, color string)`; `EnvColors []string`.
    - Cards: `TileCard(t TileCardView)`, `VolumeCard(v VolumeCardView)`; `TileCardView{Name, Href, Kind, State, Source string; Volumes []VolumeCardView}`, `VolumeCardView{Name, Href, Size string; Orphaned bool}`.
    - `Confirm(v ConfirmView)` (panics if Word set and Kept empty); `ConfirmView{Button, Title, Warning string; Kept []string; Word, Action, Target string}`; posts `Action` on the element's `confirmed` event; `Target` "" = swap none.
    - Logs: `LogPane(v LogPaneView)`, `LogLine(l LogLineView)`; `LogPaneView{StreamURL, Level, Search string}`, `LogLineView{Time, Level, Text string}`; SSE events `line` (one rendered LogLine) and `end`.
    - Jobs: `JobStatus(v JobStatusView)`, `JobStatusBody(v JobStatusView)`; `JobStatusView{Kind, State, Error, Href, StreamURL string; Live bool}`; SSE events `update` (a rendered JobStatusBody) and `end`.
    - `Plan(p PlanView)`; `PlanView{Title string; Changes []ChangeView; Blockers, Warnings []string; CanDeploy bool}`, `ChangeView{Kind, Tile, Field, Old, New, Note string}`.
    - `ParamEditor(v ParamEditorView)`; `ParamEditorView{Action, DeleteAction string; Params []ParamRowView; Secrets []SecretRowView; ReadOnly bool; Why string}`, `ParamRowView{Collection, Name, Value, DecidedBy string; Overrides bool; Warn string}`, `SecretRowView{Collection, Name string; Set bool; DecidedBy string; Overrides bool; Warn string}`; posts `param.<c>.<n>`, `secret.<c>.<n>` (empty = keep), `new_collection/new_name/new_kind/new_value`; delete posts `collection,name`.
    - `SettingsForm(v SettingsFormView)`; `SettingsFormView{ID, Action, Scope string; Rows []SettingRowView; ReadOnly bool; Why string}`, `SettingRowView{Key, Desc, Type, Value, Effective, DecidedBy, Error string}`; posts one field per key, "" = inherit.
  - Elements: `<log-pane level search>`, `<confirm-dialog word>` (fires bubbling `confirmed`), `<flash-toast kind>` (also shows htmx request errors), `<theme-toggle>` (localStorage `theme`).
  - [x] session B task 5 done (branch `rewrite-step-6b`): `<graph-canvas>`, `<graph-node>`, `<side-drawer>` in `ui/ts/`; `elements_test.go` holds the seven tags, a line budget each (canvas 300, rest 150) and the contract names each source must carry. Additions to the contract are in step-6.md "Added by task 5": canvas `divider`, `[data-look]`/`[data-zoom]` chrome, `#graph-arrow` marker, a `node_id` hidden input, `hx-disinherit="*"` on the node (else the card's drawer GET inherits `hx-swap="none"`). `components.SideDrawer()` sits in `Layout` after `#main`. Gallery: fake canvas at `/dev/components`, `POST /dev/components/positions` (204), `GET /dev/components/drawer`. Checked in a browser: drag, wall, marquee, multi-drag, drawer tabs/Escape/backdrop/URL, wheel zoom, pan, fit, hover, fan-out, arrows. Not checked: pinch, touch slop, snap, nudge, straight, localStorage, `focus`, sub-tile click. DECIDE 76–83.
  - [x] session B tasks 6–8 done (branch `rewrite-step-6b`): `graph` service (`Canvas`, positions, annotations; env node ids are row ids as C's cards expect); `internal/ui/graph` + one `internal/web/handler/canvas` for the four levels (helper routes under `<page>/-/`, one SSE producer `Poll` the env stream folds in); drawers `internal/ui/drawer/{org,stack,env,connector,vars}` at each card's own path in `components.Drawer`, create org/stack/env + install connector in `internal/ui/dialog`, delete confirms. Owed items done: a fresh `?drawer=&tab=` load renders the drawer open with its tab (same render as the route, per-tab verbs), pan/zoom kept per path in sessionStorage (DECIDE 76 took (b)). C's shared files (`stream.go` HTML events, `render.Event`, `webtest`) applied byte-identical from 3a43130. Routes in step-6.md "Canvas pages" and "Drawers and dialogs". DECIDE 84–99. Not checked in a browser.
    - Owed to task 7/8 (fresh `?drawer=&tab=` renders closed): done in B via `render.PageWith`; env drawers joined at the merge.
  - [x] session C task 9 done (branch `rewrite-step-6c`): cards in `internal/ui/graph/cards` (`Card`, `Subs`, `Footer`, `Lanes`; own views `CardView`, `FooterView`, `SubView`, `Lane`), every kind in the gallery's "Env canvas cards"; lanes are one `<svg data-edges data-lanes id="graph-lanes">` swapped by the `traffic` event of `GET /:org/:stack/:env/events` (`internal/web/handler/env`); SSE bodies are rendered templ (`stream.HTML`, `render.Event`). Contract in step-6.md "Added by task 9". DECIDE 100–101.
    - Merge need for B (`childList`, DECIDE 100): B's observer already had it; checked at the merge.
  - [x] session C task 10 done: drawers `internal/ui/drawer/{tile,instance,slice,volume,proxy}`, `internal/ui/dialog` (create tile, restart/stop/delete/rollback confirms), handlers `internal/web/handler/{tile,env}` (each `Mount`s its routes); every answer is one `#drawer-view`. New read verbs `TileImage`, `TileVolumes`, `Volume`, `InstanceSlices`, `Provision`, `Routes` (verbs.md). Shared test site `internal/web/webtest`. Drawer URLs in step-6.md "Added by task 10". DECIDE 102–114.
  - [x] sessions B and C merged 2026-09-24 (branch `rewrite-step-6c`, C rebased on `rewrite-step-6b`):
    - Env drawers, dialogs, job and log streams moved under `/:org/:stack/:env/-/` (`-/tiles/:tile`, `-/instances/:tile`, `-/slices/:id`, `-/volumes/:id`, `-/proxy`, `-/new-tile`, `-/jobs/:job/events`); `handler/env` lost its `/events`.
    - One env stream: `canvas` `/-/events` folds `Poll` (graph, footers) with a `traffic` producer (lanes at connect and on each `TrafficSeq` change); lanes also drawn with the page.
    - Env nodes wear C's cards (`cards.Card` + `cards.Subs`, live `cards.Footer`); the card is a flex column and fills what its sub-tiles leave (no inline height). A hosting instance sub-tile carries its tile slug; replica and instance sub-tiles open their drawer.
    - `drawer()` in `internal/web/handler/canvas/drawer.go` opens tile, instance, slice, volume, proxy and vars drawers on a fresh `?drawer=&tab=` load (sub-tile ids too).
    - Tile drawer uses `components.Drawer` and `PostButton`; one tab strip.
    - `+ tile` on the env canvas.
    - Browser-checked on the built binary: four canvases render; drag persists after reload (positions 204); tile card opens its drawer with 8 tabs; fresh `?drawer=&tab=` opens it; Escape closes and cleans the URL; a swapped-in lanes svg gets real paths. Fixed there: logs tab without a container no longer opens a failing stream; `main.js` afterSwap tolerates SSE swaps. DECIDE 115–119.

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
58. (step 3b) **`kind:` and `type:` are one stack-file key.** Either spelling
   works; both given and different is a plan blocker. Options: (a) keep;
   (b) `kind:` only. Lean (a).
59. (step 3b) **Each run re-prepares like a deploy.** `deploy.Spec` runs the
   managed-slice reconcile and the image pull check before every run, so
   slices and credentials are current. Options: (a) keep; (b) reuse what
   the deploy prepared. Lean (a).
60. (step 3b) **A source edit on a cron waits for the next deploy.** The
   Redeploy effect only redeploys a tile with replicas; a run kind has
   none, so it keeps the pinned image until Deploy or a promote (as a
   stopped service does, B34). Options: (a) keep; (b) redeploy run kinds
   on a source edit. Lean (a).
61. (step 3b) **`trigger` on a non-function uses the extract's wording**
   "run_on_deploy applies to function tiles only" (verbatim rule), though
   the key is now `trigger`. Options: (a) keep; (b) "trigger applies to
   function tiles only". Lean (a): the task asks for the extract's strings
   verbatim.
62. (step 3b) **Runs cut short by a restart are failed, not retried.** A run
   left `running` at boot is closed failed "stackrd restarted while this
   run was going"; its job is not re-run. Options: (a) keep; (b) re-queue
   it. Lean (a).

Raised by step 3c (builder took the lean; flip any):

63. (step 3c) **The proxy end is the proxy container too.** The spec says
   Caddy is host-network, but `stackr-proxy` is a bridge container joined
   to every ingress network, so proxy -> tile comes from its container IP.
   Both its IPs and every stackr network gateway map to `proxy`.
   Options: (a) keep both; (b) gateways only (ingress would show as
   internet). Lean (a).
64. (step 3c) **Host traffic counts as proxy.** The host-network panel
   (health gates, managed admin) reaches tiles from the network gateway,
   so it lands on the proxy lane too. Options: (a) keep; (b) a third
   pseudo id `host` for gateways. Lean (a).
65. (step 3c) **A slice end is the consumer's provision row id.** A shared
   slice (`Share` copies the row) shows once per consumer, not as one
   card. The rename runs for the env's own consumers only: on the
   instance's env canvas, a consumer from another env shows as
   `<tile> -> instance` (a card that env does not draw; step 6 drops it).
   Options: (a) keep; (b) key shared rows by the original slice.
   Lean (a).
66. (step 3c) **Zero lanes are dropped.** An open idle connection draws no
   lane. Options: (a) keep; (b) keep 0 lanes so the graph can show idle
   links. Lean (a).
67. (step 3c) **No boot warning on an empty table.** "No line carries
   `bytes=`" only warns when the file has lines. Options: (a) keep;
   (b) warn on empty too. Lean (a).
68. (step 3c) **IPs come from the container list, not inspect.** The list
   already carries every network IP; no per-container inspect each tick
   (`Detail.Networks` is only used for the proxy). Options: (a) keep;
   (b) inspect each. Lean (a).
69. (step 3c) **Events stream URL and verb.** `GET .../envs/:env/events`,
   authz verb `tile.read`, one event `traffic` (the env's full lane list,
   not a ping). Step 6 adds its card events on the same stream. Options:
   (a) keep; (b) step 6 picks another URL. Lean (a).
70. (step 3c) **Installer writes /proc/sys directly.** No `sysctl` binary
   call; a refusal (no conntrack module, read-only /proc) prints a warning
   and the install goes on. Options: (a) keep; (b) fail the install.
   Lean (a).

Raised by step 6 session A (builder took the lean; flip any):

71. **(step 6) main.js stays.** `ui/static/js/main.js` (hamr's
   revalidate-while-typing listener, scroll to the first field error,
   console logging) is the one script besides the four elements. Options:
   (a) keep; (b) fold it into an element. Lean (a).
72. **(step 6) Env colour is a palette name.** `EnvBadge` takes one of
   `components.EnvColors`; anything else renders neutral, hex is not
   supported. Options: (a) keep, the env form offers a select; (b) accept
   hex with an inline style. Lean (a).
73. **(step 6) Dark by default.** The server renders `<html class="dark">`;
   `<theme-toggle>` applies a stored "light" when its module runs, so a
   light user sees dark for a moment on a full load (no inline script
   under the CSP). Options: (a) keep; (b) a theme cookie read by
   `render.Shell`. Lean (a).
74. **(step 6) Log filters are not in the URL.** `<log-pane>` starts from
   the `level`/`search` the handler put in the view; changing them does
   not rewrite the URL. Options: (a) keep; (b) the element pushes them
   with `history.replaceState`. Lean (a).
75. **(step 6) Flash dismissal.** Success/info/warning toasts go after 5 s;
   errors (including htmx request failures, shown as "Request failed:
   <status>") stay until clicked. Options: (a) keep; (b) all stay. Lean (a).

Raised by step 6 session B task 5 (builder took the lean; flip any):

76. **(step 6) A `graph` SSE swap resets pan and zoom.** Replacing the whole
   canvas on add/remove re-runs fit. Options: (a) keep; (b) the element
   keeps its viewport in sessionStorage keyed by path. Lean (b), in task 7.
77. **(step 6) Line budgets are full.** Canvas 298/300, node 149/150, and
   only because the canvas class has no blank lines and some one-line
   statements. Options: (a) accept the dense style, no growth; (b) raise
   the canvas to 350 and the node to 180. Lean (b).
78. **(step 6) Multi-drag clamps each card at the wall on its own.** A
   system card stops at the divider while the rest keep moving, so the
   group's spacing changes. Options: (a) keep; (b) clamp the shared
   delta. Lean (a).
79. **(step 6) No-overlap nudge and hover focus are always on.** v0 had
   both as view toggles. Options: (a) keep; (b) add them to `data-look`.
   Lean (a).
80. **(step 6) Marquee replaces the selection.** Shift+drag again does not
   add. Options: (a) keep; (b) add to it. Lean (a).
81. **(step 6) Close during an in-flight open reopens the drawer** when the
   response lands. Options: (a) keep; (b) the element ignores a swap after
   a close until the next card click. Lean (a).
82. **(step 6) Drawer focus.** Opening focuses Close; no focus trap and no
   focus return, though the aside says `aria-modal="true"`. Options:
   (a) keep; (b) trap and return focus (lines in `side-drawer`). Lean (b)
   in session D's a11y pass.
83. **(step 6) Every page loads all seven element scripts.** Modules, cached,
   a few KB. Options: (a) keep; (b) canvas pages only. Lean (a).

Raised by step 6 session B tasks 6–8 (builder took the lean; flip any):

84. **(step 6) Graph verbs are web-only.** `Canvas`, positions and notes
   have no `/api/v1` routes. Options: (a) keep; (b) add them for the CLI.
   Lean (a).
85. **(step 6) One canvas handler, not `handler/<level>`.** The four levels
   and their drawers share `internal/web/handler/canvas`; the level comes
   from the resolved scope. Options: (a) keep; (b) split per level as
   ui-plan §6 says. Lean (a).
86. **(step 6) Two route spellings.** B's helper routes and drawers sit
   under `<page>/-/` (`-` is never a slug); C's env routes are
   `/:org/:stack/:env/drawer/...` and `/events`, which shadow a tile named
   `drawer` or `events`. Options: (a) keep both; (b) move C's under `/-/`
   at the merge. Lean (b).
   **Resolved at the merge: (b), all under `/-/`.**
87. **(step 6) The env canvas streams from C's `/events`.** On B alone
   that URL is a 404 until the merge folds `canvas.Poll` in. Options:
   (a) keep; (b) B serves `/-/events` at env too. Lean (a).
   **Resolved at the merge: (b), one `/-/events` with traffic folded in; C's `/events` removed.**
88. **(step 6) Params editor shows this level's rows only.** No verb
   returns the resolved chain, so "decided by" is the level itself and
   "overrides" is never set. The vars card and every params tab are the
   same editor. Options: (a) keep; (b) add a resolve verb. Lean (b),
   later.
89. **(step 6) Member emails come from the user list.** `Members` returns
   user ids only; the tab reads `Users()` and maps. Options: (a) keep;
   (b) a verb that joins members to users. Lean (b).
90. **(step 6) Invite and key tokens show in the drawer's note.** An
   invite answers "Invite link: /invite/<id>" (session D's page); a
   minted key shows once in the note. Options: (a) keep; (b) a copy box.
   Lean (a).
91. **(step 6) Placeholders where no verb exists.** Connector repos, env
   logs (per tile instead), the PR badge and stack defaults. Options:
   (a) keep; (b) add verbs. Lean (a) for v1.
92. **(step 6) Releases tab lists only.** Diff, promote plan and rollback
   are env actions (C's rollback confirm). Options: (a) keep; (b) a
   per-row "promote to" on the stack drawer. Lean (a).
93. **(step 6) Remove member has no confirm.** One button. Options:
   (a) keep; (b) a confirm. Lean (b), small.
94. **(step 6) "+ org" is admin-only.** It mirrors the API's `org.create`;
   an owner sees no button. Options: (a) keep; (b) any user may create.
   Lean (a).
95. **(step 6) Connector install posts a plain form to GitHub**
   (`templint:ignore no-native-form-actions`, as the old panel did). The
   callback page is session D's. Options: (a) keep; (b) none. Lean (a).
96. **(step 6) Anonymous on a deep page gets 401, home redirects.** `/`
   sends a visitor to `/login`; `/:org...` answers 401/404 as the access
   test holds (task 7 had broken it; fixed). Options: (a) keep;
   (b) every page redirects to login. Lean (b) in session D.
97. **(step 6) Notes land at 0,0 and redraw for the typist.** A new note
   has no drop point, and its text is in `Sig`, so the author's own tab
   gets a `graph` swap after saving. Options: (a) keep; (b) leave text out
   of `Sig`. Lean (b).
98. **(step 6) Upper canvases read Docker every 3 s per open stream** for
   the worst-status roll-up. Options: (a) keep; (b) cache status per
   tile for the interval. Lean (a) until it shows.
99. **(step 6) Ghost refs keep `ref:stack.X` ids** and the compare pill is
   names, releases and "behind" only. Options: (a) keep; (b) resolve
   ghosts to the target's id so a click opens it. Lean (a).

Raised by step 6 session C tasks 9 and 10 (builder took the lean; flip any):

100. **(step 6) Lanes need the canvas to watch its children.** `<graph-canvas>` re-paths only on node attribute changes, so a lanes svg swapped in by SSE keeps `d="M0,0"` until a card moves. Options: (a) B adds `childList` to the canvas observer; (b) the server draws lane paths. Lean (a). **Resolved at the merge: B already observed `childList`; checked in a browser.**
101. **(step 6) Lane labels.** A right-to-left lane's label reads upside down and both directions of a pair overlap. Options: (a) keep; (b) offset each direction and flip reversed text. Lean (b), session D.
102. **(step 6) The create-tile form opens in the drawer**, not a modal; no layout slot exists for one. Options: (a) keep; (b) add a dialog slot. Lean (a).
103. **(step 6) Repo pick is a URL field.** No verb lists a connector's repos; the help names the connector hosts. Options: (a) keep for v1; (b) add a repos verb and a select. Lean (a).
104. **(step 6) Tile settings show the tile rung only.** No verb answers the cascade at tile level, so "applies" is the tile's value or "no limit". Options: (a) keep; (b) a `TileSettings` verb with effective and decided-by. Lean (b).
105. **(step 6) `events` and `drawer` shadow tile slugs.** Echo matches the static segment before `:tile`, so a tile slugged `events` or `drawer` has no page. Options: (a) add both to `slug.Reserved`; (b) move them under a prefix. Lean (a). **Moot at the merge: both moved under `/-/`.**
106. **(step 6) Job stream sends the final state as `update`.** `JobStatus` swaps on `update` and closes on `end`, so the web job stream sends the finished body as one last `update`, then `end`; the component is untouched. Options: (a) keep; (b) the component swaps on `update,end`. Lean (a).
107. **(step 6) Log levels are not parsed.** The stream splits docker's timestamp only; `<log-pane>`'s level filter sees "" on every line. Options: (a) keep; (b) parse common level words. Lean (b), later.
108. **(step 6) Cards are not keyboard-openable.** A card is a div with `hx-get`. Options: (a) keep; (b) `tabindex`, `role="button"` and an Enter trigger. Lean (b), session D a11y.
109. **(step 6) The managed instance drawer has only a slices tab.** Restart, logs and status of the instance tile are not reachable from its card. Options: (a) keep; (b) add the tile drawer's status and logs tabs. Lean (b).
110. **(step 6) Domains attach and detach only.** No in-place edit of HTTPS, redirect or proxy extras (`UpdateDomain` unused by the web). Options: (a) keep; (b) an edit form per row. Lean (a) for v1.
111. **(step 6) Handlers mount their own routes** (`Mount(g, access)`), not one line each in `server.go`. Options: (a) keep; (b) list every route in `server.go`. Lean (a).
112. **(step 6) Domain detach and raw are not checked against the tile in the URL.** A domain id of another tile in the same env is accepted (env.write still gates it; mirrors the API). Options: (a) keep; (b) refuse unless the domain's tile is the URL's tile. Lean (b).
113. **(step 6) Backup schedules are read-only in the drawer.** The backups tab lists schedules and runs; editing a schedule stays in the CLI/API. Options: (a) keep for v1; (b) a schedule form. Lean (a).
114. **(step 6) The volume drawer makes 5 reads per open.** Volume, schedules, runs, the attached tile and the env are read separately. Options: (a) keep; (b) one `VolumeDrawer` read verb. Lean (a) until it shows up slow.

Raised by the step 6 B+C merge (builder took the lean; flip any):

115. **(step 6) Env drawers on a fresh load fetch themselves.** `drawer()` renders a `DrawerLoad` placeholder (`hx-trigger="load"`) for tile, instance, slice, volume and proxy, not the body, so a fresh `?drawer=` costs one extra request. Options: (a) keep; (b) call each drawer's render in-process. Lean (a).
116. **(step 6) The env stream gate is `org.read`.** C gated `/events` on `tile.read`; the merged `/-/events` uses the canvas's `org.read` at every level. Options: (a) keep; (b) `tile.read` at env. Lean (a), the canvas page itself is `org.read`.
117. **(step 6) `graph-canvas.ts` stays packed.** 298/300 lines with blank lines stripped, same as B; unpacking would break the budget. Ties to DECIDE 77. Options: (a) keep; (b) raise the canvas budget and unpack. Lean (a) until 77 is settled.
118. **(step 6) Every tile card opens on the status tab.** Cron and function cards too; the graph view does not feed `NewVersion` or `NextRun`, so those footer bits stay empty. Options: (a) keep; (b) feed them from the graph service. Lean (b), session D.
119. **(step 6) Logs tab without a container shows a line.** A tile with no run and no container gets "No container is running yet." instead of a `<log-pane>` whose stream 404s and retries. Options: (a) keep; (b) the stream answers an empty `end`. Lean (a).

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

**START HERE (Opus, step 0):** you are on branch `rewrite`, a fresh hamr
scaffold plus `docs/rewrite/`. Read `REWRITE.md` "Method" and
`docs/rewrite/tasks/step-0.md`, do its tasks in order, commit per task,
tick `[x] step 0` below when the done gate passes, open the PR `rewrite`
→ `master`. Do not read `../stackr-old`; the extracts in
`docs/rewrite/extracts/` are the only view of the old code. Start
`hamr dev` if it is not running. Next step reads `tasks/step-1.md`.

## Build steps (Opus, one fresh session + one stacked PR each)

- [ ] step 0 scaffold and docs
- [ ] step 1 groundwork
- [ ] step 2 docker wrapper
- [ ] step 3 services
- [ ] step 4 API + CLI
- [ ] step 5 installer + self-upgrade
- [ ] step 6 UI

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

Settled silently, listed so you can flip them: label key is `stackr.tile`
(was `stackr.app`); restart policy stays the three old values, not a bool;
scheduler keeps `robfig/cron/v3`, orphan retention and image watch are
fixed entries (`@daily`, `@every Nm`) not schedule rows, backup schedules
keep their `timezone` column composed into `CRON_TZ=`; `release-tilediff` dual
field vocabulary collapses to one (config plans are gone); `ToggleCron`
pause intent has no home and cron tiles are Later anyway.

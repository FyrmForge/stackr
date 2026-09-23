# Rewrite progress

The only source of "where are we". Read this first, never ask. Tick items as
they finish, commit this file with the work. Anything needing darhvader goes
under `DECIDE:` at the bottom; keep working on what does not depend on it.

Branch: `rewrite`. Old code: worktree at `../stackr-old` (never mention it in
the new `AGENTS.md`). Plan: `REWRITE.md` at repo root, "Method" section has
the builder rules. Commits: plain, no co-author, no AI attribution, ever.

## Overnight run (Fable, planning session, 2026-09-24)

Approved by darhvader as "option A": write everything, park decisions, do not
wait. Sub-agents run on `model: "opus"`.

### 1. Task lists → `docs/rewrite/tasks/step-N.md`

Source: REWRITE.md "Build order" row + the sections it names. One file per
step, numbered tasks, each with a "done when". Read one REWRITE.md section at
a time. Mark real decisions `DECIDE:` in the file and copy them to the block
at the bottom of this file.

- [ ] step-0.md (agreed list below)
- [ ] step-1.md groundwork: releases/release_tiles/environments/jobs schema first, rest of schema, package tree, store + docker under service/internal, depguard, service.New + orchestrator, authz.can + middleware, typed errors, flow/jobs, test harness
- [ ] step-2.md docker wrapper: containers, networks, volumes, images, builds, logs, exec; resolved specs only
- [ ] step-3.md services: every v1 feature as orchestrator methods, managed tiles, Caddy routes, `stackrd proxy`, backups, image watch, param store
- [ ] step-4.md API + CLI: API the only door, CLI plain HTTP client
- [ ] step-5.md installer + self-upgrade
- [ ] step-6.md UI: templ+htmx, shared components first; MUST include the custom-element whitelist (REWRITE.md "UI stack")

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

- [ ] extracts written, one per row (list rows here as they land)
- [ ] skimmed

### 3. Wipe → Opus sub-agent, Fable verifies

Delete everything on `rewrite` except `.claude/`, `.mcp.json`, `CLAUDE.md`,
`.gitignore`, `docs/rewrite/`, `REWRITE.md`. Run `hamr new` (temp dir + move
in if it refuses a non-empty dir). Module name `stackr`.

Verify before push: `make build`, `make lint`, `make test` pass;
`git diff master --stat` = old code gone + scaffold + docs/rewrite; kept
files untouched; `docs/rewrite/` has REWRITE.md copy or link, tasks/,
extracts/, this file.

- [ ] wiped and scaffolded
- [ ] verified
- [ ] committed and pushed (`git push -u origin rewrite`)

### 4. Hand-off

- [ ] this file updated with a "start here" line for the Opus step 0 session

## Build steps (Opus, one fresh session + one stacked PR each)

- [ ] step 0 scaffold and docs
- [ ] step 1 groundwork
- [ ] step 2 docker wrapper
- [ ] step 3 services
- [ ] step 4 API + CLI
- [ ] step 5 installer + self-upgrade
- [ ] step 6 UI

## DECIDE:

(nothing yet)

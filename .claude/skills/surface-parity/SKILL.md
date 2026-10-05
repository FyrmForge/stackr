---
name: surface-parity
description: Feature-parity gap analysis across stackr's three surfaces (web UI, API/CLI, config-as-code). Use when asked for "surface parity", "feature parity", "gap analysis", "what can the CLI/config not do", or before deciding parity work. Fans out one Explore agent per surface, diffs against the store models, and reports a fixed-format matrix compared to the last baseline.
---

# Surface parity

Three surfaces, one matrix. The goal is a diff against the baseline, not a
fresh essay each time.

## Where each surface lives

| Surface | Source of truth |
|---|---|
| Web UI | `internal/stackrd/handlers/web/server.go` (single route table), templ files under `internal/stackrd/handlers/web/handler/`, `frontend/static/js/` |
| API | `internal/stackrd/handlers/api/v1/v1.go` `Register()` (`op()` registers route + spec in one call), `docs/openapi.json`, non-v1 routes in `internal/stackrd/handlers/api/server.go` |
| CLI | `internal/cli/cmd/*.go` (cobra tree in `root.go`), `internal/cli/client.go` (one method per route) |
| Config | `internal/stackrd/config/stackconf/stackconf.go` (`File`, `TileConf`), `internal/stackrd/config/orgconf/orgconf.go`, `internal/stackrd/config/runpolicy/` (per-type key legality), `docs/features/config-as-code.md`, `docs/features/ui-staging.md` |
| Everything that exists | `internal/stackrd/store/repo/models.go`. Every entity and field here is a row in the matrix unless it is pure runtime state. |

## How to run

1. Read `docs/surface-parity.md` (the baseline) first.
2. Launch three Explore agents **in one message** with the prompts below.
   Ask each for a compact table grouped by the fixed domain list, citing
   file paths. Read-only.
3. When all three report, build the matrix. Every store entity gets a row.
   Mark ✅ / ❌ / ~ (partial) per surface, and a short note for ~.
4. Diff against the baseline. Report only: rows that changed, new rows,
   bugs found, and the ranked holes. Then overwrite the baseline.

### Agent prompts

**API/CLI**: list every route in `docs/openapi.json` and in `v1.go`
`Register()`. Flag routes in code but not the spec and vice versa. For every
cobra command list flags and the client method / route it calls. Report
API routes with no CLI command, CLI commands hitting undocumented routes,
and dead client methods (no cobra caller).

**Web UI**: walk the route table in `web/server.go`. For every page and
POST action say what a user can do, grouped by domain. Note gating
(`adminOnly`, `ReadOnlyGuard`, config-managed read-only). List UI-only
features (no API/config equivalent) and read-only views.

**Config**: list every top-level and nested key in `stackconf.File`,
`TileConf`, and `orgconf.File`. Then walk `models.go` and list every entity
or field that is NOT declarable. Note reference-only names (connector,
ssh_key, storage, registry). State how config is applied (triggers, dry-run,
apply policy, drift handling) and whether any export / round-trip exists.
Check `docs/plans/*.md` for agreed-but-unbuilt config items.

### Fixed domain list

org · env · stack · tile · vars/secrets · deploy/run/rollback · promote/releases ·
plan/approve · backups · storage/volumes · shared infra/provisions ·
domains/proxy/port-forward · runtime settings cascade · members/roles/invites ·
connectors/ssh keys/registries · nodes/containers · admin · account/api keys/
notifications/sharelinks · canvas/staging/browsers (UI-native) · export

## Output format

1. One lead line: what changed since the baseline.
2. Matrix (Feature | Web UI | API/CLI | Config).
3. **Bugs found on the way** (spec drift, dead code, doc lies).
4. **Ranked holes** with a one-line why each.
5. **By design, not a gap** (see below). Don't re-report these as holes.

## Known edge cases

Grow this list. A case goes here when it has fooled a run once.

- `op()` path replacer in `v1.go` only knows `:id :dep :name`. Any other
  param (`:run`) makes the spec entry fail silently, so the route vanishes
  from `openapi.json` on every `make openapi`. Always diff code vs spec,
  never trust the spec alone.
- `POST /api/v1/nodes/samples` sits outside the v1 group, skips KeyAuth,
  authenticates on the node-agent key. Not a public API route.
- `/cli/authorize` and `/cli/exchange` are web routes the CLI login uses.
  Not in openapi.json and shouldn't be.
- Hidden legacy cobra aliases (`stacks`, `envs`, `domains`) are not
  separate features.
- `--app` is rewritten to `--tile` globally in `root.go`.
- Config `scope` on a tile is structural (`yaml:"-"`), not a YAML key.
- Stack snapshot skips org-scoped managed instances (`ScopeKind=="org"`).
  Only the org file owns them. Not drift.
- Panel-created domain resources are adopted (`Declared=false`), not
  deleted by drift.
- `canonEnv` loses multi-line env values on round-trip. Known wart, not
  a parity gap.

## By design, not a gap

- **UI staging** is UI-only. API writes bypass it on purpose
  (`api/v1/apps.go` ponytail comment, `docs/features/ui-staging.md`).
- **Port-forward** is CLI/API only. UI shows tunnels, never opens one.
- **Deploy / rollback / stop / restart** are imperative. Config never
  declares them.
- **Secret values** are never in config. Only declarations.
- **Config binding** (repo/branch/path) is panel-owned. Chicken and egg.
- **Canvas** (positions, annotations, groups), **DB data browser**,
  **file browsers**, **container terminal** are UI-native.
- Per-domain port, custom cert, provisions, env secret values: staging
  carve-outs listed in `docs/features/ui-staging.md`.
- A model with store methods and a config key can still be dead. Check for
  a create path (form, route, CLI) before calling it a feature. SSH keys
  and the per-tile `registry_id` column were orphans from the first commit.
- Tables that are only written (`AuditEvent`) are a gap on every surface,
  not zero gaps. Ask which surface should read them.
- Reference-only names in config (connector, registry, backup destination)
  need a lookup rule. Check whether the resolver scopes them or just
  matches by name.
- The `{id}` in API paths: check whether a slug is accepted. An API that
  only takes ids with no list route is a gap even when every route exists.

## Decisions on record

`docs/plans/38-surface-parity.md` holds the agreed design for every gap in
the 2026-09-13 baseline. Do not re-propose those; report them as planned or
built.

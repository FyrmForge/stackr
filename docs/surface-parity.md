# Surface parity baseline

Last run: 2026-09-13, after all eight groups of `docs/plans/38-surface-parity.md`
were built. Produced by the `surface-parity` skill. Overwrite on every run.
Legend: ✅ has it · ❌ missing · ~ partial.

## Matrix

| Feature | Web UI | API/CLI | Config |
|---|---|---|---|
| Orgs: list, create, rename, delete, switch | ✅ | ~ `GET /orgs` + `org ls` only | ❌ |
| Stacks: create, list, delete, rename | ✅ | ✅ | ✅ (org file `stacks:`) |
| Envs: create, list | ✅ | ✅ | ✅ |
| Envs: delete, reset, copy, colour | ✅ | ✅ | ~ delete via drift, colour only |
| Envs: mark intended | ✅ | ❌ | ❌ |
| Env ladder order | ✅ | ❌ | ✅ (list order) |
| Per-env apply policy | ✅ | ✅ (`env set --apply-policy`) | ✅ (`apply_policy:`) |
| Per-env `protected` | ✅ | ❌ by design (no column) | ✅ |
| Per-env config branch | ✅ | ❌ | ❌ |
| Tiles: create, get, update, delete | ✅ | ✅ | ✅ |
| Tile runtime keys (limits, user, shm, devices, restart, healthcheck, replicas, node_group) | ✅ | ✅ | ✅ |
| Vars: org / stack / env / tile | ✅ | ✅ | ✅ org, stack, per-env `vars:` |
| Secret declarations (generated, required, per-env versions) | ~ generate button only | ~ `vars set --generate` | ✅ |
| Reference catalogue, stored vs resolved vars | ✅ | ✅ | n/a |
| Deploy, run now | ✅ | ✅ | by design |
| Rollback, stop, restart, cancel deployment, stop run | ✅ | ✅ | by design |
| Deployment list, deployment logs, metrics | ✅ | ✅ | n/a |
| Live logs | ✅ | ✅ | n/a |
| Promote / releases | ✅ | ✅ | by design |
| Env compare | ✅ | ❌ | n/a |
| Config plan: preview, plan now, list, show, approve, reject | ✅ | ✅ stack, ~ org (no read-one) | n/a |
| PR envs | ✅ | ✅ | ✅ |
| Cron toggle | ✅ | ✅ | n/a |
| Backups: schedules, run, restore, runs | ✅ | ✅ | ✅ (`backup:` on a tile) |
| Backup destinations, org-shared flag | ✅ | ✅ | ref only by `${{ org.backups.NAME }}` |
| Panel self-backup | ✅ | ❌ | ❌ |
| Storage pools + sub-paths + probe | ✅ | ✅ | ❌ ref only |
| Storage attach to tile | ✅ | ✅ | ✅ |
| Volumes on tile | ✅ | ✅ | ✅ |
| Volume `max_size_mb`, `volume_name` | ✅ | ✅ | ❌ |
| Volume / bucket file browser | ✅ | ❌ | UI-native |
| Shared infra: managed instances | ✅ | ✅ | ✅ |
| Provisions: create, attach, detach, fork, drop, public toggle | ✅ | ✅ | ~ `from:` slices only |
| DB port / scope | ✅ | ✅ | ✅ |
| DB data browser | ✅ | ❌ | UI-native |
| Tile domains: add, rm, auto, https, force_https | ✅ | ✅ | ✅ |
| Tile domain custom cert, per-domain port | ✅ | ❌ | ❌ |
| Domain resources (org, stack, node) + `acme_email` | ✅ | ~ no CLI for PATCH acme | ✅ org, stack |
| Proxy: static override, entries | ✅ | ✅ | ~ `traefik_override` per tile |
| Port-forward | display only | ✅ | n/a |
| Settings cascade (server, org, stack, env) | ✅ | ✅ | ✅ (`defaults:`) |
| Members, roles, invites | ✅ | ✅ | ❌ by design |
| Registries (managed creds, images, tags, external) | ✅ | ✅ | ❌ |
| Connectors (GitHub) | ✅ | ❌ | ref by raw id, unscoped |
| Nodes: add, join, drain, move, remove, group, activate | ✅ | ❌ | ❌ |
| Containers: list, lifecycle, logs, terminal | ✅ | ❌ | UI-native |
| Admin: users, TLS, maintenance, image-watch, DNS, registry domain | ✅ | ❌ | ❌ |
| Audit log | ✅ read-only | ❌ | ❌ |
| Account: profile, password, API keys, notifications, theme | ✅ | ❌ | ❌ |
| Sharelinks (mint, revoke, public reveal) | ✅ | ❌ | ❌ |
| Global search, canvas layout, staging | ✅ | ❌ | UI-native |
| Env colour defaults, `ui_edits` default | ✅ | ✅ (`org defaults`) | ✅ (org `defaults:`) |
| Onboarding wizard | ✅ | ❌ | n/a |
| Export state to YAML | ✅ | ✅ | ✅ (`ExportFile`, round-trip tested) |
| `moved:` renames | ❌ | ❌ | ✅ |

## Bugs found on the way

- **Cross-tenant connector.** Fixed, see `docs/plans/39-codex-review-fixes.md`
  point 6. A tile's `connector:` key is a raw connector id
  taken straight from the stack file (`config/stackconf/apply.go:1811`), and
  `connectorForTile` (`infra/githubapp/githubapp.go:318-321`) looks it up by id
  with no org check before minting a GitHub installation token for the clone.
  A stack file naming another org's connector id borrows that org's GitHub
  credential. Same class as the registry hole plan 38 §5c just closed, and the
  only reference in config that is not org-scoped: `backup.dest` resolves
  org+name (`infra/backup/authz.go:77-107`), storage pools are server-scoped by
  design.
- Dead client method `UnresolvedVars` (`internal/cli/lifecycle.go:53`): no cobra
  command calls it. Its route now points at the same handler as `getVars`.
- Dead client method `AddVolume` (`internal/cli/client.go:485`), superseded by
  `AddVolumeWith`.
- `PATCH /domain-resources/{id}` sets a resource's ACME account and has no CLI
  command, so `acme_email` is settable from web and config but not from the CLI.
- `POST /apps/{id}/provision` has no CLI command.
- Org config plans have no read-one route, so `stackr org approve` cannot show
  the plan first. Stack plans have `GET /config/plans/{id}`.
- The old `op()` spec-drift bug is gone: `specPath` maps any `:param`
  generically and `op()` panics on a spec error, so a route that is not in
  `docs/openapi.json` is now a startup crash. Code and spec match 1:1.

## Ranked holes

1. Nodes and containers are web-only. No way to drain a node or restart a
   container from CI.
2. Admin (users, DNS, TLS, image-watch, maintenance, registry domain) is
   web-only. A fresh instance cannot be configured headlessly.
3. Audit events are readable on one admin page and nowhere else. No API, no
   export, no per-org view.
4. Account and API-key management is web-only, so a key cannot be rotated
   from the CLI that uses it.
5. Sharelinks and notifications are web-only.
6. Env compare and mark-intended are web-only; a CI ladder cannot assert that
   two envs match.
7. Tile domain custom cert and per-domain port are web-only.

## By design, not a gap

UI staging, canvas state, DB data browser, file browsers, container terminal,
search palette, onboarding wizard. Port-forward is CLI/API only. Deploy,
rollback, stop and restart are imperative and never declared in config. Secret
values never appear in config, only declarations. Config binding (repo, branch,
path) is panel-owned. Members stay out of config on purpose. Per-env
`protected` is a file key that gates deletes during apply, not a column, so it
is deliberately absent from the env PATCH.

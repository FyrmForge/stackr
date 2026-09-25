# Schema design note (step 1)

One table per leaf. Columns start from `extracts/001_initial.md`, kept as they
are when v1 needs them; everything else is a plan override, listed per table.
Two migrations: `002_design` (A: releases, release_tiles, environments, jobs)
and `003_rest` (B: everything else). `001_initial` keeps hamr's `sessions` and
`users`.

Conventions:

- Ids are `TEXT` (uuid), set by the caller, never by the store.
- Time columns are `DATETIME` (modernc scans those back into `time.Time`);
  nullable ones map to `*time.Time`.
- Booleans are `INTEGER` 0/1.
- Every parent id is a real FK with its `ON DELETE` in SQL. Polymorphic
  scopes (`scope_kind` + `scope_id`) cannot be an FK; they get one `AFTER
  DELETE` trigger per parent table instead, so the cascade is still SQL
  (SQLite fires triggers for rows removed by an FK cascade too; tested).
- Enumerated columns carry a `CHECK`. The store sets no defaults of its own
  for them; the leaf does.
- Encrypted at rest (`enc1:` prefix, `service/internal/secrets`): param
  values, credential passwords, connector config, managed admin password,
  provision passwords and outputs, destination secret key and archive key.

## Migration A: the release model

| table | leaf | columns | overrides vs 001 |
|---|---|---|---|
| `releases` | release | id, stack_id → stacks CASCADE, number, created_at, created_by | new. `UNIQUE (stack_id, number)`. `created_by` is a user id kept as text (audit, no FK: a deleted user keeps the release). |
| `release_tiles` | release | id, release_id → releases CASCADE, slug, repo, branch, commit_sha, image_id → images SET NULL (nullable), digest | new. `UNIQUE (release_id, slug)`: mapped by slug. `commit` is a reserved word in SQLite, so the column is `commit_sha`. Surrogate `id` so every table has the same CRUD shape. |
| `environments` | environment | id, stack_id → stacks CASCADE, name, slug, type, base_env_id → environments SET NULL (nullable), settings, color, position, network, release_id → releases SET NULL (nullable), from_kind `branch\|promote`, from_branch, auto, created_at | `apply_policy`, `config_branch` gone (→ `from_kind`/`from_branch`/`auto`). `proxy_ip`, `proxy_cidr` gone: the proxy never joins an env network. `base_env_id` becomes a nullable FK (was `''`). `UNIQUE (stack_id, slug)`. |
| `jobs` | job (rows, states) + flow/jobs (runner) | id, kind, state `queued\|running\|waiting\|done\|failed\|superseded\|cancelled`, release_id → releases SET NULL (nullable), lock_set (JSON array of tile ids), payload (JSON, kind-specific), waiting_param (nullable), log_path, error, created_at, started_at, finished_at | new, replaces `deployments` + `work_items`. No `deployments` table. `payload` kept from `work_items` (a job has to know what to do). `step`/`progress`/`attempts`/`dedupe_key` not taken: the lock set replaces the dedupe key; the build/swap phase lives in the runner's memory (a crashed running job is failed, never resumed). 30-minute cap is the worker's. |

## Migration B: everything else

| table | leaf | columns | overrides vs 001 |
|---|---|---|---|
| `orgs` | org | id, name, slug UNIQUE, avatar_path, env_colors, settings, setup_done_at, setup_mode, created_at, config_connector_id, config_repo, config_branch, config_path, config_auto | `ui_edits` gone. `setup_mode` back with the setup wizard (step 7 task 11): its branch, `config` or `ui`; not in `settings`, which the file's `defaults:` overwrites. `config_*` are the org config file's binding, the same four `stacks` has, plus `config_auto` (apply unblocked plans without a click, default off). `config_repo` is stored as the https URL. |
| `org_config_plans` | orgplan | id, org_id → orgs CASCADE, commit_sha, summary, plan (JSON), status `pending\|clean\|error\|superseded\|applied\|rejected`, error, created_at, decided_at | old `org_config_plans` with a real FK, so an org delete takes its plans. `commit_sha`, not `commit` (an SQL keyword). Index on (org_id, created_at DESC). |
| `org_members` | org | id, org_id → orgs CASCADE, user_id → users CASCADE, role, created_at | `UNIQUE (org_id, user_id)` replaces the composite PK (surrogate id, same CRUD shape). v1 writes `owner` only; the column stays for the ladder ceiling. |
| `invites` | org | id (the link token), org_id → orgs CASCADE, email, role, created_by, created_at, expires_at, used_at | as 001. |
| `users` | user | id, email UNIQUE, password_hash, name, role `admin\|user`, active, avatar_path, theme, created_at, updated_at | `notify_prefs`, `graph_prefs` gone. `role` `admin` = stackr admin. Lives in `001_initial` (hamr's). |
| `api_keys` | user | id, user_id → users CASCADE, org_id → orgs CASCADE (nullable), name, token_hash UNIQUE, created_at | `org_id` from 002, but `CASCADE`, not `SET NULL` (DECIDE 13): a key bound to a deleted org must not become an unbound key. `scopes` gone, keys have none in v1. |
| `sessions` | user (closes them on revoke); hamr's session manager reads and writes them | as 001 | none. `subject_id` stays FK-less (hamr's contract). |
| `stacks` | stack | id, org_id → orgs CASCADE, name, slug, description, settings, config_connector_id, config_repo, config_branch, config_path, domains, created_at | `org_declared`, `ui_edits`, `proxy_middlewares` gone. `config_*` stay: the release pins the config repo's commit. New: `domains` (JSON list of the stack file's host reservations: host, acme_email, include_env_on_default). `UNIQUE (org_id, slug)`. |
| `tiles` | tile | id, stack_id → stacks CASCADE, environment_id → environments CASCADE, name, slug, kind `service\|image\|managed`, git_url, git_branch, image_ref, dockerfile_path, build_context, watch_paths, env_json, build_args, volumes (mount lines), command, container_port, published_ports, endpoint_protocol, health_path, healthcheck_cmd, healthcheck_interval_s, healthcheck_timeout_s, healthcheck_retries, healthcheck_start_period_s, cpu_limit, mem_limit_mb, user, shm_size_mb, privileged, devices, restart_policy, depends_on, files, shared_net, replicas, update_policy `manual\|auto`, tag_policy, created_at, updated_at | `status` gone (derived). `source_type` folded into `kind`. `env` → `env_json` (literal-or-ref block). Cron columns (`cron`, `allow_overlap`, `timeout_minutes`, `last_*`, `run_on_deploy`) gone, cron tiles are later. `basic_auth_*`, `sec_headers`, `traefik_override` → `domains.proxy_json`. `connector_id` gone (resolved from `git_url` host). `webhook_token` gone (pushes arrive through the connector). `external_port` gone (`published_ports`). `endpoint_port_var`, `storage`, `wait_for_ci`, `home_node`, `node_group` gone. `image_digest`/`latest_digest` → `images` + `release_tiles.digest`. New: `health_path`, `tag_policy`. `UNIQUE (environment_id, slug)`. |
| `images` | image | id, ref UNIQUE, digest, built_at (nullable), last_digest, last_tag, last_error, checked_at (nullable), created_at | new. Built images (`digest` = local, `built_at` set) and the watch cache (`last_*`, `checked_at`), one row per `registry/repo:tag`, deduped by `ref`. |
| `params` | params | id, scope_kind `org\|stack\|env`, scope_id, collection, name, kind `param\|secret`, value (encrypted), created_at, updated_at | old `variables`. No tile scope. `UNIQUE (scope_kind, scope_id, collection, name)`. Scope cascade by trigger on orgs, stacks, environments. Every value is encrypted (one rule, as the old table). |
| `settings` | settings | key PK, value | as 001. Install-wide knobs, plus key `defaults` = the server rung of the cascade (a `settings` JSON blob like the org/stack/env ones). `acme_email`, `panel_domain` are keys here. |
| `volumes` | volume | id, scope_kind `env\|stack\|org`, scope_id, instance_id → managed_instances SET NULL (nullable), slug, name (Docker volume), max_size_mb, orphaned_at (nullable), created_at | new (was tile columns). Task text says "env_id or owning instance"; an instance can be stack- or org-scoped and its volume follows that scope, so the volume carries the scope itself. Instance removal sets `instance_id` NULL and leaves the row (the flow sets `orphaned_at`, the schema never deletes data). `UNIQUE (scope_kind, scope_id, slug)`. Scope cascade by trigger. |
| `domain_resources` | domainres | id, level `instance\|org\|stack`, org_id → orgs CASCADE, stack_id → stacks CASCADE, host UNIQUE, include_env_on_default, acme_email, declared, created_at | v0's `owner_id` split into two real FKs so an org or stack delete takes its rows; a CHECK ties the level to which id is set. `declared`: someone asked for the row; stackr's own (the root seed, an org's default) are undeclared and lose ties. |
| `domains` | domain | id, tile_id → tiles CASCADE, host, path, container_port, https, force_https, redirect_to, auto, resource_id → domain_resources (NO ACTION), position, proxy_json, raw_caddy, created_at | `cert_pem`/`key_pem` gone (Caddy automatic TLS). `middlewares`/`priority`/`rule` (Traefik) → `proxy_json` named extras + admin-only `raw_caddy`. Unique index on `(host, path)`. `resource_id`: the resource that named an auto or apex host; a named resource cannot be deleted. NO ACTION, not RESTRICT: RESTRICT fires mid-cascade and fails an org or stack delete. |
| `credentials` | credential | id, org_id → orgs CASCADE, name, url, username, password (encrypted), created_at | old `registries` minus `domain`/`managed` (built-in registry is later), plus `org_id`. `UNIQUE (org_id, name)`. |
| `connectors` | connector | id, org_id → orgs CASCADE, provider, name, host, config (encrypted), created_at | `host` new: one connector per org and host, `UNIQUE (org_id, host)`. `config` holds the App private key, so it is encrypted (not in the task's list; added). |
| `managed_instances` | managed | id, tile_id → tiles CASCADE, engine, scope_kind `env\|stack\|org`, scope_id, admin_user, admin_password (encrypted), endpoint, created_at | new (was `tiles.engine`/`db_*`). The instance container is its tile (`kind = managed`). Scope cascade by trigger. |
| `provisions` | managed | id, instance_id → managed_instances CASCADE, consumer_tile_id → tiles SET NULL (nullable), slug, db_name, db_user, db_password (encrypted), outputs (encrypted JSON), public, on_remove, created_at | one slice per consumer. The old `managed_resources`, `resource_bindings` and `resource_outputs` fold in: a provision row *is* the binding ("referencing is not access"), its outputs are a JSON blob. Consumer removal sets NULL so `flow/managed` still finds the slice and applies `on_remove`. `status` gone (derived). |
| `backup_destinations` | backup | id, org_id → orgs CASCADE (nullable = admin-global), kind `local\|s3`, name, endpoint, region, bucket, access_key, secret_key (encrypted), archive_key (encrypted), shared, created_at | `kind` new (local destination on every install). `archive_key` new: the per-destination `age` key. |
| `backup_schedules` | backup | id, volume_id → volumes CASCADE, method, dest_id → backup_destinations SET NULL (nullable = the local default), cron, timezone, keep, mode, created_at | old `backups` moved from the tile to the volume. `method` never derived. `enabled` gone (delete the schedule). |
| `backup_runs` | backup | id, kind `volume\|panel`, volume_id → volumes SET NULL (nullable), schedule_id → backup_schedules SET NULL (nullable), dest_id → backup_destinations CASCADE, trigger, status, object_key, size_bytes, error, created_at, finished_at | `volume_id` nullable + `kind` so the panel self-backup has a row. Runs outlive their volume (SET NULL): the orphan job backs up then deletes, and that archive must stay restorable. |

## Tables with no leaf

None. `sessions` is written by hamr's session manager through the store's
`auth.SessionStore` methods; `leaf/user` is the one that closes them.

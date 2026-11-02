# Config files

Three YAML files describe an install in git. Each has a plan, then an approve that applies it.

| File | Scope | Bound with |
|---|---|---|
| `stackr-server.yml` | The install: settings, routes, backup destinations, domains, connector shares, orgs, nodes, placements | `stackr server bind --repo <repo>` |
| `stackr-org.yml` | One org: tiers, params, defaults, stacks, domains, shares, placement | `stackr org config-repo --repo <repo>` |
| `stackr-compose.yml` | One stack: tiles, envs, volumes, domains, params | the org file's `stacks:` entry |

Every file starts with `version: 1`. An unknown key is refused with its line number. A key left out is not touched; a block that is present is the whole list, so what it no longer names becomes a removal row that applies only when ticked.

## The loop

1. A push to the bound branch, a daily re-plan, or `stackr server plan` / `stackr org plan` stores a plan. Nothing changes yet.
2. Read it: `plans`, `plan-show <plan>`. `preview -f FILE` diffs a local file without storing it.
3. `stackr server approve <plan>` (or `stackr org approve <plan>`) applies it. A plan with impact lines asks first. `--remove KEY` ticks a removal row.
4. `stackr server apply -f FILE` plans and applies a local file from any machine with an admin key.
5. `export` writes the live install as a file. A fresh export plans clean.

With `auto` on, a plan with no impact lines and no removals is approved by itself. That includes the daily re-plan and the one at panel start: a panel edit the file does not hold is put back.

An apply does only what the approved plan showed. It re-reads the file and the install first; if anything differs from the plan (a panel edit since, a change that landed), it stops with "plan again" and the next plan shows the new rows.

An apply is not one transaction. It runs verb by verb and some steps push to the proxy, DNS, git or other nodes, which cannot be rolled back. A failure leaves what already applied in place, the plan ends in error with the step that failed, and a new plan shows the rest. There is no retry verb: fix the cause and plan again.

A file that does not fetch or parse stores an error plan, opens a problem on the overview and sends the `config.invalid` alert once. Bound files are re-planned daily; changes waiting for approval show as a problem too, which is how a panel edit the file does not hold surfaces.

## Secrets

A file goes in git, so it never holds a credential. These fields must be one `${{ }}` reference and a literal is refused:

- `protect_password` in any `defaults:`
- `proxy.basic_auth.password` on a tile domain
- `alert_webhook_url` and `heartbeat_url` in `stackr-server.yml` settings, as `${{ server.params.<collection>.<name> }}`
- the access, secret and archive keys of a backup destination

Params of type `secret` are declared by name and type only. Set the value in the panel or with `stackr params`. Server params are readable only in `stackr-server.yml`; a stack file that refs one is refused.

An export writes a secret by name only. A stored literal `protect_password` is left out. The stack export swaps the value of an env or build arg whose name looks like a secret (password, secret, token, key, credential, private, auth, dsn) for `${{ params.<tile>.<name> }}` and warns: create that param before you plan the file.

## stackr-server.yml

| Key | Holds |
|---|---|
| `settings` | Install knobs by key (below). A key left out is not touched; `null` or `""` resets it. |
| `defaults` | The server rung of the cascade: `cpu_limit`, `mem_limit_mb`, `protect`, `protect_user`, `protect_password`. |
| `params` | Server params: collection, then name, `{type: param|secret}`. A secret has no value here. |
| `routes` | External routes: `host`, `mode` (passthrough, http, https), `target`, `insecure`. |
| `backup_dests` | Global S3 destinations: `name`, `endpoint`, `region`, `bucket`, `access_key`, `secret_key`, `archive_key` (refs), `shared`. |
| `domains` | Instance domain resources: `host`, `from` (rename), `include_env_on_default`, `acme_email`. |
| `connectors` | Server connectors by `name` and `share` (`all` or a list of org slugs). It never creates one; connect it in the panel. |
| `orgs` | Orgs to create and bind: `slug`, `name`, `repo`, `branch`, `path`, `connector` (a server connector name), `auto`. |
| `nodes`, `placements` | By name. The file declares, never creates a node. |

Settings keys:

- Cascade and limits: `workers`, `image_check_interval`, `orphan_retention_days`, `backup_run_concurrency`, `backup_restore_concurrency`.
- Domains and proxy: `panel_domain`, `root_domain`, `acme_email`, `ipv6`, `trusted_proxies`, `proxy_custom`.
- Cleanup: `cleanup_enabled`, `cleanup_schedule`, `registry_cleanup_schedule`, `registry_keep`, `orphans_enabled`, `orphans_schedule`.
- Panel backup: `panel_backup_enabled`, `panel_backup_schedule`, `panel_backup_keep`, `panel_backup_dest` (a destination name).
- Observe: `observe_enabled`, `observe_metrics_retention`, `observe_min_free`, `observe_logs_enabled`, `observe_logs_retention`, `observe_logs_max_size`, `observe_max_memory`, `observe_access_query`, `observe_access_ip`, `observe_access_headers`.
- Alerts: `alert_webhook_url`, `heartbeat_url` (refs).

The `server_config_*` knobs are the binding itself. Set them with `stackr server bind`, never in the file. `stackr knobs` lists every knob with its default.

## stackr-org.yml

| Key | Holds |
|---|---|
| `org` | The org's name. A different name renames it. |
| `tiers` | Slug to `{locked: bool}`, bottom first. |
| `params` | Org params by env key (a tier slug, `all` with no tiers, `pr`), then collection and name. |
| `defaults` | The org rung of the cascade, as in the server file. |
| `env_colors` | Env slug to colour. |
| `stacks` | Slug to `repo`, `branch`, `path`, `connector` (a connector name), or `path` alone for a file in the org repo. |
| `domains` | The org's domain resources. |
| `shares` | Network shares: `kind`, `source`, `options`, `user`, `password` (a ref). |
| `moved` | Stack or env renames. |
| `pin`, `ingress`, `exits` | Placement names. An empty `exits:` clears the ladder. |

## stackr-compose.yml

Written by hand or with `stackr stack export [--env ENV]`. Top level: `version`, `stack`, `include`, `ladder`, `head`, `params`, `defaults`, `domains`, `volumes`, `base.tiles`, `environments`, `pr_envs`, `shared`, `pin`, `ingress`, `exits`. A tile's keys follow the panel's tile settings (`image` or `build`, `port`, `env`, `volumes`, `domains`, `healthcheck`, `limits`, `depends_on`, and so on). `volumes.<name>.backup` is refused: set a volume's backup schedule in the panel.

## What a file cannot say

Members, roles, invites, users and API keys. Host grants. Registry credentials. Raw Caddy config and VIPs. Server connector creation. Orgs and stacks the panel made and no file binds are left out of an export.

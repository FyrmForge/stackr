# Feature: Config as Code (`stackr-compose.yml`)

**Status: shipped** on all four surfaces (config file, HTTP API, CLI, panel).
Org-level config-as-code (`stackr-org.yml`) builds on top of this — see
`docs/plans/02-serverconfig-migration.md` §6 and `internal/stackrd/config/orgconf`.

## Summary
Define a stack — tiles, environments, secrets, PR-env rules — declaratively in
a versioned YAML file in a git repo. Terraform-style lifecycle: every push to
the bound branch produces a **plan** (diff against current state); clean plans
auto-apply is opt in per env, everything else waits for approval
in the panel. The file owns the keys it declares; secret values and server-side
reality stay out.

## The file

Schema authority: `internal/stackrd/config/stackconf/stackconf.go` (strict
decoding — unknown keys are parse errors, not ignored). Block YAML only; the
serializer never emits flow style.

```yaml
version: 1
stack: hamrstack

pr_envs:
  enabled: true
  comment: true             # sticky PR comment
  status: true              # commit status
  against:                  # PR target branches that spawn an env (empty = any)
    - master
  tiles:                    # overlays base tiles, same rules as an env
    worker: false

include:                    # optional, merged in order after this file, later wins
  - deploy/workers.yml

secrets:                    # declared at stack level, valued per env — names, never values
  STRIPE_KEY:               # bare = set out of band; plan warns while unset
  SESSION_SECRET:
    default: generated      # minted on the first apply that finds it unset, then stable forever
    length: 48              # generated only; 0 = 32
    required: true          # apply refuses while unset (instead of warn + deploy)
    env_versions: true      # default true: each env its own value; false = one stack-wide value

shared:                     # stack-scoped managed instances, shared by every env
  sharedpg:                 # lives in the stack's home, not in any env
    type: managed
    engine: postgres        # postgres | mariadb | mongo | redis | s3
    external_port: 25432    # publish on the host (omit = internal only)
    limits:
      cpu: 1.0
      memory_mb: 512

base:                       # tiles defined once, landing in EVERY static env
  tiles:
    web:
      type: service         # service (default) | cron | function | managed | volume
      build:
        context: .
        dockerfile: Dockerfile
      port: 8080
      domains:
        - host: hamrstack.io
        - host: www.hamrstack.io
          redirect_to: hamrstack.io
        - auto: true        # generated name under the nearest visible domain resource
      env:
        PORT: 8080
        DATABASE_URL: ${{ tile.web-db.DATABASE_URL }}
      depends_on:           # startup order on bulk starts (apply, boot resync)
        - seed:completed    # started (default) | healthy | completed
      files:                # repo files or folders shipped into the container, read-only
        - config/app.yml:/etc/app/config.yml:template   # :template = varref over the bytes
        - dashboards:/etc/grafana/dashboards           # a folder ships the whole tree
      storage:              # declared storage-tile sub-paths, or an org share
        - pool-ssd/webdata:/data
        - ${{ org.storage.media }}/tv:/tv
      volumes:
        - uploads:/app/uploads
      healthcheck: curl -f http://localhost:8080/health
      healthcheck_interval: 5   # docker-native check; also timeout/retries/start_period
      restart: always           # or on-failure, or no (exit and stay down)
      limits:
        cpu: 1.0
        memory_mb: 256

    web-db:                 # slice: logical db/bucket cut from a managed instance
      from: sharedpg        # instance | stack.instance | org.stack.instance | org.stack.env.instance
      on_remove: keep       # keep (default) | drop

    seed:
      type: function        # one-shot: cron minus the schedule; status = last run
      image: alpine:3
      command: ./seed.sh
      run_on_deploy: true   # also runs at its topo position during an apply

    cleanup:
      type: cron
      image: alpine:3
      schedule: "0 3 * * *"
      command: sh /scripts/cleanup.sh
      timeout_minutes: 30

environments:               # list or map; first = default env (bound branch deploys here)
  production:
    protected: true         # one-way ratchet on the panel apply policy
    color: teal             # how the panel draws it: violet teal amber rose sky lime, or #rrggbb
  staging:
    tiles:
      web:                  # sparse overlay onto the base tile
        env:
          APP_ENV: staging
      cleanup: false        # excluded in this env
```

More service keys, all optional: `command` (CMD override, argv shell-split),
`user`, `shm_size_mb`, `privileged`, `devices`, `watch_paths`, `build_args`,
`published_ports`, `traefik_override` (replaces the generated proxy file
verbatim; address backends by slug), `basic_auth_user`/
`basic_auth_hash`, `security_headers`, `branch`, and explicit git source
(`git_url`/`connector` — config-managed stacks normally inherit the
binding). Image-source runnables take `update_policy: off|notify|auto` (the
registry watcher: badge + notification, or auto-redeploy on a new digest);
git-built ones take `wait_for_ci: true` (a push parks its deploy until the
commit's checks pass). Domain entries set exactly one of `host` / `apex` /
`auto`, plus optional `path`, `https` (default true), `redirect_to`,
`middlewares`, `priority`, `rule` and `port` (see Domains); the first listed is
what `STACKR_PUBLIC_URL` resolves to.

Managed instances take `image:` as a per-instance override of the engine
default (you own upgrade compatibility), `shm_size_mb` and `update_policy`.
Volume tiles take `attach:` (consuming tile slug) and `path:`.

## Semantics
1. **Binding** lives on the stack (panel): connector + repo + branch + path
   (default `stackr-compose.yml`). Set at "New stack from repo" or in stack
   settings. Never auto-detected.
2. **Plan** on every push to the bound branch (via the connector webhook):
   one row per rung. The stack row carries the first declared env and the
   stack-level changes; every static env above it gets its own row, even
   when its diff is empty.
3. **Apply policy per env** (server-side, panel setting): `auto` | `manual`.
   Unset means auto for the first env and manual for the rest.
   `protected: true` in the file is a one-way ratchet — can tighten, never
   loosen. Deletes gate on approval everywhere, always.
4. **Promotion**: the declared order of `environments:` is a ladder. A push
   builds and deploys the first env only. Everything above it is promoted from
   the stack's **releases page** (`/<org>/<stack>/releases`): one row per
   commit, a Promote button per rung that does not run it, and a dialogue
   listing the commits going in. A plan row still offers Apply (config only,
   services restart on the image they already run) and its Promote opens the
   same dialogue — which, when a config plan is pending, asks whether to send
   the images alone or apply the plan with them. Images are named by commit,
   so a promote reuses the build the env below made and builds only when none
   exists. Tiles with their own `git_url` and `image:` tiles do not promote.
5. **Ownership**: the file owns exactly the keys it declares; managed fields
   are read-only in the UI with a "config" badge. Tiles/envs present in the
   stack but absent from the file are deletion candidates (strict mode).
6. **Environments**: base tiles land in every env; env entries are sparse
   overlays; `false`/null excludes; a full body under an env declares an
   env-local tile. Omitted `environments:` → single `production`. PR envs are
   built purely from the file (base + `pr_envs.tiles`), never cloned from a
   live env.
7. **References**: `${{ tile.<slug>.<output> }}`, `${{ stack.vars.<name> }}`,
   `${{ stack.secrets.<name> }}`, `${{ org.vars.<name> }}`,
   `${{ org.secrets.<name> }}`, `${{ stackr.PROXY_IP|PROXY_CIDR }}` — resolved by
   `internal/stackrd/config/varref` in env values, volume lines and command.
   Nothing degrades silently; an unresolved reference never reaches a
   container.
7. Missing file on the bound branch → no-op; stack stays UI-managed.
8. Tile key = slug; rename = delete + create (plan says so, gates).

## Stack and environment vars

`vars:` is a map of plain values the file carries. Top level sets them for the
whole stack (`${{ stack.vars.NAME }}`); the same key under
`environments.<slug>.vars:` shadows one for that environment, same reference.
Per-env vars need the map form of `environments:`; a list of names cannot carry
them.

Credentials belong under `secrets:`, which holds declarations only. A name in
both is a load error, and a name already stored as a secret is a plan error
rather than an overwrite.

Additive and update only: nothing records where a stored variable came from, so
a name the file stops declaring is left alone rather than deleted.

## Domains

`domains:` at stack or org level takes `acme_email:`, the Let's Encrypt account
certificates under that host are issued on. Blank uses the instance's.
stackrd writes one cert resolver per distinct address into traefik's static
config, so a new address restarts traefik once.

A tile domain takes `force_https:` alongside `https:`. They are two questions:
`https:` serves TLS, `force_https:` bounces plain HTTP onto it. Both default to
on, which is what serving TLS alone used to do.

```yaml
domains:
  - host: example.com
    acme_email: certs@acme.test
tiles:
  web:
    domains:
      - host: legacy.example.com
        force_https: false   # serve TLS, still answer on http
```

Four more keys shape the route itself:

- `middlewares`: traefik middleware names, run after stackr's own auth and
  header ones. A bare name is this stack's `proxy.middlewares` entry,
  `stack/name` is another stack's in the same org.
- `priority`: the traefik router priority.
- `rule`: a raw traefik rule. It replaces the generated `Host()` and
  `PathPrefix()` match only, so `host:` is still required: it picks the
  certificate. A plain entry and a rule entry can share a host.
- `port`: the container port for this entry. Blank uses the tile's.

```yaml
domains:
  - host: oc.example.com
  - host: oc.example.com
    rule: Host(`oc.example.com`) && Path(`/.well-known/openid-configuration`)
    priority: 100
    middlewares: [oidc-discovery, auth/authelia]
```

## Proxy middlewares

`proxy.middlewares` holds raw traefik middleware bodies. stackr writes them to
one dynamic file per stack, under names only it uses, so two stacks never
collide. A plan refuses a domain naming a middleware that does not exist, and
refuses to drop one a domain anywhere in the org still names: traefik would
switch those routers off without a word.

```yaml
proxy:
  middlewares:
    authelia:
      forwardAuth:
        address: http://authelia:9091/api/authz/forward-auth
        trustForwardHeader: true
        authResponseHeaders: [Remote-User, Remote-Groups, Remote-Email]
```

## Org shares

The org file's `storage:` declares network shares, smb or nfs. There is no
`local` backend: an org never touches a host's disk. Credentials come from org
values by reference. Any tile in the org mounts any sub-path of a share, or
its root, with `${{ org.storage.NAME }}`. Nothing is declared up front.

```yaml
# stackr-org.yml
secrets:
  MEDIA_SMB_USER:
  MEDIA_SMB_PASSWORD:
storage:
  media:
    backend: smb                 # smb | nfs
    address: nas.lan
    export: media                # smb share name, or nfs exported directory
    username: ${{ org.secrets.MEDIA_SMB_USER }}
    password: ${{ org.secrets.MEDIA_SMB_PASSWORD }}
    path: /library               # optional root inside the export
    opts: vers=3.0               # optional raw mount options
```

Each distinct sub-path is its own docker volume, created on every ready node,
and one volume mounts into any number of containers at once. Managed databases
cannot mount a share: database files over nfs or smb corrupt. Changing a share
recreates its volumes, so stop the tiles that mount it first. A share a tile
still mounts cannot be dropped from the file. Outside config, `stackr storage
add --org <org>` adds one to an org with no config file.

## Files

A `files:` entry naming a folder ships the whole tree, and `:template` on a
folder templates every file in it. Each deploy writes a fresh copy, so a file
deleted from the repo is gone from the next deploy. Only regular files are
copied; symlinks are skipped.

`volumes:` refuses a relative source like `./authelia:/config`. Under swarm it
used to mount an empty directory; ship repo files with `files:` instead.

## Renames: `moved:`

Map keys are identity to the differ, so renaming one plans a delete and a
create, which for a tile with a volume destroys data. A `moved:` entry says the
two keys are one thing:

```yaml
moved:
  - from: tile.site
    to: tile.web
```

Kinds are `tile.` and `env.` in a stack file, `stack.` and `shared.` in the org
file. The file's own scope is the namespace: there are no `org:stack:` prefixes,
so a stack file can never declare a move somewhere else.

- A tile move applies in every environment in the plan's scope that holds the
  slug. A plan bound to one environment moves it there and nowhere else.
- The target must be declared in the file. A move to something the file does
  not declare is a rename and then a delete; say which you mean.
- One hop per entry, no chains, no duplicate `from` or `to`, and the two names
  must differ. All load errors.
- Once applied the entry is inert and warns that it can be removed. Both sides
  live at once is a plan error: the marker cannot say which row is which.
- The row keeps its id, so volumes, variables, history and slices ride along.
  Containers and routes are rebuilt under the new name: a service cannot be
  renamed in place, so a running tile is stopped and redeployed.
- A move needs a person. Like a stack rename, it is refused on the unattended
  webhook path and waits for an approval.

Renames are never inferred. A vanished key plus an appeared key is exactly the
shape of two unrelated edits in one commit.

## Defaults

`defaults:` is one rung of the cascade (server → organization → stack →
environment → the tile's own setting). Same keys at every level:

```yaml
defaults:
  cron_timeout_min: 60
  cpu_limit: 2
  mem_limit_mb: 1024
  run_retention_days: 14
  metric_retention_hours: 48
  protect_auto_domains: true
  node_group: build
environments:
  staging:
    defaults:
      mem_limit_mb: 512
```

The org file's `defaults:` takes the same keys (alongside `ui_edits` and
`env_colors`). A level that declares nothing leaves whatever the panel set
alone; a level that declares something owns it.

`build_node` is not a config key: it is instance-wide and read only at the
server level.

## Backups

One `backup:` per tile:

```yaml
tiles:
  orders-db:
    type: managed
    engine: postgres
    backup:
      dest: ${{ org.backups.prod-bucket }}   # or ${{ stackr.backups.NAME }}
      schedule: "0 3 * * *"
      tz: Europe/Berlin        # optional, the server's zone when absent
      keep: 14                 # 0 keeps every archive
      mode: pause              # volume tiles only: pause | stop | live
```

`dest:` is a reference and never a literal: a destination carries the bucket
credentials and the file is in git. `${{ stackr.backups.NAME }}` reaches a
server-wide destination only once an admin has shared it.

`kind` is derived: a managed database is dumped with its engine's own tool, a
tile with a volume gets a tar of the volume. `mode:` on a dump is a plan error
rather than a key that silently does nothing.

The file owns the schedule. Dropping the block removes it (archives already in
the bucket are kept); `enabled: false` keeps it declared and stops it firing.

## Declared secrets

`secrets:` is a map of the values a stack needs but does not carry. Names and
options only — the file is in git, a value there is a leak.

- A **bare name** declares it: set out of band (`stackr vars set NAME=… --secret`
  or the panel), standing plan warning while unset.
- `default: generated` mints a value on the first apply that finds it unset,
  then never touches it again — changing `length:` later never rotates an
  existing value. Rotation stays a deliberate CLI/panel act.
- `required: true` turns the warning into a refusal: apply stops while unset.
- `env_versions` (default true) values the secret per environment — staging
  and production never share a session key. `false` = one stack-wide value;
  env overrides are then refused loudly.

An environment may add bare names on top via its own `secrets:` list; options
live at stack level only.

## Slices — `from:`

A tile whose body has `from:` is a slice: a logical database or bucket cut
from a managed instance. The config key is the reference slug —
`${{ tile.web-db.DATABASE_URL }}` — and defaults to the db/bucket name
(`name:` overrides). Apply resolves the dotted address, attaches if a slice of
that name already exists on the instance, provisions if not.

**Removal keeps your data unless you say otherwise.** An entry that leaves the
file detaches; the data stays, reclaimable from the instance's panel.
`on_remove: drop` opts into destruction — set it in one commit, remove the
entry in a later one (the policy is stored on the provision row while the
entry still exists). Drops are destructive in the plan, so the auto path
refuses them and a human approves. PR envs are the exception: their slices
drop on teardown regardless.

## CLI / API

| | route | scope |
|---|---|---|
| list plans | `GET /api/v1/stacks/:id/config/plans` | `config:read` |
| read one, with its diff | `GET /api/v1/config/plans/:id` | `config:read` |
| re-plan now | `POST /api/v1/stacks/:id/config/plan` | `config:apply` |
| approve (applies) | `POST /api/v1/config/plans/:id/approve` | `config:apply` |
| reject | `POST /api/v1/config/plans/:id/reject` | `config:apply` |
| promote (panel only for now) | `POST /projects/:id/envs/:slug/promote` | session |

`config:apply` is its own capability, and the decision routes also require a
live write role in the stack's org — a read-only member holding the key cannot
approve. Approval is not a bypass of `apply_policy: manual`; it is what that
policy waits for.

```
stackr plan                       # re-plan the linked stack, print the diff
stackr plan --json                # machine-readable — the CI shape
stackr plan list [--limit 20]
stackr plan show|approve|reject <plan-id>
```

A plan's `destructive` flag is what a pipeline gates on. Two shapes worth
knowing: re-planning returns a **list** (the stack branch plus each env pinned
to its own config branch), stack-scoped plan first; and reading plans needs no
live binding — unbinding a repo keeps the history readable.

The org-level counterparts (`stackr org plan|plans|approve|reject`,
`/orgs/:id/config/...`) mirror these; org plans are always applied by hand.

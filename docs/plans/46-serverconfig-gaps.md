# Plan: serverconfig gaps

Status: agreed 2026-09-16, built 2026-09-16, verified on the rig 2026-09-17 (single node).

Working rules: discuss first, one point at a time, no code without a go, no
git writes, no edits to `*_templ.go` or `output.css`, terse UI copy.

## Where this came from

Reading `~/Projects/serverconfig` (ten compose stacks on one box: auth,
dashy, huginn, immich, media, monitoring, mx5parts, owncloud, portainer,
traefik, stackr) against what `stackr-org.yml` and `stackr-compose.yml` can
say today. Most of it maps: stacks, vars, secrets, image tiles, absolute bind
mounts, healthchecks, `depends_on: db:healthy`, cron and function tiles,
build-from-repo. The old traefik and stackr stacks disappear, stackr owns
both. `/deploy` tag bumps become `update_policy: auto`.

Four things do not map. This plan closes them.

## Decisions (2026-09-16)

- **Org network shares.** `storage:` in `stackr-org.yml`, smb or nfs only.
  No `local` backend at org level: an org never touches the host filesystem.
  Credentials come from org secrets by varref. Tiles mount sub-paths with a
  new `${{ org.storage.NAME }}` reference; any sub-path under the share is
  allowed, nothing is pre-declared. Only stacks in that org resolve it.
  Managed databases on a share stay refused (`storagetiles.ValidateAttach`),
  database files over nfs or smb corrupt. Immich photos go on a share, its
  postgres does not.
- **Raw absolute `volumes:` lines stay** for service tiles:
  `/var/run/docker.sock`, `/proc`, `/sys`. Monitoring needs them.
- **Domain entries grow four keys:** `middlewares` (names appended after
  stackr's own auth and header ones), `priority`, `rule` (raw traefik rule,
  replaces the generated `Host()`/`PathPrefix()` as the matcher only), `port`
  (container port for this entry, 0 inherits the tile's). The domain row
  already has `container_port` and `WriteApp` already reads it per domain.
  `rule` still needs `host`: the cert resolver, wildcard handling and the
  unique index key on it. The index `idx_domains_host_path` becomes
  (host, path, rule) so a plain entry and a rule entry share a host.
- **Stack-level `proxy.middlewares`:** a map of raw traefik middleware bodies
  in `stackr-compose.yml`. Stackr writes them as one dynamic file per stack,
  named `stk-<stack>-<name>` so stacks do not collide. A tile references its
  own stack's by bare name and another stack's in the same org by
  `stack/name`. Forward-auth to `http://authelia:9091` works because traefik
  sits on every stack network and tiles answer on their slug. Apply refuses
  to drop a middleware while a domain row in the org still names it; traefik
  would otherwise disable those routers without a word.
- **`files:` takes folders.** A repo path that is a directory ships the whole
  tree to the container path; `:template` on a folder templates every file in
  it. Each deploy writes to `<data>/files/<tile-id>/<deploy-id>/`, the old
  container keeps its folder until it stops, and every folder except the
  current one is removed once the service has converged and only on a
  successful deploy. Pruning earlier would pull files out from under the old
  task, a bind mount sees the host directory live. No change detection:
  deploys only happen on push or apply, and the wipe is what removes files
  deleted from the repo.
- **Relative bind lines are refused.** `./authelia:/config` under `volumes:`
  becomes an error pointing at `files:`. The guard lives in
  `runtime.parseMounts` so config and panel tiles hit the same one. Under
  Swarm it mounts an empty directory today with no error.
- **Dropped:** `cap_add` (cadvisor is dropped from serverconfig, stackr
  collects per-tile CPU and memory itself), var refs in `image:` (the image
  watcher replaces tag-bump deploys), managed mariadb/mongo/redis (plain
  service tiles with volumes cover owncloud and the valkey/redis sidecars).
- **Deferred to server config-as-code:** raw traefik dynamic entries, TCP
  passthrough to other machines, LAN reverse proxies (pihole, proxmox),
  traefik prometheus metrics. Those are per server, not per org.
- **README fix:** managed engines are postgres and s3
  (`internal/stackrd/infra/managedtiles/managedtiles.go`), not the five the
  README lists.

## The file, after

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
    path: /library              # optional root inside the export
    opts: vers=3.0               # optional raw mount options

# stackr-compose.yml, stack "auth"
proxy:
  middlewares:
    authelia:
      forwardAuth:
        address: http://authelia:9091/api/authz/forward-auth
        trustForwardHeader: true
        authResponseHeaders: [Remote-User, Remote-Groups, Remote-Email]

# stackr-compose.yml, stack "media"
base:
  tiles:
    sonarr:
      image: lscr.io/linuxserver/sonarr:latest
      port: 8989
      storage:
        - ${{ org.storage.media }}/tv_shows:/tv
        - ${{ org.storage.media }}:/media
      domains:
        - host: sonarr.vulpe.dev
          middlewares: [auth/authelia]
        - host: sonarr.vulpe.dev
          path: /api/
          priority: 10

# stackr-compose.yml, stack "owncloud"
proxy:
  middlewares:
    oidc-discovery:
      replacePath:
        path: /index.php/apps/openidconnect/config
base:
  tiles:
    owncloud:
      files:
        - config:/etc/owncloud            # a folder
      domains:
        - host: oc.vulpe.dev
        - host: oc.vulpe.dev
          rule: Host(`oc.vulpe.dev`) && Path(`/.well-known/openid-configuration`)
          priority: 100
          middlewares: [oidc-discovery]
```

## Under the hood

**Shares.** Stackr never mounts on the host. Each distinct sub-path string
becomes a docker `local`-driver volume with the nfs or cifs options baked in
(`storagetiles.VolumeOpts`); the kernel mounts it when the first container
using it starts and unmounts after the last stops. The definition is created
on every ready node, so a task can land anywhere. One volume mounts into any
number of containers at once. The smb password sits in the volume options,
readable by anyone with the docker socket, same as today. Volume definitions
for sub-paths nobody uses any more are left behind; they hold no data and cost
nothing.

The `${{ org.storage.NAME }}` reference is expanded before the line is
parsed, in `storagetiles.Resolve` only; today it reads the raw line.
`placement.StorageNodes` stays as is: it only cares about local pools, org
shares are never local, and it already skips lines it cannot parse. A line
with no sub-path (`${{ org.storage.media }}:/media`) mounts the share root,
which `ParseAttachment` refuses today and must accept for org shares.

Volume names: server pools use `repo.StorageVolume(pathID)`, which needs a
declared path row. Org shares have none, so the name is
`stackr-stor-<share id, 8 chars>-<sha256 of the sub-path, 8 hex chars>`,
root sub-path included. Same sub-path string, same volume, on every node.

**Files.** At deploy the repo is already checked out under
`<data>/repos/<tile-id>`. `materializeFiles` walks a folder entry, copies each
file into the per-deploy folder, templates the flagged ones, and returns one
bind line for the folder. Known and unchanged: the copy lives on the manager's
disk, so a tile placed on a worker bind-mounts a path that is not there. True
for single files today. Fix later with docker configs or by pinning.

## Steps

1. **Org shares.**
   - `storage` table: `server_id` goes nullable (it is NOT NULL DEFAULT
     'local' today) and `org_id` is added, nullable; exactly one of the two
     is set. `internal/stackrd/store/db/migrations/001_initial.up.sql` via
     `dumpschema`, `repo.Storage`, `sqlite/storage.go`.
   - `orgconf.File` gains `Storage map[string]StorageConf`; parse rejects
     `backend: local`; `diff`/`Apply` gain create, update (recreate volumes,
     `storagetiles.Recreate`, refused while consumers hold it), delete. Apply
     resolves the credential varrefs and stores the password encrypted, same
     as the CLI add path.
   - `varref`: new bucket `storage` under `org.`; resolves to the share slug
     and refuses when the tile's org differs.
   - `storagetiles.Resolve` expands the line before parsing.
     `placement.ParseAttachment`: accept slug plus a free sub-path, or slug
     alone for the share root. `Resolve`: for org shares skip the
     declared-path lookup, name the volume from share id plus sub-path hash
     (see Under the hood), ensure it on every ready node.
   - CLI `stackr storage` and API `storage.go`: `--org` on add, org column on
     ls. Panel: org shares listed on the org page, read-only with the config
     badge.
2. **Stack middlewares.**
   - `stackconf.File` gains `Proxy struct{ Middlewares map[string]RawMap }`;
     plan diffs the rendered YAML per name; apply stores it on the stack row
     and refuses to drop a name a domain row in the org still references.
   - `proxy`: `WriteStackMiddlewares(stack)` writes
     `dynamic/stack-<id>.yml`, `Resync` regenerates, delete removes.
3. **Domains.**
   - `stackconf.DomainConf` + `repo.Domain` + schema: `middlewares`,
     `priority`, `rule`, `port`; `idx_domains_host_path` becomes
     (host, path, rule). `rule` without `host` is a load error.
   - `plan.go diffDomains` and `apply.go syncDomains` diff and write them;
     `port` writes `container_port` instead of copying the tile's.
   - `proxy.WriteApp`: `rule` replaces `hostRule(d)` as the matcher only,
     `d.Host` still drives the cert resolver; `priority` line; `middlewares`
     appended to `routerMws`, `stack/name` resolved to
     `stk-<stack>-<name>@file`.
   - Panel domain form in `handlers/web/handler/app`: four fields.
4. **Files.**
   - `runtime.ParseFileMount` unchanged; `materializeFiles` stats the source,
     walks a directory, writes under `<tile-id>/<deploy-id>/`; the engine
     prunes sibling folders after the converge wait passes, never on a failed
     deploy.
   - `runtime.parseMounts`: a source starting with `./` or `../` is an error
     naming `files:`.
5. **Docs.** `config-as-code.md` (storage, proxy, domain keys, folder files),
   README engine list, `notes.md` entry for files on workers.

## Verification

- Unit: orgconf parse and diff for `storage:`; stackconf parse for the four
  domain keys and `proxy.middlewares`; `materializeFiles` folder walk and
  prune; relative bind rejection.
- Rig: an smb share from the NAS mounted by two tiles at once, a forward-auth
  middleware from one stack used by another, an owncloud-style exact-path
  router, a folder of grafana dashboards shipped and updated on push.

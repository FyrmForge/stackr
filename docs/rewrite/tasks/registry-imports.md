# Registry imports: every node pulls only from the cluster registry

Status: plan agreed point by point with darthvader 2026-10-13. Not built.

## Agreed

- Every image a node runs comes from the cluster registry over the tunnel.
  Nodes never pull from Docker Hub, ghcr or any outside registry.
- The registry stays the stock `registry:3` behind regfront (the panel's
  token front). No Zot, no pull-through cache: both keep one fixed upstream
  login per source, so they cannot use an org's own credentials and would
  serve one org's private image to every other org.
- **Import step:** when a deploy needs an outside image, the controller
  copies it into the registry under that org's folder, with the org's own
  saved credential. Credentials never leave the controller.
- **Only the CPU types the cluster has** (Docker's `Architecture` of every
  node, `x86_64` -> `amd64`, `aarch64` -> `arm64`). The image index is copied
  unchanged, so the digest stays the upstream one (update checks keep
  working), with only those platforms' manifests and layers behind it. A node
  of a new CPU type gets its platform imported the first time it needs it.
  ARM is untested until we have an ARM box (Hetzner CAX rents by the hour).
- Copy library: `github.com/google/go-containerregistry` (Apache 2.0, under
  crane and ko). New dependency. `crane copy --platform` drops the index (the
  digest changes), so the filtered copy is our own small function on top of
  `remote`.
- **Builds:** a builder's `FROM` base images still come straight from the
  internet. The build result goes to the registry as today.
- **stackr's own images** (the stackr image that runs ingress Caddy on
  workers, `alpine:3` for volume copies, the pause image, the rsync helper)
  live in `stackr.system/...`. The controller pushes its own image there at
  boot and after every upgrade. Ends the manual `docker save | docker load`
  to h4. Exception: join, before any tunnel exists, downloads from the public
  release as today.
- **Record:** every import and every build push writes a row: org, repo,
  tag, digest, platforms, source (built / imported from `<upstream ref>`),
  who, when, size.
- **Cleanup, nightly:** delete every version no env uses that is not among
  the last 3 per repo; then the registry's garbage collect frees the disk.
  Pushes are refused (503, retried) for the seconds the sweep runs. A
  setting holds the schedule and the keep count.
- **UI:** the org Registry tab gets a drill-down (versions: tag, size,
  platforms, arrived, built or imported from where, used by which envs) and
  delete (warns and names the envs when in use). An admin view lists all
  orgs, total disk, the last cleanup's result, "Clean up now".
- **Digest-pinned refs we already hold: no outside call.** For
  `name@sha256:...`, when the registry already holds that digest under the
  import repo with every wanted platform, `Import` contacts the upstream not
  at all (content behind a digest never changes). A missing platform is
  fetched from upstream as before.
- **Tag refs: fall back to our copy when upstream is unreachable.** If
  resolving the tag fails because the upstream cannot be reached or answers
  5xx/429 (network, timeout, DNS, rate limit) and the registry holds that tag
  with every wanted platform, the deploy uses the copy and writes a warning to
  the deploy log ("could not reach <host> (<reason>); using the copy from
  <age> ago, it may not be the newest"; age from `registry_images.created_at`,
  else "an earlier import"). 401/403/404 still fail the deploy; no local copy
  fails as before. `imgcopy.UnreachableError` tells the leaf; the deploy's
  `Import` hook gets the deploy log.
- **Later (not this plan):** vulnerability scans and allow/deny lists,
  hooked into the import step.

## Points agreed before Wave 0 (all closed)

1. ~~Index validation~~ agreed: the registry's own store-time check is
   `platforms: none`. This is NOT what gets copied: the importer still copies
   only the CPU types the cluster's nodes have, and guarantees them (a fixed `list` would need the registry recreated
   as CPU types come and go). A hand-pushed broken index fails on the node.
2. ~~Repo name~~ agreed: the original name with the org in front, nothing
   invented: `postgres:16` -> `<org>/docker.io/library/postgres:16`,
   `ghcr.io/foo/bar:1` -> `<org>/ghcr.io/foo/bar:1`. Upstream names are unique;
   the second part always holds a dot (a host) and stack slugs never do.
3. ~~Record table~~ agreed: `registry_images`, owned by one leaf,
   `internal/service/internal/leaf/registry`. Every registry operation (import,
   delete, cleanup, list) goes through it and updates the table in the same
   step; regfront calls it when a push (builder or developer) completes.
   Nothing else in stackr talks to the registry. Listing: the registry decides
   what exists, the table adds detail; a row with no image is not shown.
4. ~~Per-node VIP lock~~ agreed, as G6.

## Decisions taken without asking

- The controller pulls through its own loopback registry like every node,
  so the record and the cleanup see everything.
- Containers keep the image name the user gave (`postgres:16`): the node
  pulls the registry copy and tags it with the original name, as
  `DockerRouter.Pull` already does for built images.

## Rules for every worker

No git write ops (no stash either). Never hand-edit generated files
(`*_templ.go`, `ui/static/js/elements/*`, `output.css`,
`docs/openapi.json`, `staticmanifest.go`); regenerate them. Touch only the
files you own; a need elsewhere goes into `registry-imports-seams.md`. No em
dashes. Match neighbouring style; `ponytail:` for ceilings. Never print
secrets (credentials, tokens, keys) in output or logs. **Test first.** Finish
with `go build ./... && go vet ./...`, the tests of every package touched,
and `golangci-lint run` on them.

## Wave 0: contract (one worker)

1. `go get github.com/google/go-containerregistry` (pin the version).
2. Migration `022_registry_images`: table `registry_images` (id, org_id,
   repo, tag, digest, platforms text, source `built|imported|system`,
   upstream_ref, size, created_by, created_at; unique repo+digest). Store
   type, CRUD, `ListByOrg`, `ListAll`, `InUse` hook left to the service.
3. `internal/service/internal/regfront`: accept import repos
   `<org>/<host>/<path...>` of any depth (Docker's repo grammar), splitting a
   request path into repo and endpoint from its end (`/manifests/<ref>`,
   `/blobs/<digest>`, `/blobs/uploads/...`, `/tags/list`,
   `/referrers/<digest>`), as the registry itself does. Users and admins: the per-org rules of
   tile repos. **Nodes:** a node may pull an import only when an env pinned to
   it uses that upstream ref (a new Facts entry), never by org alone, since
   one node hosts many orgs. Name helper `ImportRepo(org, upstreamRef)
   string` (Docker Hub short names normalised to `docker.io/library/...`)
   plus tests, including a node token of org A asking for org B's
   import.
4. `internal/installspec/registry.go`: mount a config file with
   `validation.manifests.indexes.platforms: none` and delete enabled; spec
   hash changes so upgrade recreates the container.
5. `internal/service/internal/imgcopy`: the `Copy` signature (G1) as a stub,
   so G2 and G3 compile while G1 writes it.
5b. `internal/service/internal/leaf/registry`: the leaf's API as stubs
   (`Import`, `Pushed`, `Delete`, `List`, `Versions`, `Clean`), the only
   holder of the registry address and the store; move the listing code of
   `internal/service/registrylist.go` into it. regfront gets a `Pushed`
   callback field.
6. Interfaces and stubs, in a new `internal/service/imports.go`:
   `Import(ctx, orgID, ref, auth string, platforms []string) (localRef,
   digest string, err error)` returning `errors.ErrUnsupported`;
   `clusterPlatforms(ctx) []string`; `recordImage(...)`.
7. Hand-off section in `registry-imports-seams.md`.

## Wave 1: in parallel

### G1 Filtered copy
Owns a new package `internal/service/internal/imgcopy`.
- `Copy(ctx, src, dst string, srcAuth, dstAuth, platforms []string)`:
  single manifest: copy if its platform is wanted, else error naming the
  platforms it has; index: copy the wanted children (layers, config,
  manifest), then put the index bytes unchanged. Missing platform upstream =
  clear error.
- Already-present blobs are skipped (a re-import of a new platform copies
  only that platform).
- First a HEAD on the upstream tag (Docker Hub does not count HEADs against
  its pull limit): when that digest and the wanted platforms are already
  stored, nothing is fetched. Every pull now leaves from the controller's one
  address, so this matters.
- Every import lands under a tag: the upstream tag, or `sha256-<hex>` for a
  digest-pinned ref, so the nightly sweep's `--delete-untagged` never takes a
  running image.
- Tests with go-containerregistry's in-memory registry: multi-arch index,
  only amd64 lands, the digest equals upstream, a second run adds arm64
  only; single-arch image; private upstream with basic auth.

### G2 Deploy routing
Owns `internal/service/imports.go` (fills the stubs),
`internal/service/dockerrouter.go`, `internal/service/move.go`,
`internal/service/internal/flow/deploy/deploy.go` (the image step only).
- Before `Images.Ensure`, an outside ref goes through `Import` on the
  controller with the org's credential; the node then pulls the local copy
  and tags it with the original ref.
- `clusterPlatforms` from each node's Docker info, cached per minute.
- A node of a new CPU type: `Import` adds the missing platform first.
- Moves and live moves import before the target pulls.
- Update checks still ask upstream (unchanged); a new digest means a new
  import on the next deploy.
- Records every import and every build push.
- Check `docker.LocalDigest` / `pickDigest` on a retagged import: its
  RepoDigests name the `localhost:5000/...` repo, so the fallback must return
  the upstream index digest, never "" (a false "update available").

### G3 System images
Owns `internal/service/ingressnode.go`, `internal/service/internal/docker/volumes.go`
(tool image name), the pause image constant, the upgrade flow's post-upgrade
hook, `internal/service/internal/flow/move/move.go` (rsync helper ref).
- At boot and after an upgrade, the controller copies `alpine:3` and the
  pause image into `stackr.system/` (platforms as above). Its own image is not
  on any outside registry yet, only in the controller's Docker: push that one
  from the local Docker (one CPU type, `ponytail:` until ghcr carries a
  multi-arch image).
- Ingress Caddy on workers, volume copies and pause containers use those
  refs. `scripts/rig.sh` drops the h4 `docker save | load` step.

### G4 Cleanup
Owns a new `internal/service/registryclean.go`, the schedule entry, the
settings `registry_cleanup_schedule` (default `15 4 * * *`) and
`registry_keep` (default 3), and the push gate in regfront.
- Mark: per repo, delete manifests not used by any env (current release,
  pinned releases and rollback targets count as used) beyond the newest
  `registry_keep`; delete their records. Deleting is by digest, which drops
  every tag on it: only when no tag on that digest is in use.
- First test, against the real `registry:3` image: garbage collect with
  `--delete-untagged` keeps everything behind a tagged index, including an
  index whose other platforms are absent.
- Sweep: gate pushes (503 with Retry-After), `docker exec stackr-registry
  registry garbage-collect --delete-untagged <config>`, ungate. A timeout
  always ungates. Imports write to the loopback registry, not through
  regfront, so `Import` waits on the same gate.
- Result (freed bytes, deleted versions, errors) kept for the admin view and
  written to the security log.

### G5 UI and API
Owns `internal/ui/drawer/org/registry.templ`, the admin registry view (new
tab in the admin drawer), `internal/web/handler/canvas/registry.go`,
`internal/service/registrylist.go`, the API routes and CLI commands
(`stackr registry ls [repo]`, `stackr registry rm <repo>:<tag>`,
`stackr admin registry clean`).
- Org tab: row click opens versions; delete with an in-use warning that
  names the envs; admins see every org's rows.
- Admin tab: total disk, per-org totals, last cleanup result, "Clean up now".

### G6 Per-node VIP lock
Owns `internal/service/vipboot.go`.
- Replace the one `vipMu` with a lock per node, so a slow or unreachable
  node no longer holds up every other node's minute refresh. A revoke still
  cannot be overwritten by a rebuild that read the grants before it (per
  node).

## Wave 2: integrate and prove on the rig

- Merge on the integration worktree; `make test && make lint`.
- Release `v0.6.0-dev.N`, upgrade 109, swap the worker binaries.
- Proofs:
  1. Block Docker Hub and ghcr on ct110 (`registry-1.docker.io`,
     `production.cloudflare.docker.com`, `ghcr.io` to 127.0.0.1 in
     `/etc/hosts`; their CDN addresses change too often for iptables); deploy `postgres:16` and a private ghcr image there; both
     run, `docker images` on ct110 shows them under the original names.
  2. The admin view lists both with the right org, source and platforms
     (amd64 only).
  3. A second org cannot pull the first org's import (regfront 403), neither
     with a user token nor with the node token of a node running only the
     second org's envs.
  4. h4's ingress Caddy comes from `stackr.system/` after an upgrade with no
     manual copy.
  5. "Clean up now" frees an unused version; a deploy during the sweep
     retries and succeeds.
  6. Stop ct110's tunnel: other nodes' VIP refresh keeps its one-minute pace.

# Registry imports: seams (Wave 0 hand-off)

Written by the Wave 0 worker. Read with `registry-imports.md`. Everything
below exists and compiles; a stub returns `errors.ErrUnsupported`. A worker
fills only the files it owns.

## Decisions taken in Wave 0

- **Registry config by env, not a mounted file.** `installspec.Registry()`
  gets `REGISTRY_VALIDATION_MANIFESTS_INDEXES_PLATFORMS=none`. Neither create
  path (`cmd/stackr-install/registry.go`, `flow/upgrade/registry.go`) places
  files, and the image's own `/etc/distribution/config.yml` keeps storage
  `/var/lib/registry`, `:5000`, delete enabled. Verified on `registry:3`: an
  index naming an absent child is refused (400) without the variable and
  accepted (201) with it. The spec hash changes, so upgrade recreates the
  container. G4: `docker exec` inherits the container env, so
  `registry garbage-collect --delete-untagged /etc/distribution/config.yml`
  sees it.
- **Refusals at the front are 401 `insufficient_scope`**, Docker's protocol
  answer for a token without the scope, not 403. This is the existing
  behaviour for tile repos; imports follow it. Proof 3 reads "refused".
- **Host with a port** in an upstream ref (`registry.corp:5443/x`) becomes
  `registry.corp_5443` in the repo name (a colon cannot be in a repo name).
- `nodeFacts` (`internal/service/registry.go`) does **not** fill
  `PullImports` yet. See G2.
- `go get` also moved `docker/go-connections` v0.6.0 to v0.7.0 and
  `klauspost/compress` to v1.19.2 (go-containerregistry needs them).
  go-containerregistry is pinned at v0.22.1.

## Store

`internal/db/migrations/022_registry_images.{up,down}.sql`, table
`registry_images` (id, org_id NULL for system, repo, tag, digest, platforms
comma separated, source `built|imported|system`, upstream_ref, size,
created_by, created_at; unique repo+digest; org delete cascades).

`internal/service/internal/store/registry_images.go`:
`store.RegistryImage`, `store.RegistryImageStore` as `Tables.RegistryImages`
(`Create`, `Get`, `GetByRepoDigest(repo, digest)`, `ListByOrg`, `ListByRepo`,
`ListAll` (all newest first), `Update`, `Delete`, `DeleteByRepoDigest` (no
error when absent)). The InUse question is not in the store.

## regfront (`internal/service/internal/regfront/regfront.go`)

- `ImportRepo(org, upstreamRef string) string` and
  `ImportName(org, upstreamRef string) (repo, tag string)`: tag is the
  upstream tag, `latest` when none, `sha256-<hex>` for a digest pinned ref.
  Docker Hub short names become `docker.io/library/...`.
  `ParseImportRepo(repo) (org, upstream string, ok bool)`.
- Paths split from the end (`splitPath`); import repos of any depth.
- `Facts.PullImports []string`: the import repos (`ImportRepo` values) a node
  may pull. **Users/admins** follow the tile-repo org rules; **nodes** pull an
  import only when it is in `PullImports` (`PullRepos` and `PushRepos` do not
  reach imports); `stackr.system/` stays pull-only for every caller.
- `Server.Pushed func(context.Context, regfront.Pushed)`, called after a
  manifest PUT answered 2xx. `regfront.Pushed{Repo, Reference, Digest, Kind,
  Subject}`. Wired in `service/registry.go` to `Orchestrator.registryPushed`.
- Push gate (G4, done): `regfront.Gate` (`Close() (open func(), ok bool)`,
  `Closed()`, `Wait(ctx)`) and `Server.Gate *Gate`. While shut, every non-GET/HEAD
  under `/v2/` answers 503 + `Retry-After: 10`; reads pass. Wired in
  `service/registry.go` as `Gate: o.regs.Gate()` (one line, next to `Pushed:`).

## imgcopy (`internal/service/internal/imgcopy/imgcopy.go`), G1

```go
func Copy(ctx context.Context, src, dst string, srcAuth, dstAuth authn.Authenticator, platforms []string) (Result, error)
type Result struct { Digest string; Platforms []string; Size int64 }
```
`authn` is go-containerregistry's; nil is anonymous. The contract is in the
doc comment (filtered platforms, index bytes unchanged, tag always set,
HEAD-first skip, blobs skipped). `Result.Platforms` includes platforms an
earlier copy stored (the leaf records the union).

## Registry leaf (`internal/service/internal/leaf/registry/`)

The only holder of the registry address (`New(store.RegistryImageStore)`,
`WithBase(url)` for tests, `Local(repo, tag)` = `localhost:5000/repo:tag`) and
of `Tables.RegistryImages`. One file per operation so workers do not collide.

| File | API | Filled by |
|---|---|---|
| `registry.go` | `Leaf`, `New`, `WithBase`, `Local`, `Record(ctx, store.RegistryImage)` (done: upsert by repo+digest, a repeat updates platforms and size only), `JoinPlatforms`, `SplitPlatforms`, `SourceBuilt/Imported/System` | done |
| `list.go` | `List(ctx, orgSlug) ([]Repository, error)` (moved from `service/registrylist.go`, `""` = every org). Note: `built()` reads image manifests, so an imported *index* shows no "latest"; read the rows instead | G5 |
| `import.go` | `Import(ctx, ImportRequest{OrgID, OrgSlug, Ref, Auth authn.Authenticator, Platforms, By}) (Imported{LocalRef, Digest}, error)` | G2 (calls G1's Copy, then `Record`) |
| `push.go` | `Pushed(ctx, Push{OrgID, Repo, Reference, Digest, By}) error` | G2 |
| `versions.go` | `Versions(ctx, repo) ([]Version, error)`; `Version{Tag, Digest, Platforms, Size, CreatedAt, Source, UpstreamRef, CreatedBy, UsedBy}` (UsedBy filled by the service) | G5 |
| `delete.go` | `Delete(ctx, repo, digest) error` (manifest + row; no in-use check) | G5 |
| `clean.go` | `Clean(ctx, keep int, inUse InUse) (Cleaned, error)`; `InUse func(ctx, repo, digest) (bool, error)`; `Cleaned{Deleted, Freed, Errors}` | G4 |

A leaf never calls another leaf: facts come in as arguments (`inUse`, org id
and slug). Each filler adds its own `_test.go` file.

## Service (`internal/service`)

- `registrylist.go` (G5): `OrgRepositories` delegates to `o.regs.List`;
  `RegistryHost` unchanged. `Repository` now lives in the leaf package; the
  canvas handler only reads its fields.
- `orchestrator.go`: field `regs *registry.Leaf`, built as `leaf/registry`
  (added to `new_test.go`'s list).
- `imports.go` (G2 fills):
  - `(o) Import(ctx, orgID, ref, auth string, platforms []string) (localRef, digest string, err error)`:
    looks up the org, defaults `platforms` to `clusterPlatforms`, calls the
    leaf. `auth` (a credential id, `""` anonymous) is **not resolved yet**;
    G2 resolves it to an `authn.Authenticator` on the controller.
  - `(o) clusterPlatforms(ctx) []string`: stub, returns nil. G2 reads each
    node's Docker `Architecture`, cached per minute.
  - `(o) recordImage(ctx, store.RegistryImage) error`: delegates to
    `Leaf.Record`. Unused until G2 calls it; the linter may need a call site.
  - `(o) registryPushed(ctx, regfront.Pushed)`: resolves the org from the
    repo, calls `Leaf.Pushed`, logs any error but `ErrUnsupported`.
- `nodeFacts` in `registry.go` (G2): fill `Facts.PullImports` with
  `regfront.ImportRepo(orgSlug, ref)` for every outside ref of the tiles of
  envs pinned to the node (the same ref set the deploy imports). Without it a
  node cannot pull any import.

## Settings (G4, done)

`registry_cleanup_schedule` (cron, default `15 4 * * *`; **empty = off**: a
new `Knob.EmptyOff`, so `Leaf.Set` stores "" instead of deleting the row) and
`registry_keep` (int, default 5, 0 refused). Schedule entry `registry-clean`
(`Drivers.RegistryClean`, `offSpec`), UI labels in `settings_form.templ`.

## Cleanup (G4, done)

- `Leaf.Clean(ctx, keep, inUse)` is real: lists repos (paged catalog, n=1000;
  **the registry answers 400 to `n` above 1000**, so `list.go`'s
  `/v2/_catalog?n=10000` fails on a real `registry:3`: G5 must page or drop
  `n`, see `Leaf.catalog` in `clean.go` to reuse), per repo resolves each tag
  to its digest, orders digests newest first (row `created_at`, else the image
  config's `created`, else zero = oldest), skips the newest `keep`, asks
  `inUse(repo, digest)`, deletes the rest (DELETE by digest at the registry,
  then `DeleteByRepoDigest`). `inUse` is per digest, so a digest with any tag
  in use stays whole. `inUse` erroring protects the version and notes the error.
  `removeManifest` in `clean.go` does what `Delete` (G5) should do; G5 may
  switch it to `Delete` or keep both.
- `Leaf.Resolve(ctx, repo, tag) (digest, err)`: HEAD, "" when absent.
- **Gate for G2:** `Leaf.Wait(ctx) error` returns once the registry accepts
  writes (the sweep gates the whole mark + collect). `Leaf.Import` must call
  it before it writes (`if err := l.Wait(ctx); err != nil { return ... }`);
  also before `Pushed`-style direct writes. `Leaf.Gate()` is the
  `*regfront.Gate`; `Leaf.WhileGated(ctx, fn)` shuts it, runs fn, opens it
  whatever fn returns (`ErrSweeping` if already shut).
- Service (`registryclean.go`): job `kindRegistryClean` (lock `registry-clean`,
  controller job). **For G5:** `o.CleanRegistryNow(ctx, byUserID) (Job, error)`
  ("Clean up now", returns the queued job) and `o.LastRegistryClean()
  (RegistryClean, bool)` with `RegistryClean{At, Took, Manual, Deleted, Freed,
  Measured, Errors}` (in memory since the panel started; `Freed` is the registry
  volume's `du` before minus after, row sizes when `du` failed). Every sweep
  also writes a `registry.clean` security event (Detail = one readable line).
- In use = pins (digest and image row ref) of `keptReleases` (new, split out of
  `cleanupKeep` in `cleanup.go`: current, rollback depth, live-job and newest
  releases) plus every env tile's `ImageRef` resolved through its import repo.
  Digests match across repos (`ponytail:`). Every `stackr.system/` repo is
  in use (never cleaned), per G3.
- Real `registry:3` finding: `garbage-collect --delete-untagged` keeps
  everything behind a tagged index (also with the arm64 child absent, under
  `platforms: none`) and every tagged image; deleting one by digest then a
  collect frees exactly its manifest, config and layer blobs; re-pushing it
  works. Test: `leaf/registry/clean_test.go` (skips without docker).

## G4 needs

- G2: call `l.Wait(ctx)` at the top of `Leaf.Import` (see above), and give
  system/G3 pushes the same.
- G5: page `_catalog` (see above); the admin view reads `LastRegistryClean`,
  the button calls `CleanRegistryNow`. A "registry sweep" job shows in the job
  list under kind `registry-clean`.

- **G1** `imgcopy/imgcopy.go` (and its tests).
- **G2** `leaf/registry/import.go`, `push.go`; `service/imports.go`,
  `nodeFacts` in `service/registry.go`, dockerrouter, move, deploy image step.
- **G3** system images (`stackr.system/docker.io/library/alpine` style repos
  via `Leaf.Import` with a system org; `Record` takes a nil org).
- **G4** `leaf/registry/clean.go`, `service/registryclean.go`, settings,
  regfront push gate.
- **G5** `leaf/registry/list.go`, `versions.go`, `delete.go`,
  `service/registrylist.go`, UI, API, CLI.
- **G6** `service/vipboot.go`.

## G3 needs

- `service/systemimages.go` publishes `stackr.system/{alpine:3,pause:3.10,stackr:<ver>}` at boot (real host only, `runMesh != nil`) with `imgcopy.Copy` and `Leaf.Record` source `system`. It calls `o.clusterPlatforms` (falls back to the controller's GOARCH while that is empty) and `o.recordImage`.
- `tile.PauseImage` and `docker.ToolImage` are now `localhost:5000/stackr.system/...`. **G2:** `deploy.go` already pulls `tile.PauseImage` through `Images.Ensure`, which needs nothing more; just do not route a `localhost:5000/` ref through `Import`.
- **G2:** the rsync helper (`service/move.go` `rsyncSteps`) still builds and `docker push`es itself and is not in `registry_images`. To record it, call `o.recordImage` there (source `system`, repo `stackr.system/tools/rsync`).
- **G4:** the cleanup must treat `stackr.system/` rows as in use (all of them, or at least the running version's stackr tag and the `alpine`/`pause` repos).
- Ingress Caddy on a worker is `localhost:5000/stackr.system/stackr:<tag>` of the controller proxy's tag; the spec hash changes, so the first ensure after this lands recreates every worker proxy once.

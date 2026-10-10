# Migration blitz: six features in waves for Sonnet workers

Status: built, committed in ddaa87e (plan 2026-10-07). Decisions are in `routes.md` (the
"Migration list" section is the contract). The next rig build is
v0.6.0-dev.14.

Rules for every worker: no git write ops, ever. Never hand-edit generated
files (`*_templ.go`, `ui/static/js/elements/*`, `output.css`,
`docs/openapi.json`, `staticmanifest.go`); run the generator instead
(`templ generate`, `go run ./cmd/stackrd --dump-openapi > docs/openapi.json`).
Only touch the files you own; if you must change a file you do not own,
write the need into `docs/rewrite/tasks/blitz-seams.md` and stop that
item. Copy comment density and naming from the surrounding code; mark a
deliberate ceiling with a `ponytail:` comment. No em dashes anywhere. One
runnable test per non-trivial branch. Finish with `go build ./... && go
vet ./...` and the tests of the packages you touched.

Ownership below is exclusive per wave. A file named under one worker is
off limits to the others in that wave.

## Wave 0: contract (one worker, serial)

Lays down every shared type, hook and route row so wave 1 runs on disjoint
files. Compiles and passes `make test` on its own.

1. Migration `internal/db/migrations/004_blitz.{up,down}.sql`:
   - `routes` (id, host UNIQUE, mode CHECK IN (passthrough,http,https),
     target, insecure INTEGER, created_at).
   - `shares` (id, org_id → orgs CASCADE, slug, kind CHECK IN (nfs,smb),
     source, options, user, password_ref, created_at; UNIQUE(org_id, slug)).
     Credentials are `${{ org.params }}` refs, never values.
   - `host_grants` (id, stack_id → stacks CASCADE UNIQUE, lines TEXT,
     privileged INTEGER, approved_by → users, created_at). `lines` is the
     approved host mount lines, sorted, newline-joined.
2. Store rows and `XStore` interfaces for the three tables
   (`store/routes.go`, `store/shares.go`, `store/host_grants.go`), copying
   `store/domain_resources.go`; `Tables` fields and `bind()` lines in
   `store/store.go`; round-trips in `store/store_test.go`.
3. `internal/service/errs/errs.go`: `type NeedsApproval struct{ Stack, What
   string }` plus `IsNeedsApproval`. `flow/jobs/jobs.go` `call()`: a
   NeedsApproval parks like Unset with the text "waiting: <What>; a server
   admin approves it on the stack". Test in `flow/jobs`.
4. `leaf/tile/check.go`:
   - `parseMount` accepts `host:/abs/host/path:/abs[:ro]` (prefix `host:`)
     and `share:<slug>/<sub>:/abs[:ro]` beside `volume:/abs[:ro]`; exported
     `ParseMount(l) (Mount, error)` with `Kind volume|host|share`.
   - `parsePorts` accepts `host:container[/udp]`; a host port published
     twice with different protocols is fine (deploy fix in wave 1).
   - `ParseFileMount` unchanged (`repo/path:/abs[:template]`).
   - Tests for each.
5. `flow/deploy/spec.go` `publishedPorts`: key the Ports map by
   `host[/udp]` so tcp and udp on one host port both bind; `docker/
   containers.go` `portBindings` follows. Test with the fake.
6. Hooks (empty bodies, one file each, owned by wave 1 workers):
   - `flow/deploy/mount_share.go`: `func (f *Flow) shareBind(ctx, o, t,
     m tile.Mount) (string, error)` returning "not supported yet".
   - `flow/deploy/mount_host.go`: `func (f *Flow) hostBind(ctx, st, t, m)
     (string, error)` same.
   - `flow/deploy/mount_files.go`: `func (f *Flow) fileBinds(ctx, t, e, st,
     rr) ([]string, error)` same; `prepare()` calls it instead of the
     refusal at `deploy.go:229`; `plan.go:383` refusal removed.
   - `flow/deploy/deploy.go` `resolve()`: dispatch on `Mount.Kind` to the
     three hooks; `deploy.Flow` gains `DataDir string` (set in
     `orchestrator.go`).
   - `leaf/domain/caddy_auth.go`: `func authHandlers(e Extras, t TileRoute,
     expand Expand) []route` holding the basic-auth block moved out of
     `domainRoute`; `caddy_routes.go`: `type Route struct{Host, Mode,
     Target string; Insecure bool}` and `func externalRoutes(rs []Route,
     tlsOn bool) (plain, secure []placed, wrapper route)` returning nothing
     yet; `Build(in, tiles, routes []Route, expand)` calls it and, when the
     wrapper is non-nil, sets `listener_wrappers` on the https server
     before `{"wrapper":"tls"}`. Goldens unchanged.
   - `flow/promote/hostaccess.go`: `func (f *Flow) checkHostAccess(ctx, p
     *Plan, w *work) error` no-op, called at the end of `planConfig`.
   - `flow/promote/files.go`: `func (f *Flow) planFiles(...)` no-op.
7. `leaf/domain/domain.go` `Extras`: add `ForwardAuth *ForwardAuth
   {URL string; CopyHeaders []string}` (json `forward_auth`); `checkExtras`
   validates the URL is https or http with a host. `stackfile.go`
   `ProxyConf.ForwardAuth` and `plan.go specOf` map it. `api/handler/v1/
   tiles.go` carries it through `service.DomainExtras` unchanged.
8. Authz verbs in `internal/authz/authz.go`: `route.admin`, `share.read`,
   `share.write`, `hostgrant.approve`. API rows in `internal/api/routes.go`
   with stub handlers (501) in `v1/route.go`, `v1/share.go`,
   `v1/hostgrant.go`: `/admin/routes[/:route]` list,create,delete;
   `/orgs/:org/shares[/:share]` list,create,delete;
   `/stacks/:stack/host-grant` get, `POST .../approve`. CLI nouns
   pre-registered in `cmd/stackr/cmds.go` `commands()` as `a.routes()`,
   `a.shares()`, `a.hostGrant()` living in `cmd/stackr/route.go`,
   `share.go`, `hostgrant.go` (stubs that print "not built yet"), ops
   annotated so `TestEveryRouteHasAVerb` passes. Regenerate openapi.
9. Admin drawer `Tabs` gains `"routes"`; `tab()` case renders an empty
   `Routes` view in `ui/drawer/admin/admin.templ`. Org drawer `Tabs` gains
   `"shares"` likewise. `templ generate`.
10. `docs/rewrite/schema.md` rows for the three tables; `verbs.md` rows.
11. `make test && make lint` green. Hand-off: list every hook signature in
    `blitz-seams.md` so wave 1 copies them exactly.

## Wave 1: six workers in parallel

### W1 Routes (server admin, pass-through and external upstreams)
Owns: `cmd/stackrd/proxy.go`, `go.mod`, `go.sum`, `leaf/route/*` (new),
`leaf/domain/caddy_routes.go`, `leaf/domain/caddy_routes_test.go`,
`leaf/domain/testdata/routes_*.json`, `flow/deploy/proxy.go`,
`internal/service/route.go` (new), `internal/service/domain.go` (squat check
only), `api/handler/v1/route.go`, `internal/api/route_test.go`,
`cmd/stackr/route.go`, `ui/drawer/admin/admin.templ` (Routes tab only),
`web/handler/admin/handler.go` (routes section only), `installspec` nothing.
- Import `github.com/mholt/caddy-l4` v0.1.2 in `proxy.go`; `go mod tidy`.
- `leaf/route`: validate host (literal or `*.x`; wildcard only for
  passthrough unless DNS-01), target `host[:port]` (default 443 for
  passthrough, 80 http, 443 https), mode, insecure only with https; squat
  both ways against `domains.host` and `domain_resources.host` (the leaf
  reads only its table; the orchestrator passes the other hosts in, as
  `domainres.CheckOrgSquat` does). Create/List/Delete.
- `externalRoutes`: passthrough → wrapper `{"wrapper":"layer4","routes":
  [{"match":[{"tls":{"sni":[host]}}],"handle":[{"handler":"proxy",
  "upstreams":[{"dial":[target]}]}]}]}` (one route per host, one wrapper)
  plus a `:80` reverse_proxy route for the host to `target-host:80`;
  http/https → `:443` reverse_proxy (transport `http`, `tls` block with
  `insecure_skip_verify` when set) and a force-HTTPS route on `:80`; the
  https hosts join `httpsHosts` for certificates. TLS off: passthrough rows
  are refused at create ("pass-through needs HTTPS on").
- `ProxyConfig` loads routes and passes them to Build. `SyncProxy` after
  every create or delete.
- Service verbs `Routes`, `CreateRoute`, `DeleteRoute` (admin only, verb
  `route.admin`); API handlers replace the stubs; CLI `stackr admin route
  ls|add|rm` (`add <host> --to <target> --mode passthrough|http|https
  [--insecure]`, confirm on rm); admin drawer Routes tab list, add, remove.
- Tests: Build goldens for the three modes and wrapper order; leaf squat
  and wildcard rules; API create/list/delete and non-admin 403; a service
  test that a route host refuses a later tile domain on the same host.

### W2 Network shares (org level, NFS or SMB)
Owns: `leaf/volume/share.go` (new, in the volume leaf), `leaf/volume/
share_test.go`, `leaf/share/*` NOT used (shares are volume-leaf rows),
`flow/deploy/mount_share.go` and its test, `internal/service/share.go`
(new), `flow/orgconfig/{file,diff,export}.go` and `orgconfig_test.go`,
`internal/service/orgconfig.go` (shares case in `walkOrgPlan`, `orgLive`),
`api/handler/v1/share.go`, `internal/api/share_test.go`, `cmd/stackr/
share.go`, `ui/drawer/org/org.templ` (Shares tab only), `web/handler/
canvas/org.go` (shares routes only), `dockerfake/fake.go` (record
CreateVolume driver and opts).
- A share is an org row; a tile mount line `share:<slug>/<sub>:/abs[:ro]`
  resolves at deploy to a Docker volume `stackr-share-<shareID>-<hash of
  sub>` created with driver `local` and opts: nfs `type=nfs,o=addr=<host>,
  <options>,device=:<path>/<sub>`; smb `type=cifs,o=username=..,password=..,
  <options>,device=//<host>/<path>/<sub>`. Credentials come from the
  share's `${{ org.params }}` refs expanded with secrets at deploy
  (`params.InEnv` rules). The volume is labelled `stackr.share=<id>`; it is
  never backed up, never orphan-pruned, and removed when no container
  references it on the tile's next remove (ponytail: leave it if Docker
  refuses). Fix round: the sweep keeps every name a remaining tile row
  still computes (the hash covers the share's recipe refs, never the
  secret value); it does not ask what a container holds.
- An SMB password is a cifs mount option, so `docker volume inspect` shows
  it to anyone with Docker access on the host (local driver).
- Refusals: a managed (database) tile mounting a share ("databases on a
  share corrupt; use a volume"); a share from another org; a sub path with
  `..`.
- Org file `shares:` block (`slug: {kind, source, options, user,
  password}`), diff rows `share`, `share-update`, `share-delete`; export.
  Remove `storage` from `removedKeys` only if the key is renamed to
  `shares`; otherwise leave it.
- Service verbs `Shares`, `CreateShare`, `DeleteShare` (owner; delete
  refused while a tile line references it); API, CLI `stackr share
  ls|add|rm`, org drawer tab.
- Tests: deploy builds the right opts for nfs and smb (fake records them);
  managed refusal; cross-org refusal; org file diff for add/update/delete;
  delete-while-mounted refusal.

### W3 Config files (`files:` from the config repo)
Owns: `flow/promote/files.go` and `files_test.go`, `flow/deploy/
mount_files.go` and its test, `internal/git/git.go` (add `ListTree(ctx,
commit, path) ([]string, error)` and `IsDir`), `internal/service/
wiring.go` (expose the repo for file reads: the `Config` closure returns a
Fetcher that also lists), `flow/promote/stackfile.go` `Fetcher` type only
(add `type Lister func(path string) ([]string, error)` beside it, carried
on `Resolved`), `flow/promote/plan.go` only the one line that stores the
lister on `work`.
- Plan time: for each `files:` line of a tile, resolve whether the repo
  path is a file or a folder at the release's commit; block with "tile %s:
  files: %s is not in the repo at %s" when missing. Record on the plan the
  list of (src, dst, template) per tile; no writing at plan time.
- Deploy time (`fileBinds`): write under `<DataDir>/files/<tileID>/<commit>/
  <n>/...` once per commit (skip if present), content fetched from the
  release's pinned config commit; `:template` lines run the file through
  the params resolver with secrets (`rr.Expand(params.InEnv, text)`, whole
  file), others are copied byte for byte; folders are walked. Bind each as
  `<hostpath>:<dst>:ro`. Old commit folders of the tile are removed after a
  successful deploy when no container of the tile still runs on them.
- Hand-made tiles (no config repo) with `files:` are refused with "files:
  needs the stack's config repo".
- Tests: file and folder land at the right paths with `:ro`; template
  expansion with a secret; missing path blocks; the stale commit folder is
  pruned; a non-config stack is refused. Use `servicetest.NewGit`.

### W4 Forward auth
Owns: `leaf/domain/caddy_auth.go`, `caddy_auth_test.go`, `testdata/
forward_auth*.json`, `leaf/domain/domain.go` `checkExtras` only, `ui/
drawer/tile/tile.templ` domain form (adds forward auth URL field) and
`view.go`, `web/handler/tile/handler.go` `AttachDomain` (reads the field),
`docs/features/*` if a domains page exists.
- `authHandlers`: when `e.ForwardAuth` is set emit Caddy's forward-auth
  shape: `reverse_proxy` to the URL's host with `handle_response` routes
  (status 2xx: copy `CopyHeaders` into the request via `headers`
  handler then continue; any other: `copy_response`), request headers
  `X-Forwarded-Method`, `X-Forwarded-Uri` and `X-Forwarded-Host` set from
  placeholders, method GET, body dropped. Mirrors what the Caddyfile
  `forward_auth` directive expands to; keep the JSON minimal and prove it
  loads with `caddy.Load` in the test (caddy is already a dependency).
  Basic auth and forward auth together: forward auth first.
- The default CopyHeaders when empty: `Remote-User`, `Remote-Groups`,
  `Remote-Name`, `Remote-Email`.
- Drawer: one URL field; headers only through the stack file.
- Tests: golden for forward auth alone and with basic auth; load check;
  URL validation refusals.

### W5 Host access (admin approval, closes ungated privileged)
Owns: `leaf/hostgrant/*` (new), `flow/promote/hostaccess.go` and test,
`flow/deploy/mount_host.go` and test, `internal/service/hostgrant.go`
(new), `internal/service/jobs.go` (promote and deploy handlers only: map
`errs.NeedsApproval` to a parked job), `api/handler/v1/hostgrant.go`,
`internal/api/hostgrant_test.go`, `cmd/stackr/hostgrant.go`, the job view
that shows a parked job (`ui/drawer/env/*` job rows or the jobs page:
an Approve button for admins when the blocker is a NeedsApproval),
`flow/deploy/spec.go` privileged line only.
- Grant set of a stack: sorted `host:` mount lines of every tile in the
  stack file (or, for a UI tile, of the tile being saved) plus a privileged
  flag plus device lines. `checkHostAccess` compares the plan's set with
  the stack's `host_grants` row; a difference adds a blocker that `Apply`
  turns into `errs.NeedsApproval{Stack, "host access: <diff>"}` so the job
  parks instead of failing. A single-tile deploy (UI save) runs the same
  check in `hostBind`/the deploy flow before any container starts.
- `ApproveHostGrant(stackID)` (verb `hostgrant.approve`, admin only): writes
  the row with the set the parked job asked for (stored on the job payload)
  and requeues the job. Revoke = delete row; running tiles stay, the next
  deploy parks.
- `hostBind`: with a matching grant, `host:/a:/b[:ro]` becomes the Docker
  bind `/a:/b[:ro]`. `spec.go`: `Privileged` only when the grant says so.
- Audit: the stack drawer header shows "host access" when a grant exists
  (one line in `ui/drawer/stack`, owned here).
- Tests: a push with a docker.sock line parks with the approval text;
  approve then the same release deploys; a changed line parks again; a
  redeploy of the same release does not; privileged without a grant parks;
  non-admin approve is 403.

### W6 UDP ports and docs
Owns: `docs/rewrite/PROGRESS.md` (a dated section for the blitz),
`docs/rewrite/tasks/leftovers.md` (strike F PROXY protocol is NOT done;
leave), `README.md`/`AGENTS.md` mentions if any, `flow/deploy/spec_test.go`
for the udp fix, a rig QA script skeleton under
`scripts/qa/blitz.sh` (bash, uses the CLI; no execution yet).
- Verify the wave 0 udp fix end to end in the fake: `53:53/udp` and
  `53:53` on one tile bind both.
- Write the QA script: login, create a passthrough route to a local TLS
  upstream container and check the cert serial through the proxy; an http
  route to the panel's own IP; an nfs share against a throwaway nfs server
  container on the rig; a files tile from the smoke config repo; a forward
  auth domain against a tiny "always 200 with Remote-User" container; a
  host grant flow (push parks, approve, deploys).

## Wave 2: seams (one worker, serial)
Read `blitz-seams.md`, resolve every cross-file need, regenerate openapi
and templ, `make test && make lint && make templint`, fix what wave 1 left
red, update `schema.md`/`verbs.md`/`PROGRESS.md`. No new features.

## Wave 3: rig (one worker, serial)
`make release RELEASE=v0.6.0-dev.14`, `scripts/rig.sh upgrade 0.6.0-dev.14`,
run `scripts/qa/blitz.sh`, fix and re-ship as dev.15 if needed. Never edit
the stacks shop, web, infra or byhand. Report pass/fail per feature with
the exact command output lines that prove it.

# Blitz seams

Cross-file needs the wave 1 workers hit that they do not own. Wave 2 reads
this file and resolves every entry.

## Wave 0 hand-off

Wave 0 is done: `go build ./... && go vet ./... && make test && make lint &&
make templint` are green. Every hook below is a stub with the exact
signature wave 1 must keep. Each stub carries a `ponytail:` comment; delete
it when the body lands.

### Migration and stores

`internal/db/migrations/004_blitz.{up,down}.sql`: `routes`, `shares`,
`host_grants` as in the plan. `store.Tables` gained `Routes`, `Shares`,
`HostGrants`, bound in `bind()`.

```go
// internal/service/internal/store/routes.go
type Route struct { ID, Host, Mode, Target string; Insecure bool; CreatedAt time.Time }
type RouteStore interface {
	Create(ctx context.Context, r Route) error
	Get(ctx context.Context, id string) (Route, error)
	List(ctx context.Context) ([]Route, error) // ORDER BY host
	Update(ctx context.Context, r Route) error
	Delete(ctx context.Context, id string) error
}

// internal/service/internal/store/shares.go
type Share struct { ID, OrgID, Slug, Kind, Source, Options, User, PasswordRef string; CreatedAt time.Time }
type ShareStore interface {
	Create(ctx context.Context, s Share) error
	Get(ctx context.Context, id string) (Share, error)
	GetBySlug(ctx context.Context, orgID, slug string) (Share, error)
	ListByOrg(ctx context.Context, orgID string) ([]Share, error) // ORDER BY slug
	Update(ctx context.Context, s Share) error
	Delete(ctx context.Context, id string) error
}

// internal/service/internal/store/host_grants.go
type HostGrant struct { ID, StackID, Lines string; Privileged bool; ApprovedBy string; CreatedAt time.Time }
type HostGrantStore interface {
	Create(ctx context.Context, g HostGrant) error
	Get(ctx context.Context, id string) (HostGrant, error)
	GetByStack(ctx context.Context, stackID string) (HostGrant, error)
	Update(ctx context.Context, g HostGrant) error
	Delete(ctx context.Context, id string) error
}
```

Store has no Create-conflict mapping beyond `mapErr` (a duplicate host or
`(org_id, slug)` surfaces as the store's usual conflict error); the leaf does
its own friendly check first.

### errs

`internal/service/errs/errs.go`:

```go
type NeedsApproval struct{ Stack, What string }
func (e NeedsApproval) Error() string // "waiting: <What>; a server admin approves it on the stack"
func IsNeedsApproval(err error) (NeedsApproval, bool)
```

`flow/jobs/jobs.go` `call()`: a NeedsApproval parks the job like Unset. The
job log gets the `Error()` text once; `waiting_param` is `What`. With
`Options.ParamSet` set (production), `What` is not a param, so `ParamSet`
must report it not-resolved (returns false or an error) and the job stays
parked until `ApproveHostGrant` requeues it with `Leaf.Requeue`. W5: check
that the production `ParamSet` does not accidentally resolve the text.
Test: `TestWaitingApproval` in `flow/jobs`.

### Tile mount and port parsing (`leaf/tile/check.go`)

```go
const ( MountVolume = "volume"; MountHost = "host"; MountShare = "share" )
type Mount struct {
	Kind   string // MountVolume | MountHost | MountShare
	Volume string // MountVolume: the volume slug
	Host   string // MountHost: the absolute host path (clean, no ..)
	Share  string // MountShare: the share slug
	Sub    string // MountShare: path inside the share, "" = root, no ..
	Path   string // the absolute container path
	RO     bool
}
func ParseMount(l string) (Mount, error)
func ParsePort(l string) (host, cont int, proto string, err error) // proto tcp|udp
```

Disambiguation: `host:/a:/b[:ro]` is a host line only when the third
segment is absolute; `host:/d` stays a volume named `host`. `share:x/sub:/d`
is a share line when the second segment is not absolute. `Mount.Sub` is
cleaned (`a/b`, never `/a/b`).

Other callers that cut a volume line on the first `:` still treat
`host:/a:/b` as volume `host` (`service/volume.go` ~52, `web/handler/env/
handler.go` `mounts()`); they should call `tile.ParseMount` and skip
non-volume kinds. Not changed in wave 0 (not mine to own); wave 2 or W2/W5.

### Ports

`flow/deploy/spec.go` `publishedPorts` keys the map `host` (tcp) or
`host/udp`; the value is unchanged (`container[/udp]`). `docker/containers.go`
`portBindings` strips the `/proto` off the key for `HostPort`, so `53:53` and
`53:53/udp` both bind. Tests: `TestSplitCommandAndPorts`, `TestPortBindings`.
Config repos and the UI still validate the line with `tile.parsePorts`
(now `ParsePort`), which accepts `/udp`.

### Deploy hooks (`internal/service/internal/flow/deploy`)

```go
// mount_share.go
func (f *Flow) shareBind(_ context.Context, _ store.Org, t store.Tile, _ tile.Mount) (string, error)
// mount_host.go
func (f *Flow) hostBind(_ context.Context, _ store.Stack, t store.Tile, _ tile.Mount) (string, error)
// mount_files.go
func (f *Flow) fileBinds(_ context.Context, t store.Tile, _ store.Environment, _ store.Stack, _ *params.Resolver) ([]string, error)
```

Each returns `errs.Conflictf("%s: share mounts are not supported yet"` /
`host mounts` / `files: mounts` `, t.Slug)`. `shareBind` and `hostBind`
return the full Docker bind string (`src:/dst[:ro]`; `roSuffix(m)` in
`deploy.go` gives `:ro`).

`deploy.go` `resolve()`: each `volumes` line is `Expand`ed, parsed with
`tile.ParseMount`, and dispatched on `Mount.Kind` to `shareBind`, `hostBind`
or the new `volumeBind(ctx, t, e, m)` (the old volume code, moved). After
the volume loop, `fileBinds` is called when `t.Files` is non-blank and its
binds are appended. `Flow.DataDir string` added, set from `cfg.DataDir` in
`orchestrator.go`.

The two `//nolint:staticcheck` comments in `resolve()` on the `fileBinds`
call exist only because the stub always errors (SA4023); delete them with
the stub.

Promote side: the refusal at `plan.go` (files `not supported yet`) is gone.
Until W3 lands, a stack file with `files:` plans clean and the deploy job
fails with the Conflict above.

### Caddy hooks (`leaf/domain`)

```go
// caddy.go
func Build(in Install, tiles []TileRoute, routes []Route, expand Expand) (json.RawMessage, error)
// caddy_auth.go
func authHandlers(e Extras, t TileRoute, expand Expand) []route
// caddy_routes.go
type Route struct {
	Host, Mode, Target string
	Insecure           bool
}
func externalRoutes(_ []Route, _ bool) (plain, secure []placed, wrapper route)
```

- `authHandlers` holds the moved basic-auth block (Extras, else the cascade's
  `t.Protect`). `domainRoute` calls `hs = append(hs, authHandlers(e, t,
  expand)...)` first, so forward auth (W4) goes at the front of the
  returned slice and "forward auth first" holds.
- `Build` appends `externalRoutes`' plain and secure routes after the
  tiles' (the `server()` sort orders them) and adds every `secure` entry's
  host (non-empty) to `httpsHosts`, so an `https`/`http`-mode route gets a
  certificate with no extra return value. A pass-through host must not
  appear in `secure` (no cert of ours). When `wrapper != nil` and TLS is
  on, the https server gets `listener_wrappers: [wrapper, {"wrapper":"tls"}]`.
  `tlsOn` is passed so `externalRoutes` can return no `secure` rows with TLS
  off.
- Every `Build` caller updated: `flow/deploy/proxy.go` passes `nil` routes
  (W1: load rows from `st.Routes.List` into `[]domain.Route`),
  `caddy_test.go` passes `nil`. Goldens are byte-identical.
- `Extras.ForwardAuth *ForwardAuth` (`json:"forward_auth,omitempty"`) with
  `ForwardAuth{URL string; CopyHeaders []string}`. `checkExtras` requires
  an `http` or `https` URL with a host (`proxy.forward_auth.url`).
  `promote.ProxyConf.ForwardAuth` (yaml `forward_auth: {url, copy_headers}`)
  maps through `specOf`. `api/handler/v1/tiles.go` needed no change:
  `DomainIn.Extras` is `service.DomainExtras` = `domain.Extras`.
  Default `CopyHeaders` is W4's job in `authHandlers`.

### Promote hooks (`flow/promote`)

```go
// hostaccess.go
func (f *Flow) checkHostAccess(_ context.Context, _ *Plan, _ *work) error
// files.go
func (f *Flow) planFiles(_ context.Context, _ *Plan, _ *work) error
```

Both no-ops returning nil. `planConfig` ends with `planDeletes`, then
`planFiles`, then `return f.checkHostAccess(ctx, p, w)`.

### Authz verbs (`internal/authz/authz.go`)

| verb | level |
|---|---|
| `route.admin` | admin |
| `share.read` | read |
| `share.write` | owner |
| `hostgrant.approve` | admin |

### API routes (`internal/api/routes.go`) and handler stubs

| method, path | op | verb | handler (stub file) |
|---|---|---|---|
| GET `/admin/routes` | `admin.route-list` | `route.admin` | `Routes` (`v1/route.go`) |
| POST `/admin/routes` | `admin.route-create` | `route.admin` | `CreateRoute` |
| DELETE `/admin/routes/:route` | `admin.route-delete` | `route.admin` | `DeleteRoute` |
| GET `/orgs/:org/shares` | `share.list` | `share.read` | `Shares` (`v1/share.go`) |
| POST `/orgs/:org/shares` | `share.create` | `share.write` | `CreateShare` |
| DELETE `/orgs/:org/shares/:share` | `share.delete` | `share.write` | `DeleteShare` |
| GET `/orgs/:org/stacks/:stack/host-grant` | `hostgrant.get` | `org.read` | `HostGrant` (`v1/hostgrant.go`) |
| POST `/orgs/:org/stacks/:stack/host-grant/approve` | `hostgrant.approve` | `hostgrant.approve` | `ApproveHostGrant` |

Stub types in the v1 files: `RouteIn`, `RouteOut`, `ShareIn`, `ShareOut`,
`HostGrantOut`, plus `errNotBuilt` in `route.go`. Owners may swap `Out`
types for `service.*` types (regenerate openapi). Middleware
(`internal/middleware/access.go` `byVerb`) learned the params `route`
(org-less, admin) and `share` (the verb takes the org and refuses another
org's share). W5/W2: a share id from another org must 404 in the verb.

### CLI

`cmd/stackr/route.go` `(a *app) routes()` is the `route` noun, registered
inside `admin()` (so `stackr admin route ls|add|rm`, as W1 specifies).
`share.go` `(a *app) shares()` (`stackr share ls|add|rm`) and `hostgrant.go`
`(a *app) hostGrant()` (`stackr host-grant show|approve <stack>`) are
registered in `commands()`. Stub leaves print `not built yet`; their `op`
annotations are `admin.route-list|create|delete`, `share.list|create|delete`,
`hostgrant.get|approve`, so `TestEveryRouteHasAVerb` passes. Owners keep
those op strings (or add to them) when they fill the bodies.

### UI

Admin drawer `Tabs`: `settings, users, routes, update, caddy, backups`;
`ui.Routes(ui.RoutesView{})` (empty struct, "No routes yet.") rendered by
`tab()` case `"routes"` in `web/handler/admin/handler.go`. Org drawer
`Tabs`: `..., domains, shares, backups, config`; `orgui.Shares(orgui.
SharesView{})` rendered by `orgTab` case `"shares"` in
`web/handler/canvas/org.go`. Tests extended: `TestAdminDrawer`,
`TestDrawerTabs`. The empty views take no data yet; W1 and W2 add fields to
`RoutesView` and `SharesView` and replace the handler line.

### Docs

`docs/rewrite/schema.md`: three table rows and the migration list.
`docs/rewrite/verbs.md`: three rows (marked not built yet).
`docs/openapi.json` regenerated (eight new operations).

### Deviations from the plan, and why

- `fileBinds` is called from `resolve()`, not `prepare()`: `rr` (the params
  resolver the signature takes) exists only inside `resolve()`. `prepare()`
  just lost the refusal.
- `Build` derives the https cert hosts from `externalRoutes`' `secure` rows
  instead of a fourth return value, keeping the signature as written.
- `route` CLI noun is registered in `admin()`, not `commands()` (W1's
  command is `stackr admin route`).
- Host-grant paths sit under the real stack path `/orgs/:org/stacks/:stack/
  host-grant`, not `/stacks/:stack/...` (no such top-level path exists).
- API stubs: GET stubs answer an empty list (`[]`) and, for the grant read,
  404, not 501, because `TestReadsLeakNoSecret` sweeps every owner GET and
  fails on 5xx. Mutating stubs answer 501.
- `TestFileBlockers` (`flow/promote/promote_test.go`) no longer asserts a
  `files:` blocker (the refusal is removed per the plan); it asserts the
  unset-param blocker it already produced.
- `parseMount` and `parsePorts` stay as thin wrappers over the new exported
  `ParseMount` and `ParsePort`, so `checkLists` is unchanged.
- Two `//nolint:staticcheck` comments in `deploy.go` (see above).

### Seams for wave 1 to write below

(none yet)

### W1 needs

- `internal/service/domainres.go` (not mine): `CreateDomainResource` should refuse
  a host an external route holds, the reverse of the route create check.
  One line before `o.domainres.Create`: `if err := o.checkRouteHost(ctx, host); err != nil { return DomainResource{}, err }`
  (`checkRouteHost` is in `internal/service/route.go`).
- `flow/promote` apply writes tile domains through `domains.Attach` without
  `checkSquat`, so a stack file can name a host an external route holds. Same
  helper (`Orchestrator.checkRouteHost`) would need to run in plan or apply.
- `internal/api/handler/v1/route.go` keeps `errNotBuilt` (share.go and
  hostgrant.go stubs use it); move it when the last stub goes.
- W1 touched, beyond its list, because the verbs need wiring: `internal/service/orchestrator.go`
  (`routes` leaf field + build line), `internal/service/wiring.go` (`proxyConfig`
  loads routes and passes them to `deploy.ProxyConfig`), `internal/service/domain.go`
  (`checkSquat` also refuses a route host). `deploy.Flow` has no routes field:
  `ProxyConfig(ctx, in, ext []domain.Route)` takes them from the caller.
- Naming: `Orchestrator.Routes(ctx, envID)` already means the env's tile
  domains, so the admin verbs are `ExternalRoutes`, `CreateExternalRoute`,
  `DeleteExternalRoute` (type `service.ExternalRoute`).
- `go get github.com/mholt/caddy-l4@v0.1.2` bumped `pires/go-proxyproto`
  0.12 -> 0.13 and `quic-go` 0.59.1 -> 0.60.0 in go.mod; `go mod tidy` ran.
- `docs/openapi.json` not regenerated: `RouteOut` is now `service.ExternalRoute`
  (same fields), so wave 2's dump changes the schema name.

### W4 needs

- `internal/service/domain.go`: add `ForwardAuth = domain.ForwardAuth` to the
  alias block (next to `DomainExtras`). `web/handler/tile/handler.go`
  `AttachDomain` fills `Extras.ForwardAuth` through a JSON decode meanwhile;
  swap it for `s.Extras.ForwardAuth = &service.ForwardAuth{URL: u}` and drop
  the `ponytail:` comment.
- `internal/service/internal/leaf/domain/domain_test.go`: W4 added one case
  ("forward auth without a host") to the `Attach` refusal table; file is not
  in the W4 list.

### W3 needs (config files)

- `flow/deploy/deploy.go` `resolve()`: `fileBinds` is no longer a stub, so
  drop the two `//nolint:staticcheck` comments on the call and the ponytail
  line above them.
- `flow/deploy/deploy.go` `Run`, next to `f.prune(...)` (both call sites):
  call `f.pruneFiles(t, mounts)` after a successful rollout (`mount_files.go`;
  it removes `<DataDir>/files/<tile>/<commit>` folders no mount points into,
  logging a failure rather than failing the deploy). `mounts` is the
  `docker.Detail.Mounts` of every replica of `f.Tiles.Replicas(ctx, t)`, and
  `leaf/tile` has no public inspect: it needs `func (l *Leaf) Mounts(ctx,
  t store.Tile) ([]string, error)` (Inspect each replica, concatenate
  `Detail.Mounts`). Until then nothing prunes. Not auto-pruned inside
  `fileBinds` on purpose: the old replicas still run on the old folder during
  the swap and a restart of one would lose its bind source.
- `flow/deploy/deploy_test.go` `TestMountKindsDispatch`: I removed its `files`
  case (the stub message is gone); the switch there now has one case left
  (`host`), W5 can fold it when `hostBind` lands.

### W3 deviations

- No `Lister` field on `Resolved`: `Config` keeps its `Fetcher`, and a
  trailing-slash path (`fetch("conf/")`) returns the folder's files NUL-joined
  (`wiring.go` `stackFile`). `promote.listOf` wraps that into a `Lister`
  stored as `work.list` (the one `plan.go` line, plus its struct field).
- Deploy reads the config clone itself (`git.Repo{Dir: <DataDir>/repos/
  config-<stackID>}`, the commit from the env release's `_config` pin);
  `Flow` has no fetcher. This relies on plan having cloned that commit.
- Plan records nothing per tile; deploy re-parses `t.Files`.
- A commit folder is written once (`<commit>.part` then rename); a template
  does not follow a param edit until the next config commit (`ponytail:`).
- File mode bits from git are not kept: files 0644, folders 0755 under a 0700
  `<DataDir>/files`.

### W2 needs (network shares)

- `internal/service/orchestrator.go` line ~292 (one-line edit made by W2, not
  mine to own): `volume.New(st.Volumes, d).WithShares(st.Shares)`. The six
  `volume.New(store, fake)` test call sites stay as they are; a test that
  mounts a share chains `.WithShares(st.Shares)`.
- `flow/deploy/deploy_test.go` `TestMountKindsDispatch`: W2 removed the
  `share` case (the hook is built). W3/W5 remove theirs the same way.
- `service/volume.go` `mounters()` and `web/handler/env/handler.go`
  `mounts()` still cut a volume line on the first `:`; a `share:` line reads
  as a volume named `share`. Harmless for deletes (no env volume is named
  `share`), but they should call `tile.ParseMount` and skip non-volume kinds.
- `docs/openapi.json`: not regenerated. `v1/share.go` kept `ShareIn` and
  `ShareOut`, now filled; the operations did not change, so the dump should
  be identical unless the `Delete` param is documented.
- `docs/rewrite/verbs.md` and `schema.md` rows for shares still say "not
  built yet"; wave 2 updates them.
- Deviations: the share's Docker volume is `stackr-share-<shareID>-<hash>`
  where the hash covers the sub path AND the mount opts (Docker keeps an
  existing volume's opts, so an edited share must get a new name). smb opts
  also carry `addr=<host>` (cifs needs it with the local driver). No volume
  is removed on the tile's next remove or on share delete (ponytail in
  `leaf/volume/share.go`): that sweep touches `deploy.go`/`Tiles.Remove`,
  which W2 does not own. `DeleteShare` takes a slug or an id.
- Org file: `shares:` block (`kind, source, options, user, password`; the
  password is an `${{ org.params }}` ref). Absent block says nothing; a
  present block deletes the shares it omits (the only delete the file does),
  blocked while a tile line mounts one. The `storage` removed-key hint now
  reads "network shares are under shares:".

### W5 needs

W5 (host access) is built end to end. Edits made outside W5's list, each
small and needed for the feature to work at all (wave 2: keep or move):

- `flow/deploy/deploy.go`: `Flow.HostGrants *hostgrant.Leaf` and, at the top
  of `resolve()`, `checkAccess` (parks a tile whose host lines, devices or
  privileged the grant does not cover) and `r.privileged`. `hostBind` and
  `spec.go` (`resolved.privileged`) read from it.
- `internal/service/orchestrator.go`: the `hostgrant` leaf, `deploy.Flow.
  HostGrants`, and `jobs.Options.ParamSet`, which reports a host access
  text as not resolved so a parked job is requeued only by
  `ApproveHostGrant` (every other param keeps the old requeue-every-poll).
- `flow/promote/stackfile.go` `checkRefs` and `flow/promote/sync.go`
  `mounts`: now `tile.ParseMount`, so a `host:` or `share:` line no longer
  reads as an undeclared volume named `host`. `service/volume.go` (new
  `MountedVolume`, used by `mounters`, `TileVolumes` and `web/handler/env`
  `mounts()`) is the same fix.
- `internal/service/release.go` `PlanPromote`: `CanDeploy` is also true for a
  plan blocked only on host access (`promote.Plan.OnlyHostAccess`), else a
  UI or CLI promote refuses before it queues and nothing ever parks.
- `web/handler/canvas/stack.go`: the stack drawer's Settings fills the new
  `Host access` section (`ui/drawer/stack` `HostView`) and POST
  `/:org/:stack/-/drawer/host-grant/approve` (verb `hostgrant.approve`).

Left for wave 2:

- The Approve button sits on the stack drawer, not on the job row: the job
  row (`ui/components/job_status.templ`, `web/render.JobView`) is shared by
  every drawer and not W5's. The parked job's text already says "a server
  admin approves it on the stack".
- Revoke has a service verb (`RevokeHostGrant`) and a leaf, but no API route
  or CLI verb: it needs a `DELETE .../host-grant` row in `api/routes.go`
  (verb `hostgrant.approve`, op `hostgrant.revoke`), a `v1` handler, a
  `host-grant revoke` leaf and an openapi regen.
- `docs/openapi.json` is stale: `HostGrantOut` is now `service.HostGrant`
  (`stack_id, granted, lines, privileged, approved_by, created_at,
  pending`). Regenerate. `docs/rewrite/verbs.md`: the host-grant row is
  built; note the CLI is `stackr host-grant show|approve` with `--stack`.
- `flow/graph/graph.go` ~924 still cuts a volume line on the first colon, so
  the canvas draws a `host:` mount as a volume named `host`. Use
  `service.MountedVolume`-style `tile.ParseMount` and skip other kinds.
- Env sync (`flow/promote/sync.go`) has no plan-time host check: the copied
  tiles park at deploy time (`Owed` is kept for a parked rollout), which is
  after the sync's writes, not before.
- A grant covers additions only (`hostgrant.Set.Missing`): a stack file that
  drops a line never parks, and `ApproveHostGrant` adds to the row (it never
  shrinks it); revoke is the only way down.

## Fix round needs

- F3 (shares): `internal/service/orgconfig.go` ~515 (`putOrgShare`) calls
  `o.volumes.UpdateShare` directly; change it to `o.UpdateShare(ctx, orgID,
  slug, want.Spec(slug))` (new in `internal/service/share.go`) so the update
  also sweeps the old recipe's volumes from the tile rows. Until then an
  org-file share edit leaves the old volume until the next tile removal.

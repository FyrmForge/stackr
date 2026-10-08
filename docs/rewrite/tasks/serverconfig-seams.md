# Server config seams

Needs that cross a worker's files go here, under a heading with the worker id.
Design: `serverconfig.md`. Who builds what: `serverconfig-blitz.md`.

## Wave 2 status (every open item below, resolved or moved)

RESOLVED means fixed in the tree with a test; ACCEPTED means decided to stay
as built; LEFTOVER means moved to `leftovers.md` section I.

| From | Item | Status |
|---|---|---|
| W0 | `notbuilt.go`, `pending.go`, `TestServerConfigStubsRefuse` | RESOLVED: S4 deleted all three; no `notBuilt` stub is left |
| W0 | `templ generate`, openapi regen, `make test lint templint` | RESOLVED: all green, openapi unchanged by regen |
| W0 | `serverconfig.md` "Param refs know `org.params` only" | RESOLVED: S2 |
| S1 | `connectorOf` takes the first host match | RESOLVED: `flow/orgconfig/diff.go` uses `connector.Resolve`, blocks `Ambiguous` ("name the connector to use") |
| S1 | `SetConfigRepo` checked `conns.Get` | RESOLVED: `Usable`; the stack picker lists shared server connectors (", server") |
| S1 / S6 | GitHub App manifest registers `BASE_URL` | RESOLVED: `githubapp.Client.WithPanelHost` reads `panel_domain` (scheme and port stay `BASE_URL`'s). Apps made before keep their webhook URL (impact line, LEFTOVER) |
| S1 | `GET /orgs/:org/connectors` lists own only | ACCEPTED, LEFTOVER if a script needs the usable set |
| S1 | `TestOrgFileShares` | RESOLVED: S4 ticks `share:docs` |
| S2 | openapi regen, AGENTS.md note | RESOLVED |
| S2 | secret reveals are not audited | LEFTOVER |
| S2 | server ref in a tile param or org field refused at deploy, not save | ACCEPTED (no leaf may import `leaf/params`), LEFTOVER |
| S3 | `archive_key` create-only | ACCEPTED: S4 writes it through the store; LEFTOVER for a `backup.Dest` field |
| S3 | `leaf/route` `Check`, `planfile` params and `Defaults` copies | LEFTOVER |
| S3 | `protect_password` exports as stored | ACCEPTED: documented in AGENTS.md and PROGRESS ceilings |
| S3 | `Plan.Notes` beside blockers (S7) | RESOLVED: `ServerPlanView` sets `Warnings: p.Notes` |
| S4 | `proxy_custom` Caddy cannot load stays stored | RESOLVED: `SetSettings` takes the value back and re-pushes (web form, settings PUT, file apply); the apply-only restore in `serverWalk.settings` is gone |
| S4 | `server_config_*` through the settings PUT | RESOLVED: `SetSetting` (single key, the settings API's door) answers Invalid and points at `PUT /admin/config-repo`; `SetSettings` stays open for bind and the tests |
| S5 | generated redirect is 301, not 308 | RESOLVED: `leaf/domain/caddy.go` `domainRoute` answers 308 for `Auto` redirects, 301 for a declared one. The tile drawer chip still says "301" (LEFTOVER) |
| S5 | managed tile URL, declared org rows, no transaction | ACCEPTED, LEFTOVER |
| S6 | session cookie domain | RESOLVED: `CookieDomain: ""` in `cmd/stackrd/main.go`; `auth.SetSession` and `ClearSession` also expire the old `Domain=<panel host>` cookie (a browser holding both sends the old one first and a stale session would shadow new logins), `internal/auth/cookies.go`. The legacy-cookie expiry was removed on 2026-10-08 (no users, no compat) |
| S6 | `checkSquat` ignores the panel host | PARTIAL: a tile domain on the panel host is refused (`checkPanelTaken`) and `checkPanelRoutes` refuses a `panel_domain` a tile domain holds (a changed value only). A domain resource shadows nothing, so it is not checked. LEFTOVER: `CreateDomainResource` and exact-host http or https external routes on the panel host are still allowed |
| S6 | `panel_domain` as `localhost` or an IP | LEFTOVER (the file diff refuses it; the form does not) |
| S7 | `approveOpts` twice | RESOLVED: `render.ApproveOpts` |
| S7 | `ServerPlanView` copies the add-kind list | ACCEPTED: kinds match `flow/serverconfig/plan.go` `Summary()` today |
| S7 | `TestAdminConfigBind` asserts the repo with `HasSuffix` | ACCEPTED: bind stores the full URL, the assertion is still true |

## W0 hand-off (the contract)

Wave 0 landed the schema, the shared plan package, the approve contract, the
catalogue knobs, the service verbs as stubs, the job kinds, the API routes and
authz verbs. `go build ./... && go vet ./...` is green and every test below
passes. A wave 1 worker replaces the stub bodies it owns and keeps the
signatures. Everything named here exists in the tree now.

Paths are under `internal/` unless they start with `cmd/`. `svc` is
`internal/service`, `flow` is `internal/service/internal/flow`, `leaf` is
`internal/service/internal/leaf`, `store` is `internal/service/internal/store`.

### 1. Schema (`db/migrations/005_serverconfig.{up,down}.sql`)

The runner (golang-migrate, sqlite driver) wraps each file in a transaction
with foreign keys ON, so the file uses no PRAGMA. Nothing references
`connectors` by a foreign key (`orgs` and `stacks` hold `config_connector_id`
as plain text), so the rebuild is safe. Tested up, down, up on live rows in
`db/serverconfig_test.go`; the down drops server connectors and clears the
org and stack bindings that named them.

- **005 is frozen.** `hamr dev` migrates on restart, so the dev DB has very
  likely applied it, and an edit to an applied file never re-runs. A schema
  need goes in a new `006_*.up/down.sql`, written by the worker who has it
  and noted under their heading below; never edit 005. The migration test is
  pinned to version 4 and steps 005 up, down, up, so a 006 does not disturb it.
- `connectors`: `org_id` nullable (NULL = server connector), new
  `share_all INTEGER NOT NULL DEFAULT 0` (last column). Still
  `UNIQUE (org_id, host)`: NULLs are distinct, so two server connectors may
  share a host. Partial unique index `connectors_server_name ON connectors
  (name) WHERE org_id IS NULL`: a server connector's name is unique (an org's
  connector may reuse it).
- `connector_shares (connector_id, org_id)`, primary key both, both FKs
  `ON DELETE CASCADE`, index on `org_id`. Empty and `share_all = 0` = shared
  with none.
- `server_config_plans`: `id, commit_sha, summary, plan, status, error,
  created_at, decided_at, source ('repo'|'local'), file, ticked (JSON list,
  default '[]'), confirmed`.
- `org_config_plans`: new `ticked TEXT NOT NULL DEFAULT '[]'`,
  `confirmed INTEGER NOT NULL DEFAULT 0` (after `decided_at`).
- Positional inserts in tests must now carry the new columns:
  `db/cascade_test.go` was updated (connectors 8 values, org plans 11).

### 2. Store (`store/`)

- `store.Connector`: `OrgID *string` (nil = server), new `ShareAll bool
  (db:"share_all", json:"share_all")`. **S1** adds the store methods it needs
  in `store/credentials.go`: the shares table's CRUD (list a connector's org
  ids, replace them, list the connectors shared with an org), `ListServer`,
  a by-name lookup, and a `GetByHost` that is not org-bound. No store type
  exists yet for `connector_shares`.
- `store.OrgPlan` gained `Ticked store.StringList` and `Confirmed bool`.
- `store.ServerPlan` (`store/server_plans.go`): the same fields without
  `OrgID`, plus `Source string`, `File string` (json:"-": never in the API),
  `Ticked`, `Confirmed`. `store.Tables.ServerPlans store.ServerPlanStore`
  (`Create, Get, ListNewest(limit), ListByStatus(statuses...), Update,
  Delete`).
- `store.PlanRefs` + `Refs()` on both plan rows: how the one status machine
  reaches the columns (`newTable` reads only top-level `db` tags, so the
  rows are flat, not embedded).

### 3. `internal/service/internal/planfile`

A sibling of `slug`. Flows and the service may import it; it imports no leaf
and no flow.

```go
type Change struct { Kind, Tile, Field, Old, New, Note string
    Impact string; Optional bool; Key string } // json: impact/optional/key omit when empty
func (c Change) Line() string                  // moved here from flow/promote
type Plan struct { Changes []Change; Blockers, Notes []string }
func (p *Plan) Block(format string, a ...any)  // pointer receivers: Block, Add
func (p *Plan) Add(c Change)
func (p Plan) Blocked() bool                   // value receivers from here down
func (p Plan) Risky() bool                     // any Change.Impact != ""
func (p Plan) Impacts() []string               // the impact lines, plan order
func (p Plan) Removals() []Change              // Optional rows, plan order
func (p Plan) AutoOK() bool                    // !Blocked && !Risky && no removals
func (p Plan) TickedRemovals(keys []string) []Change
func (p Plan) OnlyTicked(keys []string) Plan   // plain changes + ticked removals only
func (p Plan) Approvable(ticked []string, confirm bool) error
        // Invalid("ticked") for a key that is not a removal row;
        // Conflict "This plan has impact lines; confirm to approve." when Risky && !confirm
func (p Plan) Summary(isAdd func(kind string) bool) string
        // "2 to add, 1 to change, 2 removals to review, needs confirm, 1 blocker"
type Param struct { Type string; Value *string }   // yaml: type, value,omitempty
func CheckParams(map[string]map[string]Param) error
func StrictYAML(data []byte, out any, removed map[string]string) error // removed = key -> hint
```

- `flow/orgconfig`: `type Change = planfile.Change`, `type Param =
  planfile.Param`, `type Plan struct{ planfile.Plan }` (flat in JSON, stored
  rows read the same; `Summary()` stays per flow). Its copies of
  `strictYAML`, `humanYAML` and `checkParams` are gone.
- `flow/promote`: `type Change = planfile.Change`, `type Param =
  planfile.Param`; `strictYAML` is a one-line wrapper over
  `planfile.StrictYAML(.., removedKeys)`; `checkParams` and `humanYAML` are
  gone; `strictNode` stays local.
- Removal key vocabulary (Key is `<kind>:<key>`): `route:<host>`,
  `dest:<name>`, `domain:<host>`, `share:<slug>`,
  `connector-share:<conn>/<org>`, `org-binding:<slug>`. A removal row keeps its
  flow's own Kind (`share-delete`, `route-delete`, ...) with `Optional: true`
  and that Key.
- **An apply must walk `plan.OnlyTicked(row.Ticked)`**, never the raw diff:
  unticked removals then cannot run. `applyOrgPlan` already does
  (`svc/orgconfig.go`); the server apply must too.

### 4. `leaf/orgplan` (one status machine, two tables)

`orgplan.Machine[T, P]` is generic over the row; `orgplan.Leaf` (org, `New`)
and `orgplan.Server` (`NewServer`) wrap it. Shared methods: `Get`,
`Create(ctx, row)` (supersedes the scope's older undecided pending/clean rows;
resets ticked/confirmed), `Approve(ctx, id, ticked []string, confirmed bool)`,
`SetStatus(ctx, id, Rejected|Applied)`, `SetError`, `RejectPending(ctx,
scope)`, `Newest(ctx, scope, limit)`. `Leaf.ForOrg(ctx, org, limit)`;
`Server.Latest(ctx, limit)`, `Server.RejectUndecided(ctx)`. The orchestrator
holds `o.orgPlans *orgplan.Leaf` and **`o.serverPlans *orgplan.Server`**.

### 5. Approve contract (`svc/approve.go`)

```go
type ApproveOpts struct { Ticked []string; Confirm bool } // json: ticked, confirm
func checkApprove(id, planJSON string, opts ApproveOpts) error
        // blocked -> Conflict "This plan is blocked: ..."; then Plan.Approvable
```

`ApproveOrgPlan(ctx, id, opts ApproveOpts) (Job, error)` is done: pending
plans are checked, then `o.orgPlans.Approve(ctx, id, opts.Ticked,
opts.Confirm)` stamps the row, then the apply job is queued. `planOrg`'s auto
rule is `p.AutoOK()` (it was `!Blocked`); auto-approve passes `ApproveOpts{}`.
`ApproveServerPlan` (S4) does the same with `checkApprove(pl.ID, pl.Plan,
opts)` and `o.serverPlans.Approve`. A plan with only removal rows is pending
and applies nothing unless something is ticked.

Surfaces: the API takes an optional JSON body `v1.ApproveIn {ticked, confirm}`
(no body = plain approve; `BodyOptional()` is a new decode marker in
`api/handler/v1/endpoint.go`); the canvas and setup web handlers already read
the form fields `ticked` (repeated) and `confirm` (any value), so S7's
checkboxes and confirm button post those names.

### 6. Service verbs (`svc`, all on `*Orchestrator`)

Types: `ServerPlan = store.ServerPlan`, `ServerConfigPlan =
serverconfig.Plan`, `ServerBinding{ConnectorID, Repo, Branch, Path string;
Auto bool}` (json: connector_id, repo, branch, path, auto),
`ServerConnector{store.Connector; OrgIDs []string "org_ids"}`.

| Verb | Signature | File | Owner |
|---|---|---|---|
| `ServerConfigBinding` | `(ctx) (ServerBinding, error)` **real**: the five settings | `serverconfig.go` | done |
| `BindServerConfig` | `(ctx, connectorID, repo, branch, path string, auto bool) (ServerBinding, error)` | `serverconfig.go` | S4 |
| `PlanServerConfig` | `(ctx) (ServerPlan, error)` repo plan, source "repo" | `serverconfig.go` | S4 |
| `PreviewServerConfig` | `(ctx, file []byte) (ServerConfigPlan, error)` | `serverconfig.go` | S4 |
| `PlanServerFile` | `(ctx, file []byte) (ServerPlan, error)` source "local", bytes in `File` | `serverconfig.go` | S4 |
| `ServerPlans` | `(ctx, limit int) ([]ServerPlan, error)` | `serverconfig.go` | S4 |
| `ServerPlan` | `(ctx, id string) (ServerPlan, error)` | `serverconfig.go` | S4 |
| `ApproveServerPlan` | `(ctx, id string, opts ApproveOpts) (Job, error)` | `serverconfig.go` | S4 |
| `RejectServerPlan` | `(ctx, id string) (ServerPlan, error)` | `serverconfig.go` | S4 |
| `ExportServerConfig` | `(ctx) ([]byte, error)` | `serverconfig.go` | S4 |
| `runServerPlan` | `(ctx, *jobs.Run, serverPlanJob) error` | `serverconfig.go` | S4 |
| `runServerApply` | `(ctx, *jobs.Run, serverApplyJob) error` | `serverconfig.go` | S4 |
| `RenameDomainResource` | `(ctx, id, host string) (DomainResource, error)` | `domainres.go` (end) | S5 |
| `ShareConnector` | `(ctx, id string, orgIDs []string, all bool) (ServerConnector, error)` | `connector.go` | S1 |
| `ServerConnectors` | `(ctx) ([]ServerConnector, error)` | `connector.go` | S1 |
| `BeginServerConnector` | `(ctx, userID, ghOrg string) (Connector, action, manifest string, err error)` | `connector.go` | S1 |
| `RenameServerConnector` | `(ctx, id, name string) (ServerConnector, error)` | `connector.go` | S1 |
| `DeleteServerConnector` | `(ctx, id string) error` | `connector.go` | S1 |

Every stub returns `notBuilt("...")` (`svc/notbuilt.go`): a Conflict, so the
API answers 409 and a job fails (never `errs.Unset`, which parks a job).
`serverconfig_test.go` has `TestServerConfigStubsRefuse`; each worker deletes
the lines of the verbs it builds. The last one out deletes `notbuilt.go`.

`flow/serverconfig/plan.go` (new package, **S3 adds `file.go`, `diff.go`,
`export.go` beside it and keeps `plan.go`**): `DefaultPath =
"stackr-server.yml"`, `type Change = planfile.Change`, `type Plan struct{
planfile.Plan }` with `Summary()` (add kinds: `org-create`, `route`, `dest`,
`domain`, `param`, `connector-share`; change them there if the kinds differ).

### 7. Jobs, payloads, locks (`svc/jobs.go`, `svc/serverconfig.go`)

- Kinds `kindServerPlan = "server-plan"`, `kindServerApply = "server-apply"`;
  handlers are wired to `runServerPlan` / `runServerApply`.
- Payloads (no `org_id`: server jobs list under `/admin/jobs` only):
  `serverPlanJob struct{}` (`{}`); `serverApplyJob{PlanID string
  "plan_id"}`.
- Lock keys: plan `"serverconfig"`. Apply `"serverconfig"`,
  `"serverplan:<plan id>"`, plus `"orgconfig:<org id>"` for each org of the
  file that **already exists** when the apply is approved (an org the apply
  creates has no id yet; it cannot be locked, and its first org plan is only
  queued after it exists). Queue with `o.enqueue(ctx, kindServerApply,
  serverApplyJob{PlanID: id}, locks...)` as `approveOrgPlan` does.
- `job.Supersedes` needs the same kind and the older lock set inside the
  newer: a later server-plan supersedes a queued one; two applies of
  different plans never supersede each other (the plan id differs); an apply
  and a plan queue behind each other (different kind, shared `serverconfig`).
- Walk order: server params, connector shares, settings and cascade rung,
  dests, domains (renames), routes, orgs, then ticked removals; connector
  shares before org bindings.

### 8. API (`api/routes.go`, `api/handler/v1/{serverconfig,serverconnector}.go`)

Handlers are written and wired to the verbs above, so S1, S4 and S5 change
service bodies only. All admin routes are org-less; a `:plan` / `:resource`
param on them is not org-checked (the verb is admin-level, as
`/admin/jobs/:job`). `plan` / `plan-file` / `plan-preview` answer 200.

| Method path | op | verb | body / out |
|---|---|---|---|
| `GET /admin/config-repo` | `admin.config-repo-get` | `admin.read` | out `ServerBinding` |
| `PUT /admin/config-repo` | `admin.config-repo` | `serverconfig.bind` | `{connector_id, repo, branch, path, auto}`; empty repo unbinds |
| `POST /admin/config/plan` | `admin.config-plan` | `serverconfig.bind` | out `ServerPlan` |
| `POST /admin/config/plan-file` | `admin.config-plan-file` | `serverconfig.bind` | `{file}` (2 MB cap); out `ServerPlan` |
| `POST /admin/config/plan-preview` | `admin.config-plan-preview` | `serverconfig.bind` | `{file}`; out `ServerConfigPlan` |
| `GET /admin/config/plans` | `admin.config-plans` | `admin.read` | newest 20 |
| `GET /admin/config/plans/:plan` | `admin.config-plan-get` | `admin.read` | |
| `POST /admin/config/plans/:plan/approve` | `admin.config-plan-approve` | `serverplan.approve` | optional `{ticked, confirm}`; 202 Job |
| `POST /admin/config/plans/:plan/reject` | `admin.config-plan-reject` | `serverplan.approve` | |
| `GET /admin/config/export` | `admin.config-export` | `admin.read` | `application/yaml` attachment `stackr-server.yml` |
| `POST /orgs/:org/domain-resources/:resource/rename` | `domain-resource.rename` | `domain.resource` | `{host}`; out `DomainResource` |
| `POST /admin/domain-resources/:resource/rename` | `admin.domain-resource-rename` | `serverdefaults.set` | same |
| `GET /admin/connectors` | `admin.connector-list` | `admin.read` | `[]ServerConnector` |
| `POST /admin/connectors` | `admin.connector-begin` | `connector.admin` | `{github_org}`; 201 `{connector, action, manifest}` |
| `PUT /admin/connectors/:connector/name` | `admin.connector-rename` | `connector.admin` | `{name}` |
| `PUT /admin/connectors/:connector/shares` | `admin.connector-share` | `connector.admin` | `{org_ids, all}` |
| `DELETE /admin/connectors/:connector` | `admin.connector-delete` | `connector.admin` | 204 |

The org approve route `POST /orgs/:org/config/plans/:plan/approve` takes the
same optional body. New authz verbs (`authz.go`, all `LevelAdmin`):
`serverconfig.bind`, `serverplan.approve`, `connector.admin`.
`TestServerConfigRouteContract` (`api/route_test.go`) pins this table.
`docs/openapi.json` was regenerated by W0 (`go run ./cmd/stackrd
--dump-openapi`), since `make lint` diffs it; wave 2 regenerates once more.

### 9. Knobs (`leaf/settings/catalogue.go`)

- Removed: `dns_env` (catalogue, `secretKnobs`, the form label; nothing read
  it). An old stored row is ignored.
- `root_domain` is no longer `ReadOnly`: `SetSetting("root_domain", ..)` now
  writes the row and nothing else. **S5** makes it rename the instance
  resource (and `SeedInstance` must not re-seed the old one at boot).
- New `Knob.ConfigOnly bool (json:"config_only")`. Five Flat knobs carry it:
  `server_config_connector`, `server_config_repo`, `server_config_branch`,
  `server_config_path` (strings, default ""), `server_config_auto` (bool,
  default false). `ServerSettings` (the Settings tab) skips them; the server
  file's schema must skip any knob with `ConfigOnly`; the Config tab reads
  them through `ServerConfigBinding` and writes them through
  `BindServerConfig`. The generic `PUT /admin/settings/:setting` can still
  write them: S4 may refuse that and point at the bind verb.

### 10. Files W0 left stubbed or patched in code the owners rewrite

- `svc/stack.go` `Webhook` (S1): a connector with `OrgID == nil` returns nil
  (no fan-out yet). Replace with: queue the server plan when the push matches
  the server binding, then fan out to every org it is shared with.
- `web/handler/canvas/create.go` `githubCallback` (S1): a server connector
  (nil `OrgID`) falls through to `/`; return to the admin Connectors tab.
- `leaf/connector/connector.go` (S1): `Get` refuses a nil `OrgID` as not found
  (org writes never reach a server connector); `Begin` takes the address of
  `orgID`. Read paths that widen (S1): `ListConnected`, the picker, clone,
  `SetOrgConfigRepo`, `orgLive.Connectors`.
- `api/handler/v1/serverconfig.go` `ExportServerConfig` carries two
  `//nolint:staticcheck` (SA4023: the stub's error is always set). **S4**
  drops them with the stub.
- `servicetest.Connector` seeds an org connector (`OrgID: &orgID`); S1 adds a
  server-connector seed helper.

### 11. Tests W0 added or changed

`db/serverconfig_test.go` (up/down/up with live rows, share cascades),
`store/store_test.go` (connector nil org and `ShareAll`, `ServerPlan` round
trip), `planfile/planfile_test.go`, `leaf/orgplan/orgplan_test.go` (ticks,
server machine), `flow/orgconfig/orgconfig_test.go` (stored plan JSON
unchanged, removal rows in the summary), `flow/serverconfig/plan_test.go`,
`svc/orgconfig_test.go` (`TestApproveOrgPlanContract`),
`svc/serverconfig_test.go` + `serverconfig_internal_test.go`,
`leaf/settings/flat_test.go` (`root_domain` settable, the config knobs),
`svc/serversettings_test.go` (rewritten on `protect_password`; `dns_env` is
gone), `web/handler/admin/admin_test.go` (`root_domain` is an input),
`api/orgconfig_test.go` (`TestApproveBody`), `api/route_test.go`.

### 12. The CLI coverage test

`TestEveryRouteHasAVerb` holds every op to a verb or a skip reason. W0 put
every new op in `cmd/stackr/pending.go` (merged into `skipped` at init) with
the owner's name. **Delete your lines when your verb lands.** Wave 2 deletes
the file. `cmd/stackr/orgconfig.go` `approve` sends no body (the API accepts
that); S4 adds `--remove <key>` and the risky confirm there.

### 13. Deviations from the blitz doc

1. `POST /domain-resources/:id/rename` became two routes (org and admin), as
   `PATCH /domain-resources/:resource` already is: `id` is not a route param
   the access middleware knows, and an org-less path cannot be owner-gated.
2. Added `GET /admin/config-repo` (the Config tab and `stackr server bind`
   show the binding) and admin connector `list`, `rename`, `delete` (the
   Connectors tab needs them; the blitz named create and share only).
3. `plan-file` and `plan-preview` take `{file}` JSON, like the org side.
4. W0 regenerated `docs/openapi.json` and `settings_form_templ.go` (the
   `dns_env` label) although the blitz reserves generation for wave 2:
   `make lint` and `make test` need both current.
5. `ApproveOrgPlan` and `planOrg`'s auto rule are S4's functions in the
   blitz; W0 edited them because the approve contract is a W0 item. S4
   extends the apply (ticked removals) and the CLI on top.
6. `flow/promote`'s `Change.Line()` moved to `planfile.Change`.

### 14. Per worker, first things

- **S1**: store methods for `connector_shares` and server connectors; replace
  `connector.go` stubs; the connector leaf's read widening; `Webhook` fan-out;
  `serverFile` clone helper in `wiring.go`; delete the `admin.connector-*`
  lines from `pending.go` (or write verbs). Org writes must keep failing for a
  nil `OrgID`.
- **S2**: nothing in the contract; `${{ server.params.<col>.<name> }}` needs
  the `server` scope in `leaf/params` and `ref.go`. The server file's secrets
  are by name; S3's `Live` carries which exist.
- **S3**: `file.go`, `diff.go`, `export.go` in `flow/serverconfig`. Use
  `planfile.Plan.Block/Add`, `Change.Impact`, `Optional` + `Key`. Skip knobs
  with `ConfigOnly`. In `flow/orgconfig/diff.go` the `share-delete` row
  becomes `Optional: true, Key: "share:<slug>"` (kind unchanged; `walkOrgPlan`
  needs no change, the apply filters by `OnlyTicked`).
- **S4**: replace `serverconfig.go` stubs; extend `applyOrgPlan`/`walkOrgPlan`
  as needed (the filter is already in); the CLI.
- **S5**: `RenameDomainResource` in `domainres.go`, the `root_domain` branch of
  `SetSetting`; counts helper for the impact line.
- **S6**: `proxy_custom` and the panel host trace; its findings go below under
  an `S6` heading.
- **S7**: the web side posts `ticked` and `confirm` (see section 5); the
  plan component reads `Change.Impact`, `Optional`, `Key` from the stored plan
  JSON (`impact`, `optional`, `key`) and `ServerPlan.Source` for "from a
  local file, not the repo".

### 15. For wave 2

- `notbuilt.go`, `pending.go`, `TestServerConfigStubsRefuse`: delete once empty.
- `templ generate` and the openapi regen once more.
- `docs/rewrite/tasks/serverconfig.md`: "Param refs (`leaf/params/ref.go`) know
  `org.params` only" is S2's.

## S4

Landed: every server verb (`svc/serverconfig.go`: bind, plan, preview, plan a
local file, list, get, approve, reject, export, the two jobs; the live
gathering and the apply walk in `svc/serverconfig_apply.go`), the CLI
(`cmd/stackr/serverconfig.go`: `stackr server bind|plan|preview|plans|
plan-show|approve|apply|reject|export`; `stackr org approve` takes `--remove`
and the risky confirm), and the API handler change below. `notbuilt.go`,
`pending.go` and the stub test are deleted (all empty by now).

**Approver.** `ApproveOpts` gained `ApproverID string` (`json:"-"`,
`approve.go`) and the server-apply payload `approver_id` (omitted for an
auto-apply). The apply creates the orgs the file names with this user as owner
and refuses that step without one. The API server approve handler sets it from
the session (`who(c)`). **S7**: the web handler that calls `ApproveServerPlan`
must set `ApproverID` to the session user, or an org-create step ends the plan
in error ("no approver to own the new org").

**Locks.** The apply holds `serverconfig`, `serverplan:<id>` and
`orgconfig:<id>` for each existing org named by a plan row whose `Kind` starts
`org` with the slug in `Tile` (S3's `org-create`, `org-bind`, `org-unbind` do).

**Webhook.** `queueServerPlan(ctx, connectorID, repo, branch, defaultBranch)
(Job, error)` queues the `server-plan` job when a push matches the binding;
S1's `serverWebhook` already calls it.

**Export writes.** `ExportServerConfig` first stores the keys of each global S3
destination as server params (`serverconfig.DestParamCollection`/`DestKeys`)
where none is stored yet, an existing param is never touched, so the export
plans clean on the same box. A GET on `/admin/config/export` therefore writes
(only the first time, only missing params).

**Bind** stores the repo as `githubapp.RepoURL(repo)` (like an org binding) and
requires a server connector (`conns.Server`); an empty repo unbinds and rejects
the undecided plans. The generic `PUT /admin/settings/:setting` can still write
the `server_config_*` keys without that check (not refused; the Config tab and
CLI use bind).

**Deviations / ponytails.**
1. `backup.Dest` has no archive key, so a new destination whose file gives
   `archive_key` has it written over the random one straight through the store
   (`serverWalk.createDest`). Give the leaf the field if a second caller needs it.
2. The service has no route update, so `route-update` deletes then creates.
3. A cascade change (`defaults:`) writes the whole rung from the file; a field
   the block omits is cleared.
4. `panel_backup_dest` is applied after the destinations step (the file names a
   destination, the setting stores its id).
5. **proxy_custom, still open for wave 2.** The server-file apply takes a value
   Caddy cannot load back (restores the old row, syncs, fails the step). The web
   form and `PUT /admin/settings/:setting` still store it and every later push
   fails until it is fixed. The real fix is `admin.go` `SetSettings` (not S4's):
   on a sync failure with `proxy_custom` in `vals`, restore the old value and
   sync again.
6. **`server_config_*` through the generic settings PUT** is not refused; it
   skips bind's connector check. Refuse it in `SetSettings` and point at bind
   if that matters.
7. **Shared files touched:** `cmd/stackr/cmds.go` (one line in `commands()`:
   `a.serverConfig()`), `approve.go` (`ApproverID`), `share_test.go`
   (`TestOrgFileShares`), `cmd/stackr/orgconfig.go` (approve, and its export
   closure became `app.export` in `serverconfig.go`).
8. `share_test.go` `TestOrgFileShares` changed: a share the file drops is a
   removal row now, so the plan no longer auto-applies and the test approves
   it with the tick.

## S6 (panel domain trace and proxy_custom)

Landed: `proxy_custom` is wired. `domain.Install.Custom` (set from the
setting in `wiring.go` `proxyConfig`) is a JSON array of Caddy HTTP route
objects, appended after every route of ours on the main server: the https
server, or the http one with TLS off. Additive, so the panel vhost and the
tiles stay first. Its `match[].host` names join the certificate policy
rules (domain resource accounts, DNS-01). Bad JSON at save is refused by
`settings.Check` (`leaf/settings/flat.go` `validate`, shape only, Invalid).
`Build` still guards itself: a bad value is left out and named in the error,
like a broken tile, so the rest still pushes. The Caddy tab copy no longer
says "not read". `flow/deploy/proxy.go` needed no change (`Install` passes
through). Tests: `leaf/domain/caddy_custom_test.go`, `TestProxyCustomChecked`
(`leaf/settings/flat_test.go`), `service/proxycustom_test.go`,
`TestAdminCaddyCustom`.

Deviation from the blitz: validation sits in `leaf/settings/flat.go`
`validate` (+ `flat_test.go`), files S6 does not own. It is the one path that
covers the web form, `PUT /admin/settings/:setting` and the server-file
apply; a check in `handler.go` would cover only the form.

Needs for others:
- **MUST FIX BEFORE SHIP (S4 / wave 2)**: `proxy_custom` was inert; now a
  route that is valid JSON but that Caddy cannot load is stored and then
  fails every later push, tile deploys included, until fixed. Details next.
- **S3 / S4 (server file)**: `proxy_custom` is a plain Flat knob; the impact
  line "replaces the extra Caddy routes" is right. Caddy checks a route only
  at the push, so a route it cannot load fails `SetSettings` **after** the
  row is written (`SetSettings` writes, then syncs). The step fails and
  reports the Caddy error, but the bad value stays stored and every later
  push (tile changes included) fails until it is fixed. Cheap fix, in
  `internal/service/admin.go` `SetSettings` (not S6's file): when the sync
  fails and `proxy_custom` was in `vals`, restore the old value and sync
  again. S4 may do it in the apply step instead.
- **Null and empty**: `Set("proxy_custom", "")` deletes the row (no custom
  routes); `[]` is stored and means the same.

### Panel host trace (for S3's impact line)

The panel's address lives in two places that do not move together:
`panel_domain` (a setting: a row wins over the `PANEL_DOMAIN` env, which only
seeds it) and `BASE_URL` (installer-set env, read once at boot, not
changeable from the file). A change of `panel_domain` moves only the first.

Consumers of `panel_domain`: `wiring.go` `proxyConfig` (the panel vhost,
`domain.Build`); `route.go` `checkPanelHost` (refuses a pass-through route
over it) and `checkPanelRoutes` (called from `SetSettings` for the reverse).
Nothing else reads it.

Consumers of `BASE_URL` (all stay on the OLD host after a `panel_domain`
change until the panel container restarts with a new env, and the installer
spec `installspec.Load` rebuilds the env from its saved answers on upgrade):
1. **Session cookie** (`cmd/stackrd/main.go` `CookieDomain: baseDomain`,
   `service/orchestrator.go` `WithCookieDomain`). The cookie is set with
   `Domain=<old host>`. On a new host that is not under the old one the
   browser drops it: **login on the new host fails and a live session is not
   sent there**. The web UI is then locked out on the new host. Cheap fix
   (not S6's files): `CookieDomain: ""` in `cmd/stackrd/main.go` makes the
   cookie host-only and works on any host; it also stops sending the session
   cookie to every tile under the panel host. **Recommend, wave 2.**
2. **GitHub App** (`githubapp.New(cfg.BaseURL)` in `service/orchestrator.go`;
   `githubapp.go` `Manifest`): an App made after the change still registers
   the old `url`, `redirect_url` (`/settings/github/callback`) and webhook
   (`/hooks/connectors/<id>`), so creating a connector sends the browser to
   a dead host. **Apps made before** keep the webhook URL they were created
   with on GitHub: pushes go to the old host, find no route, and plans stop
   queuing, silently. Stackr cannot repair this today (it would need
   `PATCH /app/hook/config` with the App JWT). **S1** (owns the connector
   leaf and `Begin`): cheap fix is to give `githubapp.Client` a
   `func() string` that reads `panel_domain` (with the scheme and port from
   `BASE_URL`) instead of the fixed string. The webhook URL on existing Apps
   goes in the impact line.
3. **Invite links and the setup prefill** (`ui/components/static.go`
   `AbsoluteURL`, `canvas/org.go`, `setup/handler.go`): built from
   `components.BaseURL`, so a new invite points at the old host. Cosmetic
   once the old host is gone; same cheap fix as above (read the setting).
4. **CORS** (`AllowOrigins: baseOrigin`): only cross-origin callers; the
   panel is same-origin, so no break.
5. **CSRF**: hamr's double-submit cookie (`web/server.go`, `APICSRF`), host
   only, no Origin or Referer check. Not affected.
6. **CLI authorize page** (`web/handler/auth/cliauth`): host-agnostic (relative
   paths, loopback redirect). But every CLI profile stores the URL it logged
   in to (`cmd/stackr/app.go` `Server`), so **every CLI and API key client
   keeps calling the old host** and fails; each user logs in again against
   the new one. The same goes for scripts and the `server` field of CI.
7. **Installer**: the panel container's `BASE_URL` and `PANEL_DOMAIN` come from
   the saved install spec; a panel upgrade restores the old `BASE_URL`.
   `PANEL_DOMAIN` is only a seed, so the row survives.

Lockout answer for the impact line:
- The proxy drops the old panel vhost in the same push that adds the new
  one. Certificates are issued later, on demand. If DNS does not yet point
  the new host at the box, **the panel is unreachable on both hosts** until
  it does. Web login on the new host also fails on the cookie domain above.
- An admin is never locked out of the box: the panel still listens on its
  bind address (`installspec.Input.Bind`, the Docker gateway, port 8080,
  not public). From the box, an admin API key reverts it: `PUT
  /api/v1/admin/settings/panel_domain` against `http://<bind>:8080` with an
  empty value (back to the installer's host). A bearer key skips the CSRF
  check (`middleware.APICSRF`) and nothing checks the Host header, so web
  login is not needed. No CLI shortcut for the box-local call exists.
- Setting `panel_domain` to `localhost` or a bare IP drops the panel vhost
  entirely (`domain.Build` skips them): the panel is unreachable through the
  proxy. Nothing stops this; `settings.Check` has no host rule for the knob.
  S3 can refuse those in the file diff as a blocker. An empty value is the
  revert path, not a drop: `Set(key, "")` deletes the row and `Get` falls back
  to the `PANEL_DOMAIN` env (the installer's host). (Empty drops the vhost
  only when that env was empty too, as in dev.)
- **Shadowing**: the panel vhost sorts first, so a tile domain, a domain
  resource or an https external route on the panel host is silently dead.
  `checkPanelRoutes` only covers pass-through routes, and `checkSquat`
  (`service/domain.go`) does not look at the panel host. Cheap fix in
  `checkSquat` and the `panel_domain` branch of `SetSettings` (not S6's):
  refuse a tile domain on the panel host and a `panel_domain` a domain row
  already holds. S3 can add the second as a diff blocker.

Suggested impact line for `panel_domain`: "panel moves to X and stops
answering on Y at once; point DNS at this box first. The web login on X
needs the cookie fix, CLI keys and GitHub webhooks still point at Y."
(Drop the clauses the cheap fixes remove.)

## S5 (domain resource rename)

Landed in `service/domainres.go` (the verbs and helpers), `leaf/domainres`
(`CheckRename`, `Rename`, `Rehost`), `SetSettings` in `service/admin.go` (a
`root_domain` check in the check loop and a rename in the write loop; the
other keys are untouched), `flow/promote/plan.go` (one `continue` in
`planDomains`' removal loop), `service/domain.go` (`refreshAutoHosts` skips
generated redirects), `stackr domain rename`, the org drawer rename form.

- **For S3/S4, the impact line.** `Orchestrator.DomainResourceImpact(ctx, id)
  (RenameImpact{Resources, Hosts, Certs}, error)`. `Hosts` are the tile
  domains that get rewritten (redirects and literal hosts excluded), `Certs`
  the HTTPS ones among them, `Resources` the rows that move (the instance
  row takes the undeclared `<org slug>.<root>` org rows with it). For a
  `root_domain` change, look up the instance row whose host is the old root
  (`AllDomainResources`, `Level == "instance"`) and call it with that id.
  "Past ~50 without wildcard" is `Hosts` against `Wildcard(ctx)`.
- **`domains: from:` (S3's file field) is `RenameDomainResource(ctx, id,
  host)`** with the id of the resource whose host is `from`. It checks
  everything before the first write, pushes the proxy once, and refuses a
  stack-level resource (the stack file owns those). A rename onto a host
  another resource holds is a Conflict, so the file apply can retry safely
  after a partial run only once the first rename landed (`from` no longer
  matches; treat "no resource with host `from`, one with the new host" as done).
- **`SetSettings({"root_domain": v})` renames the instance row** whose host
  is the old root, moves the cascaded org rows, and keeps `root_domain` in
  step both ways (`RenameDomainResource` on the instance row writes the
  setting). With no instance row it seeds one. `""` over a live root is
  refused. The server file's `settings:` can carry `root_domain` as it is;
  the impact line is S3's.
- **The redirect marker is `Auto && RedirectTo != ""`** (no migration). The
  stack file grammar forbids `auto`/`apex` together with `redirect_to`, so
  no file row looks like one. Promote's `planDomains` no longer lists a
  generated redirect as a removal; a redirect the file declared still goes
  when the file stops naming it. Env sync never touches domains.
- **S6 or wave 2: the pushed redirect is a 301, not the design's 308.**
  `leaf/domain/caddy.go` `domainRoute` hardcodes `301` for every `redirect_to`
  row (`forceRoute` is the 308). One-line change if the design wants 308 for
  generated redirects; a 301 is cached by browsers for good, so a rename back
  needs a cache clear. S6 owns that file.
- **Deferred, not done:** a managed tile's published URL (`S3_ENDPOINT` and
  its kin, `publicBase`) keeps the old host until the stack's next reconcile
  (the existing `ponytail:` on `publicBase`); an S3 client behind a redirect
  may not follow it. Declared org rows (`acme.corp`) and stack rows never
  follow a root rename: they are the owner's own names.

## S2 (server params scope)

Landed. What other workers and wave 2 need:

- **Migration `006_serverparams.{up,down}`** (005 stays frozen). `params.scope_kind`
  gains `'server'` (a CHECK cannot be widened, so the table is rebuilt; the three
  scope triggers are dropped and recreated around it). The server scope is
  `scope_id = "server"`. Down drops the server rows. Tested up, down, up on live
  rows in `db/params_server_test.go`. **Any later migration is 007.**
- **Scope:** `params.ServerScope` (leaf) and `service.ServerParamScope`. Admin
  only: `PATCH/GET /admin/params`, `GET /admin/params/secrets`,
  `DELETE /admin/params/:collection/:name`, ops `admin.params`, `admin.secrets`,
  `admin.params-set`, `admin.param-delete`. The masked list takes `admin.read`;
  secrets, set and delete take the new verb `serverparams.write` (LevelAdmin).
  `api/routes.go` `scoped()` got a read/write verb per scope and a `volumes`
  switch (the server scope has none). No tile redeploys on a server param change.
- **Resolver:** `${{ server.params.<col>.<name> }}` is `params.KindServerParam`,
  resolved only for `params.InServerFile` with `Snapshot.ServerParams`. Every
  other `Where` refuses it with "server params are readable only in the server
  file (stackr-server.yml), never in <where>"; a stack file refuses it at load
  (`promote/stackfile.go` `checkRefs`); a share user or password already refuses
  anything but `org.params` (`leaf/volume/share.go`, test rows added). A missing
  value is `errs.Unset{Param: "server.params.<col>.<name>"}`.
- **For S3 (pure diff):** `params.ServerRefs(s) ([]string, error)` returns the
  `collection.name` keys a server-file item refs, and errors on any other ref
  form or a malformed one. Check each key against `Live`; a missing one is
  "backup dest offsite needs server.params.s3.secret_key" and blocks only that
  item. `Live` can take the set from `o.MaskedParams(ctx, ServerParamScope)`
  (a secret row exists only once it has a value; a declared secret is not
  stored, so it reads as missing, which is what the blocker wants).
- **For S4 (apply):** `o.ExpandServerRefs(ctx, s)` resolves one item's refs
  (dest `secret_key`, `access_key`, `archive_key`, later a connector token); a
  missing value comes back as `errs.Unset`, so catch it before the apply
  (the plan already blocked it) or the job parks instead of failing. The
  file's `params:` block merges with `o.SetParams(ctx, ServerParamScope, ...)`
  (a declared secret with no value keeps the stored one, as everywhere).
- **CLI:** `stackr params ... --level server` (path `/admin`); `rm` words its
  confirm for the server file. The spec said `--scope`; the CLI's flag is
  `--level`, so it is `--level server`.
- **Web:** Params tab on the admin drawer (`web/handler/admin/params.go`, the
  shared `vars.Editor` with a new `View.Lead` line; the form parser moved to
  `web/render/params.go` `ParamEntries`, canvas uses it too). `Tabs` gained
  `"params"`.
- **Wave 2:** `docs/openapi.json` needs the four `admin.*` params ops (the CLI
  coverage test reads the file; S2 regenerated it by tool for its own run, wave
  2 regenerates again). `serverconfig.md` last bullet ("Param refs know
  `org.params` only") is done; AGENTS.md config-as-code section should say
  server params exist and are readable by the server file only.
- **Not done, on purpose:** a tile param value or an org file field that holds
  `${{ server.params.. }}` is refused at deploy (the one choke point), not at
  save: no leaf may import `leaf/params`. The stack file is the one plan-time
  check, because it already parses refs.

## S1 (server connectors and sharing)

Landed. No schema change (005 holds). Paths under `internal/service/`.

**Contract for the others**
- `leaf/connector`: reads widen, writes do not. `Get` / `Rename` / `Delete`
  stay org-only (a nil `OrgID` row is not found). New: `Usable(org, id)` (own
  or shared, for reads), `Server(id)`, `ListServer`, `SharedOrgs`,
  `SetShares`, `BeginServer`, `RenameServer`, `DeleteServer`,
  `ServerInstallURL`. `ListConnected(org)` is the org's own connected rows
  first, then the shared server ones; `List` is still the org's own only.
- `connector.Resolve(conns, named, host)` is the pure resolution rule (named
  wins, then the org's own for the host, then exactly one shared server
  connector). `NoConnector` for none, new `Ambiguous{Host}` ("several shared
  git connectors serve X; name the connector to use") for several. `For` is
  `ListConnected` + `Resolve`.
- `wiring.go` `serverFile(ctx, commit string, log io.Writer) (data []byte,
  sha string, err error)`: the bound server repo at commit, read through the
  binding's server connector (`conns.Server`, never the org-side lookup;
  `o.gitEnv` still short-circuits for tests). Dir `repos/serverconfig`. A
  binding with no repo or no connector is a Conflict. `cloneEnv` now uses
  `Usable`, so an org file, a stack file and a tile build clone through a
  shared server connector.
- `connector.go`: `ServerConnectors`, `ServerConnectorInstallURL`,
  `BeginServerConnector`, `RenameServerConnector`, `DeleteServerConnector`,
  `ShareConnector` are real. `ShareConnector` takes org **ids** (the server
  file names slugs: S4 maps them via `AllOrgs`). Unknown id is Invalid
  `org_ids`; revoking is a Conflict while an org's config file or a stack in
  that org names the connector; delete is a Conflict while the server file,
  an org or a stack names it. `connectorBindings(ctx, id)` (unexported) lists
  every binding; S4 can reuse it for the plan blockers of a `connector-share`
  removal row and for `BindServerConfig`.
- `Webhook` on a server connector calls S4's `queueServerPlan`, then fans out
  to every org it is shared with (`orgWebhook`: org plan plus stacks that use
  the repo); share-all fans out to every org. Org connectors behave as before.
- `servicetest.Env.ServerConnector(t, name, secret)` seeds a connected server
  connector shared with none.
- Web: admin drawer `connectors` tab (`ui/drawer/admin/connectors.templ`,
  `web/handler/admin/connectors.go`, tab key added to `ui.Tabs`); the org
  Config tab picker lists shared server connectors marked ", server"; the
  GitHub callback of a server connector returns to
  `/?drawer=admin&tab=connectors`.
- CLI: `stackr server connectors ls|rename|share|rm` (`cmd/stackr/
  serverconnector.go`, registered with one added argument in `serverConfig()`
  in S4's `serverconfig.go`). `admin.connector-begin` is a skip (browser
  handshake). My lines are gone from `pending.go`, which is now an empty map:
  wave 2 deletes the file.
- `TestServerConfigStubsRefuse`: the `ShareConnector` / `ServerConnectors`
  lines are gone.

**Needs elsewhere**
1. **S3 / wave 2, `flow/orgconfig/diff.go` `connectorOf`**: it takes the first
   connector whose host matches, which is right only because `orgLive` lists
   the org's own first. Two shared server connectors on one host and none of
   the org's own plan clean, then the apply fails in `For` with `Ambiguous`.
   Call `connector.Resolve(live.Connectors, b.ConnectorID,
   connector.Host(b.Repo))` and turn `Ambiguous` into a blocker ("name the
   connector to use"); the diff already imports the leaf.
2. **`stack.go` `SetConfigRepo` and the stack picker** (not mine, one token):
   it checks with `o.conns.Get`, so a stack cannot bind to a shared server
   connector. This bites the org file: its `stacks.<s>.connector: <server id>`
   plans clean (`orgLive.Connectors` lists the shared one) and the apply then
   fails "not found" in `orgconfig.go` line ~449, which calls this verb.
   Change the check to `o.conns.Usable(ctx, st.OrgID, connectorID)`; switch
   the stack picker (`canvas/stack.go`) to `ConnectedConnectors` if the UI
   should offer it. Cloning already accepts it.
3. **S6's finding, not done**: the GitHub App manifest registers the fixed
   `BASE_URL` (callback, webhook, url). Making `githubapp.Client` read
   `panel_domain` is in `orchestrator.go` / `githubapp`, not my files.
4. **`GET /orgs/:org/connectors`** (`connector.list`) still lists the org's own
   only, by design (the drawers open a card per row and write through it). An
   API or CLI reader that wants the usable set has no route; add one only if
   S7 or a script needs it.
5. **Pre-existing failure, not S1**: `TestOrgFileShares` (`share_test.go`)
   times out waiting for a share the file dropped to be deleted. S3 made that
   an unticked removal row; the test must tick `share:docs` on approve.
   `TestAdminConfigBind` was failing while `PlanServerConfig` was a stub.

## S7 (web: Config tab and plan review)

Landed. Org and server plans share one review component; the admin drawer has
a `config` tab.

**What landed outside the files S7 owns, and why**
- `internal/ui/components/plan.templ`: the v0 `Plan` stays; `ChangeView` gains
  `Impact`, `Key`, `Optional`, `PlanView` gains `Ticks`. New `PlanReview`
  (removal ticks + Approve + Reject in one `<form>`, so the approve posts
  `ticked`; with impact lines the Approve is a `Confirm` dialog whose yes posts
  `confirm=1`), `PlanLine`, `PlanRow`. `PlanRow` and `planTone` moved here from
  `ui/drawer/org/org.templ` (the admin tab needs both).
- `internal/web/render/render.go` `OrgPlanView` now copies Impact/Key/Optional;
  new `internal/web/render/plan.go` holds `ServerPlanView` and `PlanOrigin`.
  `render` is where the org converter already lived; the web cannot import
  `planfile`, so the converter ranges over the service types.
- New files: `ui/drawer/admin/config.templ`, `web/handler/admin/config.go`
  (`mountConfig`, `configTab`). One line each in `handler.go` (`h.mountConfig`,
  `case "config"`) and the `config` key in `ui.Tabs`.

**Needs elsewhere**
1. **S3 / wave 2**: `render.ServerPlanView` turns a row into a "+ create" when
   its Kind is `org-create`, `route`, `dest`, `domain`, `param` or
   `connector-share` and it is not a removal row. That is the add list in
   `flow/serverconfig/plan.go` `Summary()`; if S3 renames kinds, change both.
2. **Wave 2**: `approveOpts` exists twice (`canvas/orgconfig.go`,
   `admin/config.go`; the admin one also sets `ApproverID` from the session,
   S4's note). Fold into `render` if wanted.
3. **S1**: the empty-state copy says "Connectors tab"; it matches S1's
   `connectors` key.
4. **Org Config tab**: the auto label now reads "Auto apply a plan with no
   impact lines and no removals" (the `AutoOK` rule).
5. **Tests**: `TestAdminConfigTab` and `TestAdminConfigBind` run against S4's real verbs and S1's `servicetest` server connector; no skip.

## S3 (flow/serverconfig, and org removals)

Landed: `flow/serverconfig/{file,diff,export}.go` (+ `plan.go` unchanged) and
`serverconfig_test.go`; `flow/orgconfig/diff.go` only the shares block.

**API (all pure).** `Parse([]byte) (*File, error)`, `Diff(*File, Live) Plan`,
`Export(Live) ([]byte, error)`. Helpers S4 needs: `DestParamCollection(name)`
(`backup_<name>`, lower-cased, non-alphanumerics become `_`), `DestKeys`
(`access_key`, `secret_key`, `archive_key`), `ServerRef(v) (key, ok)` (one
`${{ server.params.<col>.<name> }}` and nothing else, through S2's
`params.ServerRefs`), `OrgFilePath`. `Parse` normalises hosts, repos, route
targets and setting values in place; `File.Settings` is `map[string]any` but
every value is a string after `Parse`.

### `Live` (S4 gathers it; every field is a store fact)

| Field | What |
|---|---|
| `Settings map[string]string` | effective value (row, boot, default) of every Flat knob that is not `ConfigOnly`. `panel_backup_dest` holds the dest **id**. A missing key reads as the catalogue default. |
| `Defaults settings.Settings` | the server rung |
| `Tiles int` | tiles a cascade change redeploys (the impact line says "redeploys N tiles"; 0 means no impact line unless protect changes) |
| `Params map[string]params.Value` | server scope, keyed `collection.name`, **secrets decrypted**: `params.Values(ctx, ServerScope, true)`, **not** `MaskedParams` (masked secrets have `V == ""`, so every dest would block, no key change would show and every declared secret would note "not set"). A declared secret with no value is absent or `V == ""`: both read as not set. |
| `Routes []store.Route` | every external route |
| `Dests []store.BackupDest` | **global only** (`OrgID == nil`), local included, keys filled |
| `DestSchedules map[string]int` | dest id -> volume schedules naming it |
| `Domains []store.DomainResource` | every resource on the server (the file owns the instance ones; the rest are for clashes) |
| `DomainHosts map[string]int` | resource id -> tile domains named under it (S5's `DomainResourceImpact(...).Hosts`) |
| `Taken []string` | every host tile domains and domain resources hold (`domainHosts`) |
| `Connectors []ConnectorLive` | **server** connectors: `Connector store.Connector`, `OrgIDs` (shared with), `Bound` (org ids with a binding, org or stack, that names it) |
| `Orgs []store.Org`, `Claims []org.Claim` | every org; every domain with its org (the new-org slug check, as `orgLive`) |
| `TLSOff bool` | `STACKR_TLS=off` |

### Plan rows (what S4 walks and S7 shows)

Order is apply order, removal rows last. `Kind`, `Tile`, `Field`:

- `param`, `param-update` (Field `collection.name`); never a removal.
- `connector-share` (Tile connector name; New = org slug, or Field `share`
  New `all` with an Impact), `connector-share-delete` (removal).
- `settings` (Field = knob key; Old/New text; `panel_backup_dest` rows carry
  dest **names**, `local`/empty = local), `defaults` (Field = cascade field,
  `protect_password` never shows a value; the first row of a changed rung
  carries the one Impact line).
- `dest` (add, Tile = name), `dest-update` (Field; `access_key`/`secret_key`
  rows say only "changed"), `dest-delete` (removal).
- `domain` (add, New = host), `domain-update`, `domain-rename` (Tile = new
  host, Old = from), `domain-delete` (removal).
- `route` (add), `route-update` (Field mode/target/insecure), `route-delete`.
- `org-create` (Tile slug, New name, Impact always), `org-bind` (Field
  repo/branch/path/connector/auto, **connector as the server connector's
  name**), `org-unbind` (removal). All three start with `org` and carry the
  slug in `Tile`, as S4 asked.
- Removal keys: `route:<host>`, `dest:<name>`, `domain:<host>`,
  `connector-share:<conn>/<org slug>`, `connector-share:<conn>/all` (the
  `all` -> list case, new, outside the W0 vocabulary), `org-binding:<slug>`.

Plan `Summary()` add kinds in `plan.go` already match (`org-create`, `route`,
`dest`, `domain`, `param`, `connector-share`); S7's `render.ServerPlanView`
reads the same list.

### What S4 must know when it applies

1. **Walk by `OnlyTicked`, and read the File, not the rows.** Rows are for
   display and ticks; the apply re-parses and re-diffs, as `applyOrgPlan`
   does, and acts on `f`.
2. **A removal the verb would refuse gets a note, never a row and never a
   blocker** (a dest a schedule or the panel backup uses, an instance domain
   with tiles under it, a connector share a binding names; the org file's
   mounted share too). A blocker would make the whole plan unapprovable.
   S7: show `Plan.Notes` beside the blockers; they are the "stays" lines.
   Decision taken, deviating from the blitz's "blockers reuse each verb's
   refusal".
3. **`panel_backup_dest` is a name in the file and an id in the store.**
   Apply it **after** the dests step (the file may create the dest it
   names); the blitz walk order puts settings first.
4. **Routes have no update verb**: `route-update` rows are delete plus create
   (one proxy push). The target is stored `host:port`; `NormTarget` is the
   same normalisation as `leaf/route`.
5. **`backup.Create` always mints a fresh archive key.** A file-given
   `archive_key` (the DR path: same key, old archives decrypt) cannot be
   applied until `backup.Dest` takes an optional `ArchiveKey` on create. On an
   existing dest a different key is a blocker.
6. **Export needs the dest key params.** `Export` writes
   `${{ server.params.backup_<name>.<key> }}` for each key and declares the
   three as secrets, so the file parses; the plan then blocks on any value
   not set. `ExportServerConfig` should seed those server params from the dest
   rows first (use `DestParamCollection`/`DestKeys`), and `serverLive` must
   read them back (decrypted, see `Params` above) so export then plan reads
   clean.
7. **`root_domain` is one rename.** A `settings.root_domain` change renames
   the instance resource itself (S5 in `SetSettings`); a `domains:` entry for
   the new root is that resource, not a second one, so the plan holds one
   `settings` row with the one rename Impact and no `domain-rename`,
   `domain` or `domain-delete` for it. Only an explicit `from:` that is not
   the root rename emits `domain-rename` (apply: `RenameDomainResource` by the
   `from` host's id; "no `from`, one with the new host" is done).
   `root_domain` cannot be cleared (S5's verb refuses it): a blocker.
8. **A share to an org the same file creates** is a `connector-share` row
   that sorts before `org-create`. Apply must create the orgs first, or run
   those shares after `org-create`. The org entry's `connector:` is checked
   against live shares plus the file's list, not live alone.
9. **New org:** one `org-create` row (the binding is in the Note); apply
   creates it with the approver as owner, then `SetOrgConfigRepo` with the
   resolved connector id and `RepoURL` as `Parse` normalised it. `name:` only
   applies at creation; an existing org with another name gets a Note (the
   org file renames), so the two files do not fight. A new org's name must
   make its slug (`slug.Make(name) == slug`), else a blocker.
10. **Panel domain.** Blockers: a pass-through route over it, an exact-host
    route on it ("shadowed"), a tile domain or resource already on it
    (`Taken`). The route check looks only at routes the file names, plus all
    routes when the panel domain moves, so a live route the file never
    touches does not block every plan. `CheckRoot` already refuses an IP or
    `localhost`. The impact line is S6's suggestion: moves, stops answering
    on the old host at once, CLI logins, API keys and GitHub webhooks still
    point at the old one. If the cookie fix (`CookieDomain: ""`) does not
    land, add "web login on the new host fails until the panel restarts"
    (S6's trace item 1).
11. **Not on the plan, by design:** orgs and params are never removal rows
    (only an org's *binding* is); a bound org the `orgs:` block does not name
    is an `org-unbind` row, which is noisy for hand-bound orgs once a file
    has an `orgs:` block. Export lists only bound orgs, and writes every knob
    not at its default (boot values included).

### Wave 2

- `leaf/route` could export a `Check(Spec, tlsOn, dns01)` so `Diff` blocks
  exactly what the create would refuse; `serverconfig.NormTarget` and the
  route mode/wildcard/TLS checks in `diff.go` are a copy (`ponytail:`).
- `planfile` should take the `params` diff and the `Defaults` struct: now
  three copies (stack file, org file, server file).
- `flow/orgconfig`: the share removal is `Optional` with `Key share:<slug>`;
  a share a tile mounts is a note (`shares.<slug> stays: ...`), not a
  blocker (test `TestDiffShares`).
- `defaults.protect_password` exports as plain text when it is not a
  `${{ }}` ref, same as the org file: the exported file goes in git. Decide
  whether export drops a literal password.

## Fix round needs

- F1 (UI, `internal/ui/pages/setup/setup.templ`): the connector and config steps
  render "Install it on GitHub" with `v.InstallURL` as the href. An org whose
  only connector is a shared server one now gets an empty `InstallURL` (the
  admin installs a server App), so when that App has no repos the button is a
  dead link. Hide the button (or say "ask a server admin") when `InstallURL`
  is empty, and drop the empty-href `nav` under Continue. Pages render now
  (test `TestSharedServerConnectorOnly`).
- F1 (`internal/service/wiring.go` `buildTile`, one call line): the tile clone
  now passes `o.tileConnector(ctx, st, t.GitURL)` as its connector id. It is
  the stack's `ConfigConnectorID` only when several shared server connectors
  serve the host and that one is connected and serves it; else "".
  `connector.Ambiguous` now carries `Names` (the message lists them).
  Promote's plan still does not resolve connectors; a tile with no stack
  config connector and an ambiguous host still fails at build, now naming
  the candidates.
- F1 (store): `ConnectorStore` gained `SetShareAll`, `SetName`, `SetApp`
  (column writes); the leaf's `SetShares`, rename and `Complete` use them.
  Nothing else wrote a connector row whole.

- **F5 to F1 / verifier:** `internal/web/handler/setup/handler.go` (prefill
  and invite, around lines 229 and 286) still builds links with
  `comp.AbsoluteURL`, which uses BASE_URL's host. F5 added
  `(*handler).panelURL(ctx, path)` in `web/handler/canvas/org.go` (BASE_URL's
  scheme and port, the `panel_domain` setting's host; BASE_URL unset keeps the
  bare path). It is private to `canvas`; F1 needs the same few lines in
  `setup` (or lift it to `ui/components` next to `AbsoluteURL` with the
  host as an argument).

- F4 (`internal/service/internal/leaf/domain/caddy.go` `domainRoute`, F5's file):
  a generated redirect's 308 `Location` is always `https://<new host>`, even
  when the moved row is HTTP-only (`HTTPS` false). F4 now copies `HTTPS` and
  `ForceHTTPS` from the moved row into the redirect row (test
  `TestRenameRedirectKeepsHTTPSFlags`), but the Location scheme should follow
  the target row's `HTTPS`; that needs the redirect target row, not just
  `RedirectTo`.
- F4 (`internal/service/admin.go` `SetSettings`, F3's file): `setRootDomain`
  now takes the raw value and stores `installspec.CheckRoot`'s cleaned form,
  and refuses (Conflict) when an instance row sits on neither the old nor the
  new root. `checkRootDomain` has the same refusal, so SetSettings needs no
  change; F3's "check against the incoming panel_domain" work calls the same
  `checkRootDomain`/`planRename`.

- F2 (`internal/service/serverconfig_apply.go` `applyShares` / `unshare`, F3's):
  the share rows are told apart by `Field`, never by the value. Contract
  (`flow/serverconfig/diff.go`, tests `TestShareRowsNeverMeanShareAll`):
  - `connector-share`, Field `share`, New `all`: turn share-all on (the row
    with the Impact line). Field `org`, New `<slug>`: add that org (a slug can
    be `all` in data from before the reservation). Switch on Field, not on
    `c.New == "all"`; today's `applyShares` mistakes an org row named `all`
    for share-all and an org row must never set `all`.
  - `connector-share-delete` (removal, Optional): Field `share`, Old `all`,
    Key `connector-share:<conn>/all`: leave share-all (fall back to the
    file's list). Field `org`, Old `<slug>`, Key
    `connector-share:<conn>/org:<slug>`: take that org out. `unshare` must
    switch on Field, not `from == "all"`. The org key changed from
    `connector-share:<conn>/<slug>`; nothing else reads it.
  - The share-all removal row is no longer offered while a binding (live
    `Bound`, or an `orgs:` entry of the file) names the connector for an org
    the file's list does not keep: a Note instead.
- F2 (`internal/service/internal/leaf/org/org.go` `Rename`/create, not in any
  fixer's list): the slug is `slug.Make(name)` and nothing calls
  `slug.Reserved`, so an org can still be named "All" (or "Params"). `Reserved`
  now includes `all`; the org verbs should refuse `slug.Reserved(s)` too (the
  server file already does for `orgs:` and `share:` lists, and the share rows
  no longer depend on it).
- F2 (`internal/service/serverconfig_apply.go`, F3's): a missing server param
  is now a Note on the plan (`backup_dests.<name> waits for server.params...`),
  not a blocker, and the dest's rows (`dest`, `dest-update`) and a
  `settings.panel_backup_dest` row naming a dest that does not exist yet are
  dropped from the plan. The walk is row driven, so nothing in it changes; do
  not re-add a "blocked" check for the notes. A ref to a non-secret param the
  same file declares now resolves against the file (params apply first).
- F2 (`internal/service/admin.go` `SetSettings`, F3's): the panel accepts an
  `acme_email` ("Ops <ops@x.com>") and a `dns_provider` that `Parse` refuses.
  `Export` now skips any setting `checkSetting` refuses (so export then plan
  reads clean); the cleaner fix is to run the same check in `SetSettings`
  (`serverconfig.Parse`'s `checkSetting` is unexported; export a `CheckSetting`
  if F3 wants it).
- F2 (apply of a `from:` rename of the root, F3/F4): a `domain-rename` row for
  the live root carries Note "also sets root_domain" when the file has no
  `settings.root_domain`; with one, the two must name the same host or the plan
  is blocked (so no plan 2 flip back). `RenameDomainResource` already does the
  `followRoot`.

- F3 (done, for the verifier): `applyServerPlan` now refuses with "the server
  changed since the approve (<impact>); plan again" when the re-diff holds a
  change with an impact line the stored plan did not have (matched on
  kind, tile and field; an unconfirmed plan has none, so an auto-applied plan
  can never walk a new risky change). `SetSettings` cleans and checks
  `panel_domain` (`CheckRoot`, no wildcard, no localhost or address; the
  value it already has passes), writes in fixed order root_domain,
  panel_domain, the rest, proxy_custom last, under one save lock, and checks
  a panel_domain saved with a root_domain against the hosts the rename
  creates. `applyShares` and `unshare` switch on `Field`, per F2's contract.
- F3 (not done, F2's offer): `SetSettings` still does not run `checkSetting`
  on `acme_email` / `dns_provider` (F2's `Export` skips what `Parse` refuses
  instead). It needs `serverconfig.CheckSetting` exported; then one loop line
  in `SetSettings`.

## Fix round status

Verified 2026-10-07 by the Fable pass: every finding replayed with its own
scratch test in the package (written, run, deleted), the fixer's test named
here is the one that stays. `make test`, `make lint`, `make templint` green.

| Finding | Status | Test |
|---|---|---|
| F1 MEDIUM setup wizard 404 with only a shared server connector | fixed | `setup.TestSharedServerConnectorOnly` |
| F1 LOW tile builds Ambiguous | fixed | `service.TestTileConnectorFallsBackToStack` |
| F1 LOW share/rename overwrites a completing App's config | fixed | `connector.TestShareRenameKeepCompletedConfig`, `TestCompleteKeepsShares` |
| F2 HIGH org slug `all` turns on share-all | fixed | `serverconfig.TestShareRowsNeverMeanShareAll`, `slug.TestReserved` |
| F2 MEDIUM missing server param blocks the whole plan | fixed | `serverconfig.TestMissingParamBlocksOnlyItsItem`, `TestMissingParamDropsThatItemsRowsAndNotes` |
| F2 LOW ref to a param the same file declares | fixed | `serverconfig.TestRefToParamTheFileDeclares` |
| F2 MEDIUM create-only checks on existing rows | fixed | `serverconfig.TestExportThenPlanKeepsCreateOnlyChecksOffExistingRows` |
| F2 MEDIUM root `from:` rename vs `settings.root_domain` | fixed | `serverconfig.TestRootRenameAndRootDomainAgree` |
| F2 MEDIUM rename target skips the clash checks | fixed | `serverconfig.TestRenameTargetGetsTheClashChecks` |
| F2 LOW two renames from one host | fixed | `serverconfig.TestTwoRenamesFromOneHost` |
| F2 LOW share-all removal while a bound org would lose it | fixed (note, not blocker: as named-org shares) | `serverconfig.TestShareAllRemovalWaitsForBoundOrgs` |
| F2 LOW export writes values Parse refuses | fixed both ways | `serverconfig.TestExportSkipsValuesParseRefuses`; `SetSettings` now runs `serverconfig.CheckSetting` on `acme_email` and `dns_provider` |
| F3 MEDIUM re-diff applies new risky changes without confirm | fixed | `service.TestServerApplyRefusesNewRiskyChange` (replayed on the panel_domain path too) |
| F3 MEDIUM panel_domain stored raw | fixed | `service.TestPanelDomainStoredClean` |
| F3 MEDIUM root_domain plus panel_domain in one save | fixed | `service.TestSetSettingsRootAndPanelTogether` |
| F3 LOW proxy_custom take-back race and skip | fixed | `service.TestProxyCustomTakeBackRace`; the skip half by write order (proxy_custom last, every check before any write) |
| F3 share rows apply side | fixed | `service.TestApplySharesFieldNotValue`, `TestServerUnshareOrgNamedAll`, `TestServerLeaveShareAll` |
| F4 HIGH rename reclaims another org's redirect | fixed | `service.TestRenameDoesNotReclaimAnotherOrgsRedirect` |
| F4 MEDIUM setRootDomain stores the raw value | fixed | `service.TestSetRootDomainStoresCleanedHost`, `TestSetRootDomainRefusesUnmatchedRow` |
| F4 LOW redirect ignores HTTPS flags | fixed | `service.TestRenameRedirectKeepsHTTPSFlags`; the Caddy Location scheme now follows the row too |
| F4 LOW later stack, env or tile rename strands the redirect | fixed | `service.TestRenameStackRepointsGeneratedRedirects` (tile rename replayed) |
| F5 LOW approved plan still offers Approve, Reject and boxes | fixed | `admin.TestAdminConfigApplyingPlan`, `canvas.TestOrgConfigApplyingPlan` |
| F5 LOW invite links use BASE_URL's host after panel_domain moves | fixed, setup too | `canvas.TestInviteLinkFollowsPanelDomain` |
| F5 LOW custom route hosts get no :80 route with TLS on | fixed | `domain.TestCustomHostRedirectsOnPlain` |

Fix round needs, closed by the verifier:

- `setup.templ`: `install(href)` renders the GitHub install button only with
  a link, else "Ask a server admin to install the shared GitHub App"; the
  "Change which repositories" nav is hidden with no link.
- `comp.PanelURL(host, path)` in `ui/components/static.go` is the one
  builder; `canvas.panelURL` and a new `setup.panelURL` wrap it. Setup's
  invite links and the domain prefill follow `panel_domain`.
- `caddy.go` `domainRoute`: a generated (308) redirect's Location is
  `http://` when the row is HTTP-only or TLS is off; a declared 301 stays
  `https://`.
- `leaf/org` `Rename` refuses `slug.Reserved` ("All", "Params").
- `serverconfig.CheckSetting` is exported; `SetSettings` runs it on
  `acme_email` and `dns_provider`.
- `buildTile`, the store column writers, the missing-param notes and the
  root-rename `followRoot`: verified, no change needed.

Accepted as is:

- The org canvas banner says "Config plan pending" while an approved plan
  is applying (`canvas.planBanner`); the drawer behind it is right.
- `newRisk` matches an impact by kind, tile and field, not by its text: a
  confirmed move to the file's host still applies when only the live "from"
  changed.

## Rig fix needs

R2:
- `stackr org preview` (`cmd/stackr/orgconfig.go`) still prints nothing for a clean file; same two lines as `server preview` in `cmd/stackr/serverconfig.go`.
- `render.OrgPlanView` (`internal/web/render/render.go`) still drops the name on a create row (domain, param): same fix as `ServerPlanView` in `render/plan.go`.
- Bug 4 needed one `Done` branch in `internal/web/handler/admin/config.go` and `internal/web/handler/canvas/orgconfig.go` (status applied), outside R2's list.

R1:
- Limits above the host are refused at the orchestrator setters and in both files' defaults blocks, but a tile limit that arrives through a stack file (`flow/promote` `stackfile.go` Limits, applied in `plan.go`) is not checked; that deploy still fails at create with Docker's words and, since the create now precedes any stop, the old replica keeps running. A check there needs a host-info handle in `flow/promote`.
- `internal/service/internal/leaf/tile/world.go` (not on R1's list) gained `CreateReplica` and `Launch`, and the `Docker` interfaces (`leaf/tile`, `service/docker.go`) gained `Create` and `HostInfo`: the stop-first rollout needs create without start.
- The apply jobs wait for their queued redeploys (`internal/service/redeploys.go`). With `workers` = 1 they cannot (the deploys would never start), so they only list what they queued and still end `done`.

## Rig fix status

Verified 2026-10-08 by the Fable pass on `v0.6.0-dev.18` (rig upgraded from
dev.17; results in `docs/qa/serverconfig-2026-10-07.md`, "Rig verify").
`make test`, `make lint`, `make templint` green before and after the closes
below.

| Bug | Status | Test | Rig |
|---|---|---|---|
| 1 limit above the host refused at every rung | fixed | `service.TestLimitsAboveHostRefused`, `TestLimitsBlockFilePlans` | tile, stack, env, org (web) and the server file's blocker replayed on the 1 CPU, 1973 MB rig |
| 1 failed create or start keeps the old replica | fixed | `deploy.TestCreateErrorKeepsOldRunning`, `TestStartErrorBringsOldBack`, `TestStopFirstGateFailureBringsOldBack` | create (`mem_limit_mb: 4`) and start (host port 443 taken) replayed on a throwaway stop-first tile |
| 1 dead tile counts for "redeploys N tiles" | fixed | `service.TestFailedDeployTileStillRedeploys`, `TestFirstDeployFailureNotRetried` | count replayed read-only (`server preview`: 8 with the dead tile); the redeploy itself only in the test |
| 1 apply reports its redeploys | fixed | `service.TestServerApplyFailsWithItsRedeploys`, `TestOrgApplyFailsWithItsRedeploys` | not on the rig: needs a server cascade change or an org repo |
| 2 plan view keeps the name on create rows | fixed | `render.TestServerPlanViewKeepsNames`, `TestOrgPlanViewKeepsNames` | `+ create route qa-r18-route... 192.168.1.100:80` in the Config tab |
| 3 CSP lets the checkbox tick render | fixed | `web.TestCSPAllowsDataImages` | header carries `img-src 'self' data:`; a ticked box paints its `data:` SVG with no console violation |
| 4 applied plan shows removed and kept | fixed | `components.TestPlanReviewDoneShowsTicks`, `admin.TestAdminConfigAppliedPlanShowsTicks`, `canvas.TestOrgPlanAppliedShowsTicks` | not on the rig: the server plan approve was refused by the harness, org plans need a repo |
| 5 org domain rename confirms in the web | fixed | `canvas.TestOrgDomainRename` | dialog with the CLI's text; Enter does nothing; confirm renamed `qa-r1` to `qa-r1b` |
| 6 `server preview` says "no changes" | fixed | `TestServerPreviewSaysNoChanges` | printed on the rig's export |

Rig fix needs, closed by the verifier:

- `stackr org preview` prints "no changes" for a clean file
  (`cmd/stackr/orgconfig.go`, `TestOrgPreviewSaysNoChanges`).
- `render.OrgPlanView` keeps the name on domain, param and share create
  rows and leaves a removal row its kind (`TestOrgPlanViewKeepsNames`).
- The `Done` branches in `admin/config.go` and `canvas/orgconfig.go`, the
  `leaf/tile` `CreateReplica`/`Launch` split and the `Create`/`HostInfo`
  Docker methods: read, right where they are, no change.

Accepted as is:

- A tile limit that arrives through a stack file (`flow/promote`) is not
  checked against the host; the deploy fails at create with Docker's words
  and the old replica survives it (replayed: that is the create path above).
- With `workers` = 1 an apply cannot wait for its redeploys and says so in
  its log. The rig runs 2.
- The limit error names the CLI flag (`--cpus: This host has 1 CPU...`) even
  for `stack defaults --set cpu_limit=`, because the field is `cpu_limit`
  and the CLI maps it to the tile flag. Cosmetic.
- `stackr org preview` needs a bound repo (`PreviewOrgConfig`), so the org
  file's limit blocker is reachable on the rig only with a connector.

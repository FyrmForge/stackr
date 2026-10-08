# Server config blitz: stackr-server.yml for Sonnet builders

Status: plan, waiting for darthvader's go. Design and every decision:
`serverconfig.md` (read it first; this file only says who builds what).
Stacks on the uncommitted ready blitz: commit that first so review diffs
stay apart.

Rules for every worker: no git write ops (no stash either). Never hand-edit
generated files (`*_templ.go`, `ui/static/js/elements/*`, `output.css`,
`docs/openapi.json`, `staticmanifest.go`); regenerate them (only the wave 2
worker runs `templ generate` and the openapi regen; wave 1 workers run
`templ generate -f <their file>` for a local build only). Touch only the
files and functions you own; re-read a shared file right before every edit;
needs elsewhere go into `serverconfig-seams.md`. No em dashes. Match
neighbouring style; `ponytail:` for ceilings. **Test first**: write the
failing test from the scenario, see it fail, then fix. Finish with
`go build ./... && go vet ./...`, the tests of every package you touched,
and `golangci-lint run` on them. depguard holds: a leaf never imports a
leaf or a flow; a flow imports another flow only on the listed edges.

## Wave 0: contract (one worker)

1. **Migration `005_serverconfig`** (up and down; check how migrations run:
   inside a transaction, foreign keys on or off; test up then down then up
   on a DB with live rows that reference connectors):
   - `connectors.org_id` nullable (NULL = server connector) via table
     rebuild; `orgs.config_connector_id` and `stacks.config_connector_id`
     still point at it. Add `share_all INTEGER NOT NULL DEFAULT 0`.
     Partial unique index: one server connector per host is NOT required,
     but server connector **names** are unique (`WHERE org_id IS NULL`),
     since the file names them.
   - `connector_shares (connector_id, org_id, PRIMARY KEY both)`, both FKs
     cascade.
   - `server_config_plans`: the columns of `org_config_plans` minus
     `org_id`, plus `source TEXT` (`repo` | `local`), `file TEXT` (the
     local file's bytes; "" for repo plans), `ticked TEXT` (JSON list of
     removal keys the approver ticked), `confirmed INTEGER`.
   - `org_config_plans` gains `ticked` and `confirmed` too.
2. **Shared plan package** `internal/service/internal/planfile` (a sibling
   of `slug`, so flows and the service may import it): move `Change`,
   `strictYAML`, `humanYAML`, `checkParams` out of `flow/orgconfig` (and
   point `flow/promote` at it where it holds copies; the "third user
   moves it" notes). `Change` gains:
   - `Impact string`: the human line for a risky change ("panel moves to
     X; point DNS first"). A plan with any Impact is **risky**.
   - `Optional bool` and `Key string`: a removal row, keyed by a stable
     `<kind>:<key>` (`route:<host>`, `dest:<name>`, `domain:<host>`,
     `share:<slug>`, `connector-share:<conn>/<org>`, `org-binding:<slug>`).
   - `Plan.Risky()`, `Plan.Removals()`, `Plan.AutoOK()` (= no blocker, not
     risky, no removal rows).
3. **Approve contract**, enforced in the service, not the UI:
   `ApproveOrgPlan(ctx, id, ApproveOpts{Ticked []string, Confirm bool})`
   and the same for `ApproveServerPlan`. A risky plan without `Confirm` is
   refused ("This plan has impact lines; confirm to approve."). `Ticked`
   keys not in the plan's removals are refused. Both are stored on the row.
   Apply re-diffs and runs only ticked removals that still exist.
4. **Store and leaf**: `store/server_plans.go` (same shape as
   `org_plans.go`); `leaf/orgplan` gains a sibling or a generic so both
   tables share the status machine (worker picks; no logic copy).
5. **Settings catalogue** (`leaf/settings/catalogue.go`): drop `dns_env`
   (and its `secretKnobs` entry, form label); `root_domain` no longer
   `ReadOnly`; new Flat knobs `server_config_connector`,
   `server_config_repo`, `server_config_branch`, `server_config_path`,
   `server_config_auto` (bool, default false), marked so the server file
   schema and the Settings tab skip them (the Config tab shows them).
6. **Stubs** returning `errs.Unset`-style "not built" so wave 1 compiles:
   service verbs `BindServerConfig`, `PlanServerConfig`,
   `PreviewServerConfig(file)`, `PlanServerFile(file)` (local),
   `ServerPlans`, `ServerPlan`, `ApproveServerPlan`, `RejectServerPlan`,
   `ExportServerConfig`, `RenameDomainResource(id, host)`,
   `ShareConnector(id, orgIDs, all)`; job kinds `server-plan`,
   `server-apply`; API routes (below) wired to them.
7. **API route table** (`internal/api/routes.go`, admin perms in
   `internal/authz/authz.go`): `PUT /admin/config-repo`,
   `POST /admin/config/plan`, `POST /admin/config/plan-file` (body = file,
   stores a local plan), `POST /admin/config/plan-preview`,
   `GET /admin/config/plans`, `GET /admin/config/plans/:plan`,
   `POST /admin/config/plans/:plan/approve` (body: ticked, confirm),
   `POST .../reject`, `GET /admin/config/export`,
   `POST /domain-resources/:id/rename`, admin connector create/share
   routes. Org approve route takes the same body.
8. Hand-off section in `serverconfig-seams.md`: types, verbs, routes,
   knobs, job kinds and payloads, lock keys (`serverconfig`,
   `serverplan:<id>`, plus `orgconfig:<org>` per org an apply touches).
9. `make test && make lint` green.

## Wave 1: seven workers in parallel

### S1 Server connectors and sharing
Owns: `leaf/connector/*`, `store/credentials.go`, `internal/service/connector*.go`
(grep where Begin/Complete live), `internal/service/stack.go` ONLY
`Webhook`, `internal/service/wiring.go` ONLY `clone`/`cloneEnv` and a new
`serverFile`, `internal/service/orgconfig.go` ONLY `SetOrgConfigRepo`'s
connector check and `orgLive`'s `Connectors`, `web/handler/canvas/orgconfig.go`
connector picker, admin drawer Connectors tab (`ui/drawer/admin/admin.templ`
connectors section, `web/handler/admin/handler.go` connectors section),
setup/connector callback handlers as needed, tests.
- An admin creates a server connector through the same GitHub manifest
  flow (Begin records the starting user; Complete refuses a mismatch).
- Share: none (default), named orgs, all orgs. Warning copy on "all orgs":
  every org owner can clone every repo the App is installed on.
- **Read paths widen, write paths do not.** Org reads (`ListConnected`,
  the picker, `SetOrgConfigRepo`, `orgLive.Connectors`, clone) see the
  server connectors shared with that org. Org writes (delete, rename,
  reconfigure, keyed by org) never reach a server connector: keep
  `conns.Get(orgID, id)` failing for NULL-org rows. Test: an org owner
  cannot delete or edit a shared server connector.
- Resolution for a repo host: the connector the file or binding names
  wins; else the org's own for that host; else exactly one shared server
  connector; anything else is a blocker "name the connector to use".
- Webhook for a server connector: queue the server plan when the push
  matches the server binding, then fan out to every org it is shared with
  (their org plans and stacks), instead of `o.stacks.List(ctx, c.OrgID)`.
- Revoking a share is refused while a binding in that org names the
  connector.

### S2 Server params scope
Owns: `leaf/params/*` (scope `server`, `ref.go`), `internal/service/params.go`,
API params handlers for the scope, `cmd/stackr` params commands (`--scope
server`), admin drawer Params tab (reuse the existing params editor
component; `admin.templ` params section, `handler.go` params section), tests.
- `server` scope: admin only, params and secrets, same editor and CLI.
- `${{ server.params.<col>.<name> }}` resolves **only** inside the server
  file's items (backup dest keys, later connector tokens). Refused, with a
  clear error, in an org file, a share password, a stack file and a tile
  param: orgs never read server secrets.
- A missing value: the referencing item is a blocker in the server plan
  ("backup dest offsite needs server.params.s3.secret_key"), nothing else.

### S3 flow/serverconfig, and org removals
Owns: new `internal/service/internal/flow/serverconfig/{file,diff,export}.go`
and tests; `flow/orgconfig/diff.go` ONLY the shares delete; tests.
- `File`: `version: 1`, `settings:` (Flat knobs minus the
  `server_config_*` ones), `defaults:` (server cascade rung), `params:`
  (server scope, secrets by name), `routes:`, `backup_dests:` (name, kind,
  endpoint, region, bucket, access_key, secret_key and archive_key as
  `${{ server.params }}` refs), `domains:` (instance domain resources:
  host, include_env_on_default, acme_email; a `from:` field renames an
  existing one), `connectors:` (by name: share `all` or a list of org
  slugs; never created), `orgs:` (slug, name, config repo, branch, path,
  connector name, auto). Strict parse (planfile).
- `Diff(File, Live) Plan` (pure). Never deletes: anything live the file
  does not name is an Optional row (`Key` as in wave 0) when its block is
  present; orgs and params are never removal rows. Blockers reuse each
  verb's refusal (dest used by a schedule or `panel_backup_dest`, the
  local dest, a domain resource tiles sit under, a share a binding uses).
- Impact lines (Live carries the counts the service gathers):
  `panel_domain` ("panel moves to X; point DNS at this box first", plus
  whatever S6 finds), `root_domain` / `domains: from:` rename ("moves N
  hosts, issues N certificates; point DNS first"; past ~50 without
  wildcard, add "Let's Encrypt allows about 50 a week per domain"),
  `dns_provider` ("wildcard certificates need the DNS token on the proxy";
  refuse a provider not compiled in: cloudflare only), `proxy_custom`
  ("replaces the extra Caddy routes"), cascade defaults ("redeploys N
  tiles"), `trusted_proxies`, `acme_email` (validate as email, as domain
  resources do), **creating an org** (an auto-applied plan has no
  approver to own it).
- `Export(Live)`: every Flat knob with a stored value, rung, routes,
  dests with refs, instance domains, server params (secrets by name),
  server connectors by name with shares, orgs with bindings. Test: export
  then diff reads clean.
- Org file: a share missing from a present `shares:` block becomes an
  Optional row instead of `share-delete` (darthvader 2026-10-07).

### S4 Service, jobs, API, CLI
Owns: new `internal/service/serverconfig.go` and tests;
`internal/service/orgconfig.go` ONLY `ApproveOrgPlan`, `approveOrgPlan`,
`walkOrgPlan`'s share removal, `planOrg`'s auto rule; `internal/service/jobs.go`
the two new kinds only; API handlers for the wave 0 routes (new
`internal/api/handler/v1/serverconfig.go`, the org approve handler body);
new `cmd/stackr/serverconfig.go` (`stackr server bind|plan|apply|preview|
plans|approve|reject|export`; `apply <file>` = plan-file, show the plan,
confirm, approve with `--remove <key>` and an extra confirm when risky);
`cmd/stackr/orgconfig.go` approve gains `--remove` and the risky confirm;
tests.
- Plan from the bound repo (clone through S1's `serverFile`) or from a
  local file (stored on the row, `source=local`, shown "from a local file,
  not the repo"). Local apply stays allowed while a repo is bound.
- Apply: refetch (or read the stored file), re-diff, refuse when blocked,
  walk in order: server params, connector shares, settings and rung
  (`SetSettings`, `SetSettingDefaults`), dests, domains (renames via
  `RenameDomainResource`), routes, orgs (create with the approver as owner,
  then `SetOrgConfigRepo`, which plans the org), then ticked removals.
  Locks as in the hand-off.
- Auto-apply: only `AutoOK()` plans, for server and org files alike.
- `Live` gathering: counts for the impact lines (hosts under a resource,
  tiles a cascade change redeploys).

### S5 Domain resource rename (root domain changes)
Owns: `leaf/domainres/*`, `internal/service/domainres.go`,
`internal/service/admin.go` ONLY the `root_domain` branch of `SetSetting`,
`store/domain_resources.go`, the domains store if a host update needs one,
API/CLI `stackr domain rename`, org drawer Domains rename (`org.templ`
domains section, `web/handler/canvas/org.go` `orgDomains`), tests.
- `RenameDomainResource(id, newHost)`: pre-check every new host against
  the squat check, `checkRouteHost` and the panel host; rewrite every
  auto/apex host whose `resource_id` is the resource (literal hosts
  untouched); add a 308 redirect row per old host to its new host (same
  tile, `redirect_to`), kept until removed; one proxy push.
- Redirect rows survive a later promote or env sync of the stack: check
  whether the stack-file domain sync deletes rows the file does not name,
  and make redirect rows exempt. Test it.
- `SetSetting("root_domain", new)` renames the instance resource whose host
  is the old root; `SeedInstance` must not re-seed the old one at boot.
- Counts helper for S3/S4's impact line.

### S6 Panel domain trace and proxy_custom
Owns: `leaf/domain/caddy.go`, `flow/deploy/proxy.go`, `internal/service/wiring.go`
ONLY the Install fields, admin Caddy tab copy (`admin.templ` caddy
section, `handler.go` caddy section), tests; a findings note in
`serverconfig-seams.md`.
- `proxy_custom` (DECIDE 127, decision taken: a JSON array of Caddy HTTP
  route objects appended after stackr's routes on the main server,
  additive, never a whole-config replace, so the panel vhost survives).
  Validate as a JSON array at save; a Caddy load error fails the setting
  save or the apply step. Test: a route in it reaches the pushed config;
  bad JSON refused.
- Trace every consumer of the panel host before S3 writes its impact
  line: the panel's `BASE_URL` env (installer-set; the file cannot change
  it), cookie and CSRF origin checks, the GitHub App webhook and callback
  URLs registered on GitHub, the CLI authorize page, `checkPanelRoutes`.
  Write what breaks on a change and whether the admin can be locked out;
  fix what is cheap (read the setting instead of the env), list the rest
  for the impact line.

### S7 Web: Config tab and plan review
Owns: admin drawer Config tab (`admin.templ` config section,
`handler.go` config section, new admin config handler file if the
handler grows), org drawer plan view (`ui/drawer/org/org.templ` config
section, `web/handler/canvas/orgconfig.go` plan and approve only; S1 owns
its connector picker), shared plan component (new
`ui/components/plan.templ`), tests.
- Config tab: bind form (server connector select, repo, branch, path,
  auto switch), plan now, export download, plans list (source repo or
  local file, commit, summary, status).
- Plan view (server and org alike): changes; impact lines highlighted;
  removal rows as checkboxes, unticked; blockers. Approve with impact
  lines opens an "are you sure?" that lists them and posts `confirm`.
- Terse copy, no em dashes.

## Wave 2: seams and review
One worker: resolve `serverconfig-seams.md`, `templ generate`, openapi
regen, `make test && make lint && make templint` green; docs:
`PROGRESS.md` dated section and DECIDE entries (server file, server
connectors, server params, removal rows, impact lines, root rename,
proxy_custom shape), `leftovers.md` (strike 127), AGENTS.md config-as-code
section, `recovery.md` one line (the server file as DR step 0).
Then static analysis (gitleaks, govulncheck, semgrep, trivy fs) and its
findings into an Opus review round per area (S1-S7): real defects only,
failure scenario each; a fix round on what survives verification.

## Wave 3: rig
Ship `v0.6.0-dev.17`. Bootstrap: `stackr server export` from the rig,
edit (a schedule, a route, an instance domain acme_email), `stackr server
apply` the local file, approve, check. A risky change (cascade cpu_limit)
needs the confirm; auto on does not apply it. A route dropped from the
file is an unticked row; ticking removes it. Org domain resource rename on
a throwaway org: hosts move, old hosts answer 308. No root rename on the
rig. Server connector and repo binding: **ask darthvader before creating
any GitHub App or repo**; the manifest flow runs headed. Never edit shop,
web, infra, byhand; never set smoke org-level params. Report in
`docs/qa/serverconfig-<date>.md`.

## Not in this plan
DNS connectors, SMTP2GO, port forwarding, the terminal, the v0 gaps list.

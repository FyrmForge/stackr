# Placements + multi-node config as code: build plan

Status: plan written 2026-10-16, not started. Design: `placements.md` (same
folder). Base: branch `multi-node` at b42298c. Built on a new branch
`placements` by parallel Sonnet workers, one git worktree each, after a
Wave 0 the integrator does alone.

Every "Agreed" bullet of `placements.md` is a requirement. The one "Open"
item (installer `restore --from` an S3 destination) is out of scope.

Before Wave 0 starts, darthvader confirms the list in section 0. Each line
is a default this plan already builds on; a "no" changes the lanes named.

## 0. Decisions taken (confirm one at a time)

These are places where the design and the code disagree, or the design is
silent. The plan builds on the default named on each line.

- **D1 `open` lives on placements only.** The design gives `nodes:` the keys
  "tags, open", but its last bullet says access is per placement and nodes
  are admin-only hardware. **decision taken:** `nodes:` carries `tags` only;
  `open: all | [orgs]` is on `placements:` (over `open` on both). This
  follows on from it:
  - the Add node dialog prefills name and tags, and no orgs;
  - `join_tokens.open_to_all` and `org_ids` go;
  - `node_orgs` and `nodes.open_to_all` go.
  Lanes: A, S, P.
- **D2 "Node not open to the org" plan error becomes "placement not open to
  the org".** It follows from D1. The move check "open to orgs" has nothing
  left to check once nodes carry no access. **decision taken:** that check is
  dropped (over keeping it as a no-op line). Lanes: O, M.
- **D3 Members never see node names.** Today members see them in these
  places:
  - `GET /nodes` and `/nodes/:node` are `Self`, redacted but still named;
  - the cluster map has a member view;
  - `PlacementView` and the env move dialog show node names;
  - `stackr env move --to <node>` and `stackr env create --node` take node names;
  - NodeDown banners name the node ("errors name the node", from
    `multi-node-finish.md` item 10).

  **decision taken:**
  - `/nodes`, `/nodes/:node` and the cluster map become admin-only (`node.manage`);
  - member surfaces show the placement name;
  - the NodeDown banner says "placement eu-prod is offline, last seen ..."
    for members and names the node for admins.

  The alternative was keeping the redacted member views. Lanes: A, C, U.
- **D4 Changing an env's pin moves the env.** Multi-node step 1 said "a pin
  never moves an existing env". Option C makes placement live state.
  **decision taken:**
  - an env's placement is stored on the env (`environments.placement_id`);
  - changing the pin at env, stack or org level so that an existing env's
    effective placement changes moves that env through the same move flow,
    checks and confirm as a placement repoint;
  - `stackr env move` goes and `stackr pin` replaces it.

  The alternative was pins placing new envs only. Lanes: A, M, O, C.
- **D5 Default placement.** The design does not say where an env goes when
  nothing is pinned. **decision taken:** a reserved placement named
  `default`:
  - the migration creates it on the controller, open to all;
  - it can be repointed and never deleted;
  - the server file may declare it like any other.

  The alternative was a `default_placement` settings knob. Lanes: W0, A, S.
- **D6 "Envs wait" when a placement has no node.**
  **decision taken:**
  - such an env is created with `node_id = ''`;
  - a deploy, run or schedule of an env with no node is refused up front
    ("placement eu-prod has no node yet"), the same way as an offline node;
  - when the admin (or the server file) gives the placement a node, every
    env of the placement with `node_id = ''` gets that node and its tiles are
    queued for deploy. There is nothing to move.

  The alternative was parking the jobs. Lanes: A, M.
- **D7 A placement the file no longer names is a removal row** (`Optional`,
  key `placement:<name>`, ticked by the approver), with a note when it is in
  use. The verb then refuses it while pins or envs reference it. Nodes never
  get a removal row: a joined node missing from the file is shown as
  "unmanaged". Lane S.
- **D8 Lost envs come back through their placement.** After Forget node,
  repointing a placement whose node was forgotten is a restore: each env is
  rehomed (`rehomeLostEnv`) and then restored from backups. The plain move is
  not used. The `env restore` CLI and the admin lost-envs list take a
  placement, never a node. A placement on a forgotten node shows "node
  forgotten: repoint to restore". Lanes: M, C, P.
- **D9 The storage tag.** The controller gets `storage` at bootstrap and in
  the migration. A worker that already holds volumes is tagged too, so
  nothing on the rig becomes a plan error. `storage` is added to `joinTags`.
  Lanes: W0, A.
- **D10 Ingress default changes.** With no override, an env's routes enter
  on its own node when that node is tagged ingress, and on the cluster
  default otherwise. Today it is always the cluster default unless picked.
  The DNS for such hosts then has to point at that node. The rig's h4 is
  tagged ingress, so its envs switch on upgrade. This follows the design as
  written; it is flagged because it changes live traffic. Lane A.
- **D11 Overrides at org, stack and env in files; tile-level ingress and
  exit overrides stay panel and API only.** The design lists org, stack and
  env. Lane O.
- **D12 Node-level default exit ladders stay.** These are migration 024's
  `node` scope and `stackr node exit`. They are admin hardware config and
  become the "else" in "its node if tagged, else cluster default". Lane A.
- **D13 Dead columns stay.** `orgs.node_id` and `stacks.node_id` carry
  `REFERENCES nodes`. SQLite's `DROP COLUMN` refuses such a column, and
  rebuilding `orgs` drags every cascade trigger with it, which is the trap in
  the squash notes. **decision taken:** the migration nulls them and the
  store stops mapping them. The next dumpschema squash removes them (over a
  table rebuild now).

## 1. Target model

### Tables (migration 027 by W0, migration 028 by lane A)
- `placements`:
  - `id` TEXT PK
  - `name` TEXT UNIQUE (node-name rules: `^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)
  - `node_id` TEXT NOT NULL DEFAULT '': '' means no node yet. No FK: `dropNode` deletes the node row on forget, and the placement must keep naming the forgotten node (D8). `NodeForgotten` is derived from `forgotten_nodes`; a repoint off a forgotten node is a restore, off '' a node set (D6)
  - `open_to_all` INTEGER NOT NULL DEFAULT 1
  - `created_at`
- `placement_orgs`: (`placement_id` FK CASCADE, `org_id` FK CASCADE), PK both.
- `placement_moves`:
  - `id`, `placement_id` FK CASCADE
  - `from_node`, `to_node`: no FK, as in `env_moves`
  - `restore` BOOL: true after a forget, per D8
  - `state` CHECK in (`running`, `failed`, `done`)
  - `by`, `key_id`, `started_at`, `finished_at`, `error`

  The placement keeps `node_id` = from until every env is on `to_node`.
- Column changes:
  - `orgs.placement_id` and `stacks.placement_id`: TEXT NULL REFERENCES placements ON DELETE RESTRICT. These are the pins.
  - `environments.placement_id`: TEXT NOT NULL DEFAULT ''. Where the env belongs. `environments.node_id` stays as where it actually runs.
  - `env_moves.placement_move_id`: TEXT NULL.
- `ingress_picks`: `node_id` is renamed to `placement_id`. Rows for org, stack, env and tile scopes are rewritten from node ids to placement ids.
- `exit_ladders`: `node_id` is renamed to `target_id`. org, stack, env and tile rows now hold placement ids; `node` scope rows keep node ids, and the `own` rung stays.
- Migration 028 (lane A, after its readers are gone):
  - drop `node_orgs`;
  - drop `nodes.open_to_all`;
  - drop `join_tokens.open_to_all` and `org_ids`.

### Data conversion in 027
1. The `default` placement goes on the controller row, `open_to_all = 1`.
2. One placement per worker node that anything references (an org or stack
   pin, an env's `node_id`, an ingress pick, a ladder rung), named after the
   node. Its `open_to_all` and `placement_orgs` are copied from the node's
   `open_to_all` and `node_orgs`. A controller pin maps to `default`.
3. `orgs.placement_id` and `stacks.placement_id` are set from the old
   `node_id` pins, and then `node_id` is set to NULL (D13).
4. `environments.placement_id`:
   - the placement of the env's stack pin when that pin's node is the env's node;
   - else the org's pin, under the same condition;
   - else the placement named after the env's node;
   - else `default`.
5. Ingress picks and ladder rungs are mapped node id to placement id.
6. `server_setup_done_at` is stamped when any org exists (lane U's gate).
7. The controller's `tags` gains `storage`, and so does every node in
   `volumes.node_id` or hosting an env with volumes (D9).

### Store (W0)
- New `store/placements.go`:
  - `Placement` row;
  - `PlacementStore`: Create, Get, GetByName, List, ListByNode, SetNode, SetOpen, Delete;
  - `PlacementOrgStore`: Set, OrgIDs, Has;
  - `PlacementMoveStore`: Create, Get, Latest(placementID), SetState.
- Edits:
  - `store/orgs.go` and `stacks.go`: `NodeID *string` becomes `PlacementID *string`;
  - `store/environments.go`: `PlacementID`, `SetPlacement`, `ListByPlacement`;
  - `IngressPick.NodeID` becomes `PlacementID` (`ListByNode` becomes `ListByPlacement`);
  - `ExitLadder` rows `NodeID` becomes `TargetID`;
  - `store/moves.go`: `PlacementMoveID`;
  - `store/store.go`: new tables on `Tables`.

### Service (exact signatures in section 2)

| Design bullet | Where it lands |
|---|---|
| No scheduler; names in config, one node each, repointed by admin | `placements` table; `MovePlacement` (admin); `PlanPlacementMove` |
| Plan errors: undeclared name, not open to org, wrong tag | `orgconfig` and `promote` diff blockers from `PlacementFacts` (lane O); one rule, `planfile.Fits` |
| Plan warns: no node yet, node offline | Notes and Warnings, same facts |
| Offline source waits; restore only via Forget | move check `source online` fails with "wait, or forget node X"; forgotten source = restore move (D8) |
| `storage` tag | `node.TagStorage`; `planfile.Fits` needs storage when an env has volumes or a managed tile |
| Mixed env = DB stack + cross-node link | nothing to build; docs line in `docs/guide/placements.md` |
| Server setup on first login, by hand or file; restore docs only | `/-/setup/server` wizard (lane U); `server_setup_done_at` setting |
| Server file `nodes:` and `placements:`; declares, never creates; Add this node; unmanaged | `serverconfig.File.Nodes`, `.Placements`; `Plan.Gaps`; notes (lane S) |
| Placement node is live state; one move flow for wizard and file | `MovePlacement` called by the panel, CLI and server apply walk |
| Move checks, blocked plan, confirm dialog, job not awaited, partial failure, Retry | `moveChecks`, `planfile.Check`; blocker when failing; impact line when passing; `placement_moves` + `RetryPlacementMove` |
| Plan fills gaps: secret field, Add this node prefilled | `planfile.Gap`; `PlanView.Gaps`; existing `SetParams` and Add node dialog (lane P) |
| `pin:` at org/stack/env; ingress and exit follow placement; overrides name placements | org file `pin`, `ingress`, `exits`; stack file `pin`, `ingress`, `exits` at stack and env (lane O); `IngressFor` and `ladders` resolve placement to node |
| Devs list placements open to their org | `OrgPlacements`; `GET /orgs/:org/placements`; `stackr placements`; org Placement tab |
| Access per placement; node names admin only | `placement_orgs`; D3 redaction |

### Server file keys (lane S)
```yaml
nodes:
  h3: {tags: [egress, storage]}
  h4: {tags: [ingress, egress]}
placements:
  default: {node: ctl}
  eu-prod: {node: h4, open: all}
  eu-db:   {node: h3, open: [acme]}
```
- Placement `open` reuses `serverconfig.Share` (`all` or a list of org slugs); it defaults to `all`.
- `node:` may be absent, which means no node yet.

### Org and stack file keys (lane O)
- `stackr-org.yml` top level:
  - `pin: <placement>`
  - `ingress: <placement>`
  - `exits: [<placement>|own, ...]`
- stack file top level, and under `environments.<env>`: the same three keys.

### API routes (lane C; `internal/api/routes.go`)
- `GET /orgs/:org/placements` (`org.read`): the dev list.
- `GET /admin/placements`, `POST /admin/placements`, `PUT /admin/placements/:placement/open`, `DELETE /admin/placements/:placement` (`node.manage`).
- `GET /admin/placements/:placement/move-plan?to=<node>`, `POST /admin/placements/:placement/move` (body `{to, confirm}`), `POST /admin/placements/:placement/retry`, `GET /admin/placements/:placement/move` (`node.manage`).
- The existing scope routes `.../pin`, `.../ingress`, `.../exits` take placement names. An env gets a `pin` route, whose PUT returns 409 with the move plan unless `confirm` = the env slug (D4).
- Removed:
  - `POST /envs/.../move`;
  - `POST /admin/nodes/:node/orgs`, `DELETE /admin/nodes/:node/orgs/:org`, `PUT /admin/nodes/:node/open-to-all`;
  - `/nodes` and `/nodes/:node` become `node.manage` (D3).

### CLI (lane C; `cmd/stackr`)
- `stackr placements`: the dev list (name, tags, public IP, online, usable).
- `stackr admin placement ls|add <name> [--node n] [--open all|org,org]|open <name> all|org,...|rm <name>`.
- `stackr admin placement move <name> <node> [--yes]`: prints the checks; exit code 3 when it needs `--yes`, which types the name.
- `stackr admin placement retry <name>`.
- `stackr pin [placement] [--env]`: an env pin that moves prints the plan and needs `--yes`.
- `stackr ingress set [placement]`, `stackr exits set [placement|own]...`.
- `stackr env restore` takes `--placement`.
- Removed: `stackr env move`, `stackr node open|close`, the `node add` org flags.

### Panel
- `/-/admin/placements` (lane P):
  - list (name, node, tags, open to, envs, state);
  - add and edit dialog;
  - move wizard: target node, CI-style checks list, then a confirm dialog naming from and to, the envs and expected downtime;
  - progress view with Retry.
- `/-/admin/nodes`: open-to-orgs controls are removed; the Add node dialog takes `?name=&tags=` prefill (lane P).
- Plan review (server and org): gap rows. An unset secret gets an inline field that posts to `SetParams` and then re-plans; a missing node gets "Add this node", which opens the prefilled dialog (lane P).
- Per-scope Placement tab (`handler/placement`, rework): the pin select lists placements open to the org with fit hints; ingress and exits pick placements; the env move dialog becomes the D4 pin-change dialog; org level lists the placements open to the org (lane U).
- `/-/setup/server`: the first-run wizard (lane U).

## 2. Wave 0 (integrator, before lanes)

The integrator does this alone on branch `placements`. The goal is a base
that builds, passes `make test`, and holds every type and signature the
lanes code against. Logic that belongs to a lane is a stub in that lane's
file, returning `errs.ErrNotBuilt` (a new sentinel in `service/errs`).

1. **Migrations.** `internal/db/migrations/027_placements.up.sql` and
   `.down.sql`, as in section 1. A down migration that cannot restore the
   data says so in a comment and drops the new tables.
2. **Store.** The new `store/placements.go` and the edits listed in section 1.
   Fix the callers mechanically (field renames only) in:
   `placement.go`, `nodes.go`, `nodeadmin.go`, `nodeapi.go`, `nodeexit.go`,
   `mesh.go`, `noderemove.go`, `store/placement_test.go`, and the
   `storetest` seeding (the `default` placement).
3. **Keep behaviour green with the smallest shim** in these existing
   functions, so no lane has to touch the callers. `wiring.go`,
   `vipboot.go`, `cluster.go`, `ingressswitch.go`, `mesh.go` (outside
   `RemoveNode`) and the proxy code belong to nobody.
   - `IngressFor(ctx, tileID) (string, error)` keeps its signature and still
     returns a node id. It maps the picked placement to its node. D10
     ordering is lane A's.
   - `ladders(ctx, ch)` maps placement ids to node ids before
     `exits.Levels`.
   - `envPins` reads placement pins, resolves them to the placement's node,
     and returns `environment.Pins` unchanged.
4. **Node leaf.** `node.TagStorage = "storage"`. The controller bootstrap
   tags it. It goes into `joinTags`.
5. **planfile contract** (`internal/service/internal/planfile/planfile.go`):
```go
// Check is one line of a move's CI-style checks.
type Check struct {
	Name   string `json:"name"`   // "target online", "mesh link", "tags", "disk", "memory", "cpu arch", "source online"
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"` // "h4 free 12 GB, needs 9.4 GB + 10%"
}

// Gap is something a plan can fill in place.
type Gap struct {
	Kind  string   `json:"kind"`            // "secret" | "node"
	Scope string   `json:"scope,omitempty"` // secret: server | org | stack | env
	Env   string   `json:"env,omitempty"`   // secret at env scope: the env slug
	Key   string   `json:"key"`             // secret: collection.name; node: the node name
	Tags  []string `json:"tags,omitempty"`  // node: tags from the file
}
```
   Add `Gaps []Gap \`json:"gaps,omitempty"\`` to `planfile.Plan` and to
   `promote.Plan`. Gaps render; they do not gate `AutoOK()` (an unset
   secret was a Note and never stopped auto-apply; that stays).
6. **Service contracts.** New files with stub bodies, each owned by the lane
   named:
```go
// internal/service/placements.go (lane A)
type Placement struct {
	ID, Name     string
	NodeID       string   // "" = no node yet
	NodeName     string   // admin views only
	Tags         []string // the node's tags
	Public       bool     // the node has a public IP
	Online       bool
	OpenToAll    bool
	Orgs         []string // org slugs, when not open to all
	Envs         int
	Default      bool
	Moving       *PlacementMoveStatus
	NodeForgotten bool
}
type PlacementSpec struct {
	Name, Node string // Node is a node name; "" = none yet
	OpenToAll  bool
	Orgs       []string // slugs
}
// PlacementOffer is one row of the dev list; never names a node.
type PlacementOffer struct {
	Name         string
	Tags         []string
	Public       bool
	Online       bool
	HasNode      bool
	Usable       bool
	Why          string // "" when usable: "no node yet", "offline"
}
// Needs is what an env asks of its placement's node (defined in planfile, item 7).
type Needs = planfile.Needs

func (o *Orchestrator) Placements(ctx context.Context) ([]Placement, error)                      // admin
func (o *Orchestrator) PlacementByName(ctx context.Context, name string) (Placement, error)       // admin
func (o *Orchestrator) OrgPlacements(ctx context.Context, orgID string) ([]PlacementOffer, error) // members
func (o *Orchestrator) CreatePlacement(ctx context.Context, by *Principal, s PlacementSpec) (Placement, error)
func (o *Orchestrator) SetPlacementOpen(ctx context.Context, by *Principal, name string, all bool, orgSlugs []string) error
func (o *Orchestrator) DeletePlacement(ctx context.Context, by *Principal, name string) error
func (o *Orchestrator) placementOpenTo(ctx context.Context, placementID, orgID string) (bool, error)
func (o *Orchestrator) envNeeds(ctx context.Context, envID string) (Needs, error)
// The one tag rule is planfile.Fits (W0, item 7); service code calls it,
// never its own copy.
// SetPin, SetIngress and SetExits keep their names and signatures in
// placement.go; only the string now means a placement name (lane A rewrites
// them). SetPin also takes ScopeEnv; an env whose effective placement
// changes node is refused with *MoveNeeded (D4), and the caller confirms
// through MoveEnvToPlacement. MoveEnvsToPin goes (lane A); the tab's
// move-all route goes (lane U).

// internal/service/placementmove.go (lane M)
type MoveEnvLine struct {
	Path     string
	Volumes  int
	Bytes    int64  // -1 = unknown
	Downtime string // "about 20 s"
}
type PlacementMovePlan struct {
	Placement, From, To string // node names (admin) or placement names (env pin change)
	Restore             bool   // the source was forgotten: restore from backups
	Envs                []MoveEnvLine
	Checks              []planfile.Check
	OK                  bool
}
type PlacementMoveStatus struct {
	State     string // running | failed | done
	From, To  string
	Done, All int
	Error     string
	Envs      []MoveStatus
}
// MoveNeeded is SetPin's refusal when the pin change moves an env.
type MoveNeeded struct{ Plan PlacementMovePlan }
func (e *MoveNeeded) Error() string

func (o *Orchestrator) moveChecks(ctx context.Context, envIDs []string, fromNode, toNode string) ([]planfile.Check, error)
func (o *Orchestrator) PlanPlacementMove(ctx context.Context, placement, toNode string) (PlacementMovePlan, error)
func (o *Orchestrator) MovePlacement(ctx context.Context, by *Principal, placement, toNode, confirm string) (Job, error)
func (o *Orchestrator) RetryPlacementMove(ctx context.Context, by *Principal, placement string) (Job, error)
func (o *Orchestrator) PlacementMove(ctx context.Context, placement string) (*PlacementMoveStatus, error)
func (o *Orchestrator) PlanEnvPin(ctx context.Context, envID, placement string) (PlacementMovePlan, error)
func (o *Orchestrator) MoveEnvToPlacement(ctx context.Context, by *Principal, envID, placement, confirm string) (Job, error)
// placementNodeSet gives a nodeless placement a node and deploys its waiting envs (D6).
func (o *Orchestrator) placementNodeSet(ctx context.Context, by *Principal, placementID, nodeID string) error
// D8: a repoint off a forgotten node. MovePlacement calls it when the
// source is in forgotten_nodes: rehome each env, deploy, restore backups.
// RestoreLostEnv, RestoreEnv and rehomeLostEnv (nodeadmin.go, nodeapi.go)
// are deleted by lane A; lost envs come back only through this.
func (o *Orchestrator) restorePlacement(ctx context.Context, by *Principal, placementID, toNodeID string) (Job, error)

// internal/service/serversetup.go (lane U)
type ServerSetup struct {
	Done     bool
	Mode     string // "" | "ui" | "config"
	Steps    []string
	Binding  ServerBinding
}
func (o *Orchestrator) ServerSetupState(ctx context.Context) (ServerSetup, error)
func (o *Orchestrator) SetServerSetupMode(ctx context.Context, by *Principal, mode string) (ServerSetup, error)
func (o *Orchestrator) FinishServerSetup(ctx context.Context, by *Principal) error
```
7. **Flow facts** (the stubs live in the lane's files; the types are W0's
   so that both sides compile):
```go
// flow/serverconfig/diff.go Live gains (lane S fills serverLive):
Nodes      []NodeLive
Placements []PlacementLive
Moves      map[string][]planfile.Check // "placement->node": checks for each repoint the file asks for
MoveEnvs   map[string][]string         // "placement->node": env paths it moves

type NodeLive struct {
	ID, Name string
	Tags     []string
	Pending  bool
	Online   bool
	Forgotten bool
}
type PlacementLive struct {
	ID, Name, Node string // Node = name, "" = none
	OpenToAll      bool
	Orgs           []string // slugs
	Used           int      // pins + envs
}

// flow/orgconfig and flow/promote both take (lane O fills the gatherers):
type PlacementFacts struct { // in planfile, shared
	Known map[string]PlacementFact // by placement name
}
type PlacementFact struct {
	Open, HasNode, Online bool
	Tags                  []string
}
```
   `PlacementFacts` and `PlacementFact` go in `planfile`. `orgconfig.Live`
   and the stack plan input gain a `Placements planfile.PlacementFacts`
   field, plus `EnvNeeds map[string]planfile.Needs` (env slug to needs),
   plus `Moves map[string]planfile.MoveFact` (env path to the move a pin
   change would cause, filled by the service gatherer from `PlanEnvPin`):
```go
type MoveFact struct {
	From, To string // placement names
	Checks   []Check
	Downtime string
}
```
   `Needs` moves to `planfile` and the service aliases it. W0 also writes
   the one tag rule there, complete (it is small and every lane uses it):
```go
// Fits is why a node with tags does not fit n; nil = it fits.
func Fits(tags []string, n Needs) []string // "needs storage", "needs ingress", "needs egress"
```
8. **Done when:** `go build ./...` and `make test` are green on `placements`
   and the migration runs up on a copy of the rig DB (`stackr-install
   backup` from 109).
9. **Gate:** lanes branch from a commit, so Wave 0 is committed on
   `placements` (files added by name, one-line message) before any lane
   starts. darthvader's "go" for the build covers this commit; ask if it
   was not given.

## 3. Lanes

### Rules for every lane
- Base is W0's commit on `placements`. Your worktree: `git worktree add
  ../stackr-<lane> placements`, done by the integrator.
- Read `AGENTS.md` (layering), `docs/rewrite/tasks/placements.md`, and
  sections 0 to 2 of this file at their main-tree paths under
  `/home/darthvader/FyrmForge/stackr/docs/rewrite/tasks/`.
- Edit only the files on your list. If you need a change elsewhere, write
  it in your final report as a patch.
- Nobody edits these:
  - `internal/service/orchestrator.go`, `cmd/stackrd/main.go`, `internal/web/server.go`;
  - generated files: `*_templ.go`, `ui/static/js/elements/*`, `output.css`, `staticmanifest.go`, `docs/openapi.json`, `docs/llms*.txt`.

  Report the one-line wiring or route mount instead. You may run `templ
  generate` in your worktree to compile; the integrator regenerates.
- No git writes of any kind. Leave changes unstaged.
- Build and test inside the worktree only: `go build ./...`, `go vet`, and
  `go test` for your packages or `-run` patterns. Do not run `make` targets
  that need the dev server.
- Tests: stdlib `testing`, table-driven; `servicetest.New(t)` with the fake
  nodes (`fakenodes.go`).
- Copy style:
  - no em dashes; terse UI copy, one sentence per hint;
  - members' copy never names a node;
  - controller and worker in admin copy.
- Never print secrets: no keys, tokens, passwords or master key in logs,
  test output or reports.
- Final report: files changed, tests run with their results, what you could
  not do, and patches for files you do not own.

### Lane A: placement core service
- **Goal:** placements, per-placement access, resolution, pins at org, stack and env, ingress and exits by placement, D3 redaction, the removal of node-level access.
- **Owns:**
  - in `internal/service/`: `placements.go` (fill), `placement.go`, `nodes.go` (`envPins`), `nodeadmin.go`, `nodeapi.go`, `nodeexit.go`, `noderedact.go`, `join.go`, `nodegate.go`;
  - `internal/service/mesh.go` (only `RemoveNode`, which prunes rungs by placement), `internal/service/noderemove.go` (refused while a placement points at the node);
  - `internal/service/internal/leaf/environment/environment.go` (Spec gains `PlacementID`; `Place` works on placement ids);
  - `internal/service/internal/leaf/jointoken/`, `internal/service/internal/leaf/node/join.go`, `internal/service/fakenodes.go` (fake workers get placements `fake-a`, `fake-b`) + `fakenodes_test.go`;
  - `internal/service/internal/store/node_admin.go`, `internal/service/internal/store/join_tokens.go`;
  - `internal/db/migrations/028_drop_node_access.{up,down}.sql`;
  - tests in `internal/service/`: `placement_test.go`, `nodeadmin_test.go`, `nodeapi_test.go`, `join*_test.go`, `ingressswitch_test.go`, `imports_test.go`, `noderedact_test.go`, `nodegate_test.go`, new `placements_test.go`.
- **Provides:** section 2 item 6 (lane A part). `IngressFor` gets D10 order: nearest override placement's node, then the env's node if tagged ingress, then the cluster default. `planfile.Fits` is the only tag rule.
- **Tests:**
  - migration conversion (pins to placements, envs' `placement_id`, open copied);
  - access (a closed placement refused to another org, admin anywhere);
  - fit (storage needed for volumes and managed tiles);
  - env create lands on the pinned placement's node, or with `node_id ''` when it has none (D6), and its deploy is refused;
  - `IngressFor` order;
  - a member view never holds a node name (scan the JSON).
- **Done:** W0 stubs replaced; all lane tests green; `go test ./internal/service/...` green except tests owned by M.

### Lane M: moves
- **Goal:** CI-style checks, placement repoint, env pin moves (D4), restore after forget (D8), partial failure and Retry, the D6 node set.
- **Owns:**
  - in `internal/service/`: `placementmove.go` (fill), `move.go`, `placement_move.go`, `move_test.go`, new `placementmove_test.go`;
  - `internal/service/internal/flow/move/` only if the flow needs a hook (report first);
  - `internal/service/internal/store/moves.go`.
- **Consumes:** `planfile.Fits`, lane A's `envNeeds`, store rows.
- **Check details:**
  - target online (`nodeUp`);
  - mesh link from source to target up (`meshSvc.Direct` or `Via`);
  - tags (`planfile.Fits` over every env's needs, plus ingress and egress when the placement is used as an override);
  - disk: target free >= sum of volume sizes + 10% (`diskOf`, `InspectVolume`);
  - memory: target `MemFree` >= sum of tile memory limits; tiles with no limit are a detail line, not a failure;
  - cpu arch: source == target `Arch`;
  - source online: fail "h3 offline: wait for it, or forget it to restore from backups".

  `MovePlacement` re-runs the checks, needs `confirm == name`, and writes
  `placement_moves`. It enqueues one live env move per env (tagged
  `placement_move_id`) and returns the first job without waiting. The
  `runMove` tail calls `placementMoveStep`: when every env is on the target,
  `placements.node_id = to` and the move is done; a rolled-back env marks it
  failed and the placement keeps the old node. Retry re-enqueues the envs
  still off target. A placement with no envs repoints at once. Boot resume
  stays as `resumeMoves`.

  Downtime: `ponytail:` heuristic, 5 s + 5 s per tile for a live move,
  documented as an estimate.
- **Tests:**
  - each check failing alone;
  - repoint happy path on two fake nodes;
  - one env failing leaves the placement on the old node, and Retry finishes it;
  - D6 node set deploys waiting envs;
  - D8 restore path;
  - D4 pin change refused with `*MoveNeeded`, then moved on confirm.
- **Done:** all of the above green; no `errs.ErrNotBuilt` left in lane files.

### Lane S: server file `nodes:` and `placements:`
- **Goal:** parse, diff, export and apply of nodes and placements, with gaps.
- **Owns:**
  - in `internal/service/internal/flow/serverconfig/`: `file.go`, `diff.go`, `export.go`, `plan.go`, new `placements.go`, and its tests;
  - `internal/service/serverconfig_apply.go` (`serverLive` fills `Nodes`, `Placements`, `Moves`, `MoveEnvs`; a new walk step `{"nodes and placements", w.placements}` after `orgs`, before `removals`), new `internal/service/serverconfig_placements.go`, `serverconfig_apply_test.go`, `serverconfig_test.go`.
- **Plan rules:**
  - unknown or pending node named by a placement: a Note "eu-prod waits for node h4", plus `Gap{Kind:"node", Key:"h4", Tags}` from `nodes:`;
  - joined node not in the file: a Note "h9 is not in the file (unmanaged)", never a row;
  - node tags differ: Change `node-tags`; removing the controller's ingress or storage follows the existing `SetNodeTags` refusal as a Blocker;
  - new placement: Change `placement`; open change: `placement-open`;
  - repoint, all checks OK: Change `{Kind:"placement-move", Tile:name, Field:"node", Old, New, Impact:"moves 3 envs (a/b/c, ...) to h4; each stops for about 20 s"}`;
  - repoint, any check fails: a Blocker listing the failed checks;
  - no node, to a node: Change `placement-node`, with no impact;
  - removed placement: D7;
  - unset server secret: `Gap{Kind:"secret", Scope:"server"}` in place of today's note.
- **Apply:** walk `nodes` with `SetNodeTags`. Then placements: create or open through lane A's verbs; a repoint calls `MovePlacement(..., confirm=name)`; a node set calls `placementNodeSet`. The move job is not awaited: it is kept out of `queued`. `newRisk` keys on `Kind|Tile|Field`, so an approved move stays approved on re-diff, and a check failing after the approve is a Blocker on the re-diff.
- **Tests:**
  - parse (the `open` forms, a bad name, an unknown tag);
  - every plan rule above;
  - export round trip;
  - apply starts the move job and the plan ends `applied` while it runs.
- **Done:** green; the export of the rig DB copy shows `nodes:` and `placements:`.

### Lane O: org and stack file pin, ingress and exits
- **Goal:** `pin`, `ingress` and `exits` in `stackr-org.yml` and stack files, with plan errors and warnings.
- **Owns:**
  - new `internal/service/internal/planfile/placements.go` + test;
  - in `internal/service/internal/flow/orgconfig/`: `file.go`, `diff.go`, `export.go`, new `placements.go`, and its tests;
  - in `internal/service/internal/flow/promote/`: `stackfile.go`, `plan.go`, `sync.go`, new `placements.go`, and its tests;
  - `internal/service/orgconfig.go` (`orgLive` fills `Placements`, `EnvNeeds`), `envsync.go`, `stackexport.go`, new `internal/service/configplacements.go`;
  - tests: `orgconfig_test.go`, `envsync_test.go`.
- **Plan rules** (one pure helper, `planfile.PlacementRef(ref string, need Needs, f PlacementFacts) (blocker, warning string)`, in a new `planfile/placements.go` owned by lane O; both the org diff and the stack plan call it, since flows never import flows):
  - Blockers: an undeclared placement; a placement not open to the org; a wrong tag (storage for an env with volumes or managed DBs; an ingress override whose node lacks ingress; an exit rung whose node lacks egress).
  - Warnings and Notes: a placement with no node ("new envs wait"); node offline ("new work waits").
  - Stack file: a pin change that moves an existing env becomes a Change with an Impact line, or a Blocker when `moveChecks` fail, using the facts the service passes; the apply calls `MoveEnvToPlacement` (D4).
  - An unset secret in org and stack plans becomes a `Gap`.
- **Tests:** each error and warning for the org file and the stack file; a pin applied to a new env; export writes the pins back.
- **Done:** green; `stackr-test`'s `stackr-org.yml` with `pin: eu-prod` plans clean on a fake cluster.

### Lane C: API and CLI
- **Goal:** the routes and commands in section 1; docs.
- **Owns:**
  - `internal/api/routes.go`; in `internal/api/handler/v1/`: `placement.go`, `nodes.go`, `join.go`, `orgs.go`, new `placements.go`;
  - `internal/authz/authz.go` (`node.list` becomes admin);
  - in `cmd/stackr/`: `placement.go`, `node.go`, `cmds.go`, `stack.go` (`--node` goes, `--placement` comes), new `placements.go`;
  - tests: `cmd/stackr/node_test.go`, `cli_test.go`, `internal/api/*_test.go`, `cmd/stackr/serverconnector_test.go`, `internal/api/serverconnector_test.go`;
  - `docs/guide/nodes.md`, new `docs/guide/placements.md` (includes the mixed env recipe and restore being docs only).
- **Consumes:** lanes A and M signatures.
- **Tests:** an API round trip per route; `stackr admin placement move` without `--yes` exits 3 and prints the checks; `stackr placements` as a member shows no node name.
- **Done:** green; the route list matches section 1; the integrator regenerates `docs/openapi.json`.

### Lane P: panel admin and plan gaps
- **Goal:** the `/-/admin/placements` page and move wizard, Add node prefill, gap rows on plan review, lost envs by placement.
- **Owns:**
  - in `internal/web/handler/admin/`: new `placements.go`, `nodes.go`, `config.go`, each with its test;
  - new `internal/ui/drawer/admin/placements.templ`, `ui/drawer/admin/nodes.templ`, `ui/drawer/admin/admin.templ` (tab), `ui/drawer/admin/config.templ`;
  - `ui/components/plan.templ` (`PlanView.Gaps`, gap rows);
  - the org plan review templ in `ui/drawer/org/` (gap rows only; report if it is shared with lane U).
- **Behaviour:**
  - The move wizard has step 1 (pick node), step 2 (checks rendered as a CI list, pass or fail, with detail; Apply disabled until all pass, with a Re-check button), and step 3 (a confirm dialog naming from, to, envs and downtime, typing the placement name). It ends on the progress view (`PlacementMove`), with Retry when it failed.
  - Gap rows: a secret field posts to the existing param route, then the plan re-plans; "Add this node" opens `/-/admin/nodes/add?name=h4&tags=ingress,egress`.
  - Join confirm of a node the bound server file names queues a server re-plan (`kindServerPlan`), so a placement waiting on it gets its node without a push. The service line is lane A's (`join.go` confirm); lane P only shows "re-planning" after confirm.
- **Tests:** handler tests for each route; templ unit tests for the check list and gap rows; `make templint` clean in the worktree (`hamr lint templ`).
- **Done:** green; route mount lines reported for `server.go`.

### Lane U: server first-run setup and the dev-facing Placement tab
- **Goal:** the `/-/setup/server` wizard (by hand or server file; no restore option), and the per-scope Placement tab reworked to placements.
- **Owns:**
  - `internal/service/serversetup.go` (fill) + test;
  - new `internal/web/handler/setup/server.go` + test, new `internal/ui/pages/setup/server.templ`;
  - `internal/web/handler/placement/handler.go` + test, `internal/ui/placement/placement.templ`;
  - `internal/web/handler/canvas/create.go` (only if the gate needs it there);
  - D3 member HTML: `internal/web/handler/cluster/` + tests (admin only), and the member templ files that print a node today: `ui/components/layout.templ` (NodeDown banner), `ui/components/drawer_view.templ`, `ui/drawer/tile/tile.templ`, `ui/drawer/volume/volume.templ`, `ui/drawer/instance/instance.templ`, `ui/drawer/stack/stack.templ`, `ui/drawer/slice/slice.templ`. Lane A fills placement names into the member view structs; lane U relabels and adds an HTML test that an owner's pages carry no node name.
- **Behaviour, setup:**
  - An admin's first request while `server_setup_done_at` is unset redirects to `/-/setup/server`. This is a middleware `setup.ServerPending`, mounted by the integrator; it runs before org setup.
  - Branch "By hand": domains (root, panel, ACME email through `SetSetting`), a backup destination (the existing verb), and a placements summary with a link to the admin page.
  - Branch "From a server file": connector, bind (`BindServerConfig`), then plan, approve, done.
  - Then org setup as today.
  - Migration 027 stamps `server_setup_done_at` when any org exists, so the rig skips it.
- **Behaviour, the tab:**
  - pin select (placements open to the org, each with its fit hint);
  - ingress and exits pick placements;
  - the env pin change shows the D4 move plan and confirm;
  - org level lists `OrgPlacements` (name, tags, public IP, online, usable);
  - no node names for members.
- **Tests:** gate redirect for an admin only; both branches end with done stamped; the tab renders no node name for an owner.
- **Done:** green; mount lines reported.

### File ownership check
No file appears in two lanes. Shared registration is handled this way:
- `server.go` and `orchestrator.go`: the integrator;
- `routes.go`, `cmds.go`, `authz.go`: lane C;
- job kind registration for the placement move: lane M reports the line to add in `jobs.go` if one is needed, and the integrator applies it.

### Lane prompt (paste one per lane, filling LANE and the worktree path)
```
You are lane LANE of the stackr placements build. Worktree:
/home/darthvader/FyrmForge/stackr-LANE (branch placements-LANE from the
Wave 0 commit). Read, in this order:
/home/darthvader/FyrmForge/stackr/AGENTS.md,
/home/darthvader/FyrmForge/stackr/docs/rewrite/tasks/placements.md,
/home/darthvader/FyrmForge/stackr/docs/rewrite/tasks/placements-build.md
(sections 0 to 3; your lane is "Lane LANE"). Build exactly your lane: its
goal, its file list, its contract, its tests. Follow "Rules for every lane".
Edit only your files; patches for others go in your report. No git writes.
No em dashes. Never print secrets. Test with go build ./..., go vet and
go test on your packages inside your worktree. When done, report: files
changed, tests and results, anything not done, patches for files you do
not own.
```

## 4. Integration

1. W0 commit on `placements` (section 2 item 9).
2. Lanes run in parallel: A, M, S, O, C, P, U.
3. Merge onto an integration worktree of `placements`, in this order:
   A, M, S, O, C, P, U. After each merge: `go build ./...` and the lane's
   tests. Apply the reported patches and mount lines (`server.go`,
   `orchestrator.go`, `jobs.go`).
4. Search for leftovers: `grep -rn ErrNotBuilt internal/` must be empty;
   so must `grep -rn "NodeOrgs\|OpenToAll" internal/ cmd/` (outside
   migrations).
5. Regenerate everything generated: `templ generate`; `cd ui && npm run
   css:build && npm run ts:build`; `hamr gen static`; `go run ./cmd/stackrd
   --dump-openapi > docs/openapi.json`; `docs/llms*.txt` with the repo's
   generator.
6. Run the gates in the integration worktree:
   - `go test -timeout 20m ./...` (make test);
   - `/tmp/claude-1000/-home-darthvader-FyrmForge-stackr/10d11dff-c68c-46af-81a5-851d1685d8e0/scratchpad/bin/golangci-lint run` (lint 2.14, depguard included);
   - `cd ui && npm run ts:check`;
   - `hamr lint templ`.

   In the main checkout, build only through hamr MCP (`make.run`), never
   from Bash.
7. Run the `handler-audit` skill over `handler/admin/placements.go`,
   `handler/placement`, `handler/setup/server.go` and `api/handler/v1/placements.go`.
8. Merge `placements` into `multi-node` only when darthvader says so. The
   rig build is `v0.6.0-dev.N`, never a tag.

### Rig proof checklist
Rig: controller CT 109 (`stackr-lxc.vulpe.dev`), CT 110, VM 106, h4
(77.42.15.115, tagged ingress), h3 (2.29.101.116).
Test repo `FyrmForge/stackr-test` (`stackr-org.yml`, `stacks/*.yml`).
1. Upgrade the controller and workers. Check the migration's result:
   `stackr admin placement ls` shows `default` on the controller, plus one
   placement per pinned or used worker; envs are unchanged; no plan errors on
   re-plan of `stackr-test`.
2. Server file with `nodes:` (tags) and `placements:` (`eu-prod` on h4,
   `eu-db` on h3 open to the test org), then apply. Expect:
   - placements created;
   - a node in the file and not joined shows a gap with "Add this node"; the prefilled dialog shows the curl line;
   - CT 110 not in the file shows as unmanaged.
3. Org file `pin: eu-prod`; a stack with a DB in its own stack `pin: eu-db`.
   Expect:
   - plan errors: an undeclared name, a closed placement (from a second org), the DB stack pinned to a node without storage;
   - warnings: a placement with no node, h3 stopped.
4. Repoint `eu-prod` from h4 to VM 106 in the file:
   - the plan is BLOCKED while VM 106 is stopped;
   - it is unblocked once VM 106 is up;
   - Apply shows the confirm with envs and downtime;
   - the job runs and the apply ends without waiting;
   - apps answer from 106.
5. Partial failure: fill the disk on the target mid move (or stop the target
   during the second env). Expect the placement to stay on the old node, the
   wizard to show failed, and Retry to finish.
6. Offline source: stop h3 and repoint `eu-db`. Expect the check "source
   online" to fail. Then forget h3, repoint, restore from backups (D8), and
   the data to come back.
7. Dev view as a member of the test org: `stackr placements` and the org
   Placement tab list `eu-prod` and `eu-db` with tags, public IP, online and
   usable. No node name appears anywhere (`/nodes` returns 403).
8. Ingress follows placement (D10): an env on h4 serves its route through
   h4's Caddy with no override; an override `ingress: default` moves it back
   to the controller.
9. First-run setup: wipe and reinstall the test rig (disposable). The first
   admin login lands on server setup. Run the file branch once and the
   by-hand branch once (wipe between). The UI has no restore option.

## 5. Risks and contradictions in the code

- **The existing `placement` handler is the per-scope tab**, not a
  placements page. It is reworked by lane U; the admin page is new (lane P).
  `service/placement.go` (724 lines) is lane A's alone.
- **`MoveEnv` checks authz inside the service** (`authz.Can(...
  "placement.write")` in `move.go`). That breaks AGENTS.md's "auth is
  middleware, never a service". Lane M moves it to the route; a placement
  repoint is admin (`node.manage`) and spans orgs.
- **`applyServerPlan` ends in `queued.await`.** The placement move job must
  stay out of that wait, or the apply blocks on a live rsync (lane S).
- **`newRisk` refuses new impact lines on re-diff.** The move impact must
  keep a stable `Kind|Tile|Field` (`placement-move|<name>|node`). Otherwise
  an approved move is refused when a byte count changes the text.
- **Checks go stale.** A plan blocked at plan time does not re-check by
  itself; the wizard has Re-check and the file path re-plans. A target that
  goes down after the approve is caught by the apply's re-diff (Blocker),
  then by the env move itself.
- **D10 changes live ingress for every env on an ingress-tagged worker** at
  upgrade. The DNS for those hosts must point there. The default-ingress
  switch already lists hosts that do not; the placement move checks do not
  look at DNS. Flagged, not built.
- **`environments.node_id = ''`** was "a row from before nodes", and `onNode`
  leaves ctx alone, which the router then refuses. D6 gives '' a meaning
  again. Lane A must make every Docker path for such an env refuse with the
  placement message, before the router error.
- **Ladder rungs by placement.** `meshSvc.RemoveNode` and `exitGuard` prune
  and guard by node id. After 027 they must resolve placements on the node
  (lane A).
- **Join tokens' org list.** The Add node dialog, `stackr node add`, and
  `installer join` all carry "open to" today. D1 removes it on every surface
  (lanes A, C, P).
- **SQLite FK columns** (`orgs.node_id`, `stacks.node_id`) cannot be dropped
  without a rebuild (D13). The next squash must remove them, as must the
  store mapping, which W0 already drops.
- **Test churn.** These tests reference removed APIs and are assigned to
  lanes A, M and C: `placement_test.go`, `nodeadmin_test.go`,
  `nodeapi_test.go`, `move_test.go`, `ingressswitch_test.go`,
  `joinscript_test.go`, `imports_test.go`, `store/placement_test.go` (W0),
  `cmd/stackr/serverconnector_test.go`, `internal/api/serverconnector_test.go`.
- **Fake mode:** `fakenodes.go` makes two fake workers, which need
  placements so `hamr dev` stays usable. W0 seeds `default` only; lane A adds
  `fake-a` and `fake-b`.

## W0 notes

What Wave 0 did differently from section 2, or added where the plan was silent:

- **Pins need a two-way shim, not renames only.** `orgs.placement_id` and
  `stacks.placement_id` reference `placements`, so `SetPin` cannot keep writing
  node ids. `internal/service/placements.go` has a block marked "W0 shim":
  - `nodePlacement` on write (`SetPin`): the controller maps to `default`; a
    worker maps to the placement on it, made and named after the node when it
    has none;
  - `placementNode`, `pinNode` and `ingressPick` on read: a placement id
    becomes its node, any other id (a node id, `own`) passes through.

  `SetIngress` and `SetExits` still write node ids (those tables have no FK),
  and every reader accepts both. Lane A deletes the shim.
- **The ladder chokepoint is `scopeLadder`** in `nodeexit.go`, which the mesh
  reaches through `envExits`. `ladders()` in `placement.go` now calls it.
  Rungs on a placement with no node are dropped.
- **`ExitLadders.ListByNode` became `ListByTarget`.** Callers that mean "this
  node" loop over `nodeTargets`: the node id plus every placement on it.
- **A fresh database has no controller row when 027 runs.** `default` is
  created with `node_id = ''`; the boot path (`adoptDefault` in
  `orchestrator.go`) and `storetest.Controller` set it to the controller.
- **Forgotten nodes get a placement.** One per forgotten node that envs still
  sit on, named after `forgotten_nodes.name` and skipped on a name clash, so
  D8 has something to repoint. A worker named `default` gets a placement
  named `node-default`.
- **There is no "stack plan input" struct in promote.** `promote.Flow` gains
  `Placements func(ctx, envID) (promote.PlacementLive, error)` (nil = no
  checks), where `PlacementLive` is `{Placements, EnvNeeds, Moves}`. It is
  unwired: lane O writes the gatherer and reports the `orchestrator.go`
  line. `orgconfig.Live` carries the three fields directly.
- **The shared planfile types live in `planfile.go`**, as section 2 says, with
  `Fits` tested in `fits_test.go`. That leaves `planfile/placements.go` free
  for lane O.
- **`Needs` is `{Storage, Ingress, Egress bool}`.** The plan did not name its
  fields.
- **`storage` is also in the admin `NodeTags`** (`nodeadmin.go`), not just in
  `joinTags`, so admins and lane S can set it.
- **`server_setup_done_at` is an RFC 3339 UTC string**
  (`2026-10-16T12:00:00Z`).
- **`PlacementMoveStore.SetState(ctx, id, state, errMsg, finishedAt)`** writes
  those three columns only.
- **Envs made after 027 have `placement_id = ''`** until lane A's env leaf
  sets it. `Environments.Update` never writes `placement_id`;
  `SetPlacement` is its one writer.
- **D13 finding.** SQLite's `DROP COLUMN` does work on a column-level
  `REFERENCES`: 011's down already drops `orgs.node_id`, and 027's down drops
  `placement_id` the same way. D13 is kept as written. The dead columns could
  go in 028 instead of waiting for a squash.
- **Unexported stubs carry `//nolint:unused // lane X`.** Drop the comment
  together with the stub.

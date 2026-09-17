# Plan: unset values, the whole chain

Status: implemented 2026-09-03. Both open points decided 2026-09-17; the
not-set row built the same day and verified on the rig.

Working rules: discuss first, one point at a time, no code without a go, no
git writes, no edits to `*_templ.go` or `output.css`, terse UI copy.

## Where this came from

QA on the test box, 2026-09-03. A stack file declared `SMTP_PASSWORD` as a
secret with no `generated` default. The plan page showed it as an amber input
row, apply went through, and the tile that read it parked as "Waiting for
SMTP_PASSWORD". Two things read wrong:

1. The row looked like a warning. It stops a deployment, so it should look
   like one, without holding up the apply.
2. Nothing said what else would not come up. A tile with `depends_on` on the
   waiting tile either fails red after two minutes (`:healthy`, `:completed`)
   or starts blind against a tile that never came up (`:started`).

## Decisions (2026-09-03)

- **Apply still goes through.** A declared value with no value is an input,
  not a blocker. Approve stays enabled. Unchanged from 2026-09-02.
- **The input row is an error visually.** Red border, red name, one line
  saying it is needed before the tile can deploy. Approve is not tied to it.
- **The row expands to show the blast radius.** The tiles that read the
  value, and everything that depends on those tiles, transitively, within the
  env. That is the list of what will not deploy until the value is set.
- **Dependents wait too.** At deploy time a tile whose `depends_on` names a
  tile parked in `waiting:NAME` parks on the same name instead of failing or
  starting. Setting the value releases the whole chain, since `ClearWaiting`
  already matches on the name.

## Implementation

Three phases, each deployable on its own. A is copy and CSS, B is the
planner, C is the engine.

### A. Red input row

`internal/stackrd/handlers/web/components/plan.templ`,
`internal/stackrd/config/stackconf/plan.go`

- `plan.templ` line ~184: the `templ.KV("border-rw-warning/40", ch.Input)`
  becomes the error border, and the `ch.New` span (line ~252) goes
  `text-rw-error` when `ch.Input`. Same classes the `Errors` panel above
  already uses.
- `InputChange` note becomes: "Needed before the tiles below can deploy.
  Apply goes ahead without it." Required stays as the "Required. " prefix.
- Approve logic untouched: `canApprove` still keys on `len(Errors) > 0`
  only.

Test: `components/plan_test.go` already renders an input row; assert the
error class on it instead of the warning one.

### B. Readers and dependents on the row

`internal/stackrd/config/stackconf/plan.go`, `runner.go`,
`internal/stackrd/handlers/web/components/plan.templ`

- `Input` gains `Blocked []string`, entries as `env/tile`, sorted. Readers
  and their transitive dependents in one list; the row does not need to
  distinguish them.
- `DeclaredInputs` (`runner.go` ~436) fills it: for each env in
  `r.EnvOrder`, walk `re.Tiles`, and a tile whose `Env` values contain a
  `varref.Refs` body parsing to `stack.NAME` (or `org.NAME`) is a reader.
  Then close over `DependsOn` in the same env (`ParseDep` from `deps.go` for
  the slug) until no new tile is added. `re.Tiles` is the resolved config,
  so no store reads.
- `InputChange` takes the list and writes it into `Fields` as one
  `{Key: "will not deploy", Value: strings.Join(blocked, ", ")}` line. The
  accordion already renders `Fields`, so `plan.templ` needs no new markup.
  Empty list: no field, the row still opens for the Set box.
- Org plans: `orgconf` builds its plan through the same `stackconf.Plan` and
  only carries org-scope inputs, whose readers live in stacks that may not
  exist yet. Left as today: no blocked list on org-scope rows.

Test: `runner_test.go`, one env with `web` reading
`${{ stack.SMTP_PASSWORD }}`, `worker` with `depends_on: web`, `db` with
neither. The input's `Blocked` is `[production/web production/worker]`.

### C. Dependents park on the same name

`internal/stackrd/infra/deploy/engine.go`,
`internal/stackrd/config/stackconf/apply.go`, `deploy/waiting.go`

- `engine.go` pipeline, before `rr.Resolve` (~445): for every line of
  `app.DependsOn`, `ParseDep` the slug, `GetTileBySlug(ctx, app.EnvironmentID, slug)`,
  and if `WaitingFor(dep.Status) != ""` return
  `&varref.UnsetError{Scope: "dep", Name: name}` wrapped with the dep's
  slug in the text. The existing `varref.Unset(err)` branch at line ~330
  then stores `waiting:NAME` on this tile with no further change. The log
  line says "waiting: NAME (via web)". This runs on every deploy trigger,
  manual Deploy included, so the chain holds outside apply too.
- `apply.go` `waitDeps` (~677): inside the poll loop, if
  `WaitingFor(dep.Status) != ""` return the same `UnsetError` at once
  instead of polling to the deadline. The tile's enqueue still happens; the
  engine parks it by the rule above. This only cuts the two-minute wait.
- `ParseDep` and `WaitingFor` live in different packages; `deploy` already
  imports nothing from `stackconf`, and `stackconf` imports `deploy`. Move
  `ParseDep` to a tiny leaf package only if the import cycle bites; a copy
  of the eight-line parser in `deploy` is the simpler answer.
- Release: `ClearWaiting`, `ClearWaitingEnv` and `ClearWaitingOrg` compare
  `WaitingFor(t.Status) == name`, so dependents parked on the same name go
  to `stopped` with the reader. Nothing to change. Order of the follow-up
  deploys is the operator's, same as today: setting a value is not a request
  to ship.

Test: `waiting_test.go`, a tile whose dependency row is `waiting:X` parks as
`waiting:X` without the resolver being reached.

## Decided 2026-09-17

- **Nested plans: left as is.** Org-scope input rows carry no blocked list.
  Each stack's own plan shows it once the stack exists. Building it on the
  org plan would mean fetching every stack's repo during an org plan.
- **Secrets panel: show a "not set" row.** A declared secret with no value
  gets a red row in the stack Variables panel, with a Set box. Plan below.

## Plan: the not-set row

Built 2026-09-17. Differences from below:

- `LatestSettledConfigPlan` reads the newest `applied` **or `clean`** plan.
  A clean plan never has inputs (an input row makes a plan non-empty), so it
  correctly says nothing is missing any more.
- The filter drops names set at the stack or the org. An env-only value for
  a per-env secret still shows as not set until the next plan settles.
- Tested as a unit (`handler/project/unset_test.go`) and a store test
  (`TestLatestSettledConfigPlan`) instead of a handler render test. Checked
  in the browser: row renders, Set opens the edit row, saving replaces it.
- Rig, 2026-09-17: `test-org/stackr-test` declares `SMTP_PASSWORD`. No row
  while its plan was pending; after the apply the row showed, and Set with
  Generate replaced it with a normal secret.

### Where the declared names come from (decided 2026-09-17: A)

The panel has no config file in hand. Two store-only sources:

- **A. The last applied stack plan.** `repo.ConfigPlan.Plan` is the JSON
  `stackconf.Plan`, which already has `Inputs` (name, scope, secret,
  required, blocked). Read the newest `applied` row, drop names that now
  have a value. Knows everything the plan page knew. A stack with no config
  file has no plan and no rows, which is right.
- **B. Waiting tiles.** Tiles parked as `waiting:NAME`
  (`internal/stackrd/infra/deploy/waiting.go`). Always current, but it
  misses a secret no tile has tried to deploy with yet, and cannot tell a
  secret from a var or stack scope from org.

### Touch points

- `internal/stackrd/store/repo/repo.go` + `sqlite/stacks.go`: a
  `LatestAppliedConfigPlan(ctx, stackID)`, unless `ListConfigPlans` plus a
  status filter is enough (check before adding).
- `internal/stackrd/handlers/web/handler/project/handler.go`
  `renderStackVars`: load the plan, decode `Inputs`, keep `Scope == "stack"`
  names missing from `vars`, pass them in `VarsEditCfg`.
- `internal/stackrd/handlers/web/components/varsedit.templ`: in page mode,
  render those names in the Secrets group as red "Not set" rows whose edit
  opens the existing Set flow. Blocked tiles as one short line.
- Org Variables panel (`handler/org/graph.go`): nothing, per the
  nested-plans decision.

Test: handler test, an applied plan with an input `SMTP_PASSWORD` and no
variable renders a not-set row; setting the variable removes it.

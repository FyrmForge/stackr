# Plan: unset values, the whole chain

Status: implemented 2026-09-03. The secrets-panel question under Open is still open.

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

## Open

- **Secrets panel.** An unset declared secret has no row, so the env
  secrets panel does not list it (seen on the test box: `SESSION_SECRET`
  shows, `SMTP_PASSWORD` does not). Showing it as a "not set" row means
  `setVarNames` has to treat an empty value as unset. Not decided.
- **Nested plans.** Org-scope inputs still cannot name their readers. Same
  parking lot as before.

# Step 7: org config file

Read first: `docs/rewrite/PROGRESS.md`, `AGENTS.md` "Layering rules",
REWRITE.md "Org config file" (the design; every rule below is there) and
"Promote and releases" (the stack file it mirrors). Code to mirror:
`internal/service/internal/flow/promote/stackfile.go` (parse, strict
decode, hints), `flow/promote/plan.go` (`Change`, `Plan`, blockers),
`internal/service/stack.go` (`SetConfigRepo`, `Webhook`),
`internal/service/wiring.go` (`stackFile`: clone + read at a commit),
`internal/service/internal/leaf/job` (a leaf that owns a table with a
status machine), `internal/web/handler/canvas/stack.go` +
`internal/ui/drawer/stack/` (the Config as code section and its routes).
v0 for the look only: `stackr-old .../handler/org/config.templ`,
`plans.templ` (the plan page and the canvas banner).

What it is: an org bound to a git repo whose `stackr-org.yml` declares
the org's name, params, defaults, env colors and stacks. Every plan is a
row; an owner approves or rejects it, or the binding's Auto switch
approves unblocked plans on its own. Apply is one job that calls the
orchestrator's own verbs.

Branch `rewrite-step-7`, stacked on `rewrite-step-7a`.

## Tasks

1. **Store.** `orgs` gets `config_connector_id`, `config_repo`,
   `config_branch`, `config_path`, `config_auto` (bool, default false);
   new table
   `org_config_plans` (id, org_id FK ON DELETE CASCADE, commit, summary,
   plan JSON, status CHECK in the six, error, created_at, decided_at;
   index on org_id, created_at DESC). Edit
   `internal/db/migrations/003_rest` in place, up and down (no installs
   exist); `store.Org`, `store.Stack`, `store.OrgPlan` + `newTable`;
   `Tables` and `bind`; `internal/db/cascade_test.go` inserts updated and
   a case that deleting the org deletes its plans.
   Done when: store round-trip tests on both; cascade test green.

2. **One repo spelling.** `leaf/stack.SetConfigRepo` and the new org bind
   normalize `owner/name` to `https://github.com/owner/name` before
   storing; `wiring.clone` resolves the connector by the stored id when
   set, by host otherwise. Root-cause fix for the stack drawer's
   `owner/name` hint against `connector.For`'s host lookup.
   Done when: a stack bound as `acme/shop` from the drawer clones; test on
   the leaf for both spellings; the drawer hint says either is fine.

3. **`flow/orgconfig`.** `Parse(data) (*File, error)`: strict YAML,
   `version: 1`, `org:` required, the stack file's `humanYAML` hints;
   `stacks.<slug>` takes `repo`/`branch`/`path`/`connector` or `path`
   alone (the org repo); any other stack key is refused with "inline
   stacks are not supported; put the stack in its own file"; `moved:` is
   a list of `from`/`to`, each `stack.<slug>` (`shared.` dropped by
   DECIDE 193). `Diff(f
   *File, live Live) Plan` with `Live{Org, Stacks []StackLive, Params, Domains,
   Settings, EnvColors}` and `Plan{Changes, Blockers, Notes}` reusing
   promote's `Change` shape (kinds: `org`, `param`, `param-update`,
   `defaults`, `colors`, `create`, `rebind`, `rename`, `domain`,
   `domain-update`; the
   `instance` kinds dropped by DECIDE 193), `Summary()` as v0's
   plan had. The rules are the design's "The diff" bullet, verbatim.
   Pure: no store, no clone.
   Done when: table tests: rename; rename collides → blocker; param
   create with value; secret declared without value → note; param not in
   file → no change; defaults and colors only when present; stack create;
   rebind; a stack gone from the file → no change, hand-made or file-made
   alike (DECIDE 188); host without connector → blocker;
   inline stack → parse error; `shared:` → unknown key (DECIDE 193);
   `moved:` stack → rename and
   the `stacks.<to>` entry diffs against it (no create); `to` exists and
   `from` gone → no change; both exist → blocker; neither → blocker;
   `shared.` in `moved:` → parse error; `domains:` entry missing → domain
   create; env flag or ACME differs → domain update; entry gone → no
   change; host taken or squatting → blocker; a JSON round trip of `Plan`.

4. **`leaf/orgplan`.** Owns `org_config_plans`: `Create(ctx, p)` (marks
   the org's pending and clean rows superseded in the same tx), `Get`,
   `ForOrg(ctx, orgID, limit)`, `SetStatus(ctx, id, status)` (approve and
   reject only from pending; applied only from pending; `Conflict`
   otherwise), `SetError(ctx, id, msg)` (status error + message),
   `RejectPending(ctx, orgID)` for unbind. Statuses as the design lists.
   Done when: leaf tests for the state machine and the supersede.

5. **Org rung setters.** `leaf/org`: `SetSettings(id, blob)` and
   `SetEnvColors(id, json)`; orchestrator `SetOrgSettings` and
   `SetOrgEnvColors`, both redeploying the org's running tiles as
   `SetStackSettings` does; `canvas/cascade.go` `saveRung` wired for the
   org so the org drawer's Defaults section saves (closes DECIDE 166).
   Done when: service test; drawer test posts a rung and reads it back.

6. **The stack file makes the envs (DECIDE 189).** In `runPush`
   (`internal/service/jobs.go`), when the push is to the stack's config
   repo and branch: read the file at `ev.Commit` through `stackFile`,
   resolve it, and for every name in `Order` the stack's ladder lacks
   call `o.envs.Create` with `Spec{Type: Static, FromKind, FromBranch,
   Auto, Color}` from its `ResolvedEnv`, bottom rung first, logging each;
   then `promote.Push` as today. An unparsable file logs the error and
   pushes as today (the promote plan already reports it). Envs are never
   deleted here. Not a flow change: `flow/promote` `Push` keeps returning
   nothing when no env tracks the branch.
   Done when: service test with the git fake: a push to a bound stack
   with no envs creates dev and prod from `ladder: [dev, prod]` +
   `head: main`, dev tracks main auto, prod promotes, and a release lands
   in dev; a second push creates nothing; an env dropped from the file
   stays.

7. **Verbs.** `SetOrgConfigRepo(ctx, orgID, connectorID, repo, branch,
   path string, auto bool)` (empty repo unbinds: clears the five columns
   and rejects the pending plan; a bind plans at once, as v0's Save and
   plan). `planOrg(ctx, orgID) (OrgPlan, error)`: clone into
   `repos/orgconfig-<org>` under `repoLock`, read at head, build `Live`,
   `Diff`, store the row with the commit (a parse error stores `error`),
   and when `config_auto` is on and the plan is pending and unblocked,
   approve it. `PlanOrgConfig(ctx, orgID)` calls it synchronously (as
   `PlanPromote` does); `PreviewOrgConfig(ctx, orgID, file []byte)`
   diffs the bytes and stores nothing (unbound → `Conflict`, as v0's
   409); `OrgPlans(ctx, orgID, limit)`, `OrgPlan(ctx, id)`.
   `ApproveOrgPlan(ctx, id) (Job, error)`: refused unless pending and
   unblocked, else `SetStatus` and enqueue `kindOrgApply{PlanID}` with
   lock `orgconfig:<org>`; the handler refetches at the plan's commit,
   re-diffs, walks the changes calling `RenameStack` for
   `moved:` first, then `RenameOrg`, `SetParams`
   (`ParamScope{Kind: "org"}`), `SetOrgSettings`, `SetOrgEnvColors`,
   `CreateStack` + `SetConfigRepo` (the shared-instance walk dropped by
   DECIDE 193), then `CreateDomainResource` (org level) or
   `UpdateDomainResource` per `domains:` entry,
   then enqueues the webhook's push job for every created or rebound
   stack at its branch head (the plan's commit for a `path:` stack), then
   `applied`; any error → `SetError`. `RejectOrgPlan(ctx, id)`.
   `ExportOrgConfig(ctx, orgID) ([]byte, error)`. `orgof.jobOrg` learns
   `org_id`. `docs/rewrite/verbs.md` row.
   Done when: service tests with the git fake: plan from head stores a
   pending row and supersedes the last; a bad file stores `error`;
   preview stores nothing; approve creates and binds a stack, sets params
   and colors, marks applied; a blocked approve is refused with the
   blocker text; a second approve is refused; reject; auto on: the plan
   job applies without a call; export of the applied org previews clean.

8. **Webhook.** In `Webhook`, before the stack loop: a push whose repo
   and branch match the org's binding enqueues `kindOrgPlan{OrgID}` with
   lock `orgconfig:<org>` (a queued one is superseded, the Railway rule);
   the handler calls `planOrg`. No other event touches the org.
   Done when: test with the connector fake: a push to the org repo queues
   the plan job and nothing else; a push to a stack repo queues no org
   plan.

9. **API + CLI.** Routes, v0's paths: `PUT /orgs/:org/config-repo`
   (`org.config.bind`), `POST /orgs/:org/config/plan` (`org.config.bind`,
   returns the row), `POST /orgs/:org/config/plan-preview` body `{file}`
   (2 MiB cap), `GET /orgs/:org/config/plans` (last 20), `GET
   /org-config/plans/:id`, `POST /org-config/plans/:id/approve` → 202
   job (`orgplan.approve`), `POST /org-config/plans/:id/reject`, `GET
   /orgs/:org/config/export` (`org.config.export`); `docs/openapi.json`
   regenerated. CLI, v0's names: `stackr org config-repo --repo --branch
   --path --connector [--auto]`, `stackr org plan`, `stackr org preview
   -f FILE [--detailed-exitcode]`, `stackr org plans`, `stackr org
   plan-show <plan>`, `stackr org approve <plan>` (follows the job),
   `stackr org reject <plan>`, `stackr org
   export [-o FILE --force]`. `TestEveryRouteHasAVerb` clean.
   Done when: api tests for the eight routes incl. a member getting 403
   on plan, approve and reject and 200 on export; cli test for the ops.

10. **UI.** Org drawer tab `config` (`internal/ui/drawer/org/`,
   `canvas/org.go` routes `/config`, `/unbind`, `/plan`,
   `/plans/:id/approve`, `/plans/:id/reject`): v0's Config as code section
   (Export link; connector select listing the org's connectors with
   "(unbind)", repository, branch, path, Auto apply switch; Save and plan
   / Plan now), the latest plan under it through `planView` +
   `comp.PromoteAsk` with Approve and Reject for `orgplan.approve` holders,
   the queued job through
   `render.JobView`, then the last plans as rows (status, summary, when,
   commit short sha). Graph: `EdgeConfig` from the connector card to the
   org level (`flow/graph` `org()`), and the org canvas banner from the
   latest plan: pending → "Config plan pending" linking to the tab, error
   → "Config invalid" (v0's `orgPlanBanner` look). Params tab and
   Defaults section stay as they are.
   Done when: drawer tests: bind, plan renders the changes, viewer sees
   no Approve, approve posts and shows the job, reject; canvas snapshot
   with the edge and each banner; `make templint` clean.

11. **Org setup wizard (DECIDE 187).** A one-to-one copy of v0: the same
    pages, copy, stepper, switch links and skip paths, nothing
    redesigned. v0's `handler/org/setup.go` +
    `setup.templ` cloned as `internal/web/handler/setup/` + `internal/ui/
    pages/setup/`: `GET /setup` (the branch question, by hand or from a
    config file; "+ Create organization" links here and the `/-/new-org`
    dialog is removed from `graph.View.Create`), `POST /setup` makes the
    draft (`CreateOrg`) and records the branch in `Settings`; steps under
    `/:org/-/setup/:step`, owner only: config branch `connector` (the
    org's connectors, or begin one as the drawer does) → `config` (the
    bind form, `SetOrgConfigRepo`; the plan on the same step through
    `planView` + `comp.PromoteAsk`, Approve runs the apply job and the
    step waits on it through `render.JobView`; the file names the org) →
    `team` (v0's people panel over the members tab's routes) → `done`
    (the summary rows and Finish → `FinishOrg`); by hand `name`
    (`RenameOrg`) → `connector` → `domain` (v0's `setupDomainPage` and
    `SaveOrgDomain` over step 7a's domain resources) → `team` →
    `done`. v0's switch links
    between the branches (switching to by hand clears the binding and
    rejects the pending plan). The gate: a request into an unfinished org
    sends an owner to `/:org/-/setup/done` and shows anyone else v0's
    holding page (`setuppending.go`), canvas and drawer included.
    Done when: handler tests walk both branches to a finished org; a
    member on an unfinished org sees the holding page; the config branch
    with the git fake ends with the org named from the file and one stack
    bound; `make templint` clean.

12. **VM proof.** Bind the smoke org to the VM-connector test repo
    (memory: org-config-test-repo) at a branch holding a `stackr-org.yml`
    that declares one stack by `repo` and one param; push; banner shows;
    Config tab shows create + param; Approve; the stack appears bound, its ladder envs exist and
    its own push lands a release (the `shared:` postgres check dropped by
    DECIDE 193); switch Auto on,
    push a second param,
    it lands without a click; make a stack by hand and drop the declared
    stack from the file → the next plan touches neither; rename the
    declared stack's key with a `moved:` entry → renamed, nothing created.
    A `domains:` entry comes up as an org domain resource and a tile with
    `auto: true` is named under it. Then a new org through the wizard's
    config branch: bound, planned, approved, named from the file,
    finished. Playwright, deep links,
    snapshot asserts.

13. **Done gate.** `make build`, `make lint`, `make test`, `make
    templint`; PROGRESS.md ticks and the step 7 line; local commit;
    stacked PR only on "commit and push".

## Not in this step

Inline stacks, storage shares, a second approval for a bound stack's own file, PR
comments or check runs on the org repo. Each has a DECIDE item (182,
185) or is Later.

# Leftovers: build plan

Status: built 2026-10-06, on the rig as v0.6.0-dev.13, QA passed.

Sources: `ux-pass.md` "Not in this pass", `env-sync-build.md` known limits,
`PROGRESS.md` DECIDE block, rig QA notes. Every item below was traced in the
code; the fix is decided here. Paths are repo-relative. `flow/`, `leaf/` and
`store/` are under `internal/service/internal/`.

## How the work runs
- One worker per package; a package owns its files and touches no other.
  No two workers in one wave share a file.
- The orchestrator (`internal/service`) is the only door; handlers call verbs.
- Generated files are regenerated, never edited: `*_templ.go` (`templ
  generate`, or `make build` on the dev box runs it), `docs/openapi.json`
  (`go run ./cmd/stackrd --dump-openapi > docs/openapi.json`),
  `ui/static/js/elements/*`, `output.css`, `staticmanifest.go`.
- Every package ends green on `make test` and `make lint`; web packages also
  `make templint`. One test per behaviour change, stdlib testing, real SQLite
  (`servicetest.New`, `webtest.New`, the CLI fake server in `cli_test.go`).
- No git write ops. Changes stay unstaged for darthvader.
- No em dashes in copy. Plain punctuation.

## Items and decisions

### A1. Parked env-sync loses its owed deploys over a restart
Code: `internal/service/jobs.go` `handlers()` keeps `parkedSyncs sync.Map`
(job id to `plan.Owed`). The runner (`flow/jobs/jobs.go` `run`) re-runs a
waiting job's handler each poll; after a restart the map is empty, so the
handler calls `SyncApply` again, re-plans, reads its own writes as drift and
fails "the environments changed since the review".
Decision: persist Owed in the job row. `envSyncJob` gains
`Owed []string json:"owed,omitempty"`. The handler: `len(p.Owed) > 0` means
rollout only (`SyncRollout`), else `SyncApply`; when the result parks with
`plan.Owed`, write the payload back with Owed set (keep the `org_id` key that
`withOrg` added: unmarshal to a map, set `owed`, marshal) into `r.Job.Payload`.
The runner's `run` must park the Run's current row, not the `j` copy from
`start`: build the `Run` in `run`, pass it to `call`, and `Park(fresh, rn.Job,
param)`. Delete `parkedSyncs`. `Sync.Owed` keeps `json:"-"` (it is not API).
Size: 2 files plus 2 test files.

### A2. Sync creates a slice whose source instance is not running
Rig QA: sync adds a slice tile whose `provision_from` instance (another env) is
stopped; the deploy fails "pg is not running" (`flow/managed/flow.go` `tools`);
re-running sync says "nothing to sync" (the row exists, no diff).
Decision: plan-time blocker, shared by promote and sync. In `flow/promote/
plan.go` `sliceTarget`, after `address.Resolve` returns the env slug: when the
target is not a tile this plan writes (`st.ID != w.st.ID`, or the slug is not
in `w.re.Tiles`), look the instance up (`Envs.GetBySlug(st.ID, env)`,
`Tiles.GetBySlug(envID, tg.Tile)`, `Tiles.Replicas`) and block
`slice <x>: <stack>/<env>/<tile> is not running; start it first` when no
replica is `running`. A missing row keeps the existing "unreachable" path.
Deferred: treating undeployed kept tiles as owed on re-plan. The blocker stops
the row from being written in the first place, and B below covers a tile that
exists but never ran; a second rule would mix "nothing to sync" with deploy
state.
Size: 1 file plus 2 test files (promote and sync).

### A3. Three small sync and promote limits
- Drawer after a deploy shows the default source: `canvas/env.go` redirects
  to `tab=sync&job=<id>` without `sync=`, so `syncTab` falls back to
  `src.Default`, and its `jv.Refresh` drops it again. Fix: the redirect and
  `Refresh` carry `sync=<from>`.
- Bar refusal redirect drops `drop=`: `env.go` builds `syncQ{from}` only.
  Fix: the bar form (`internal/ui/graph/graph.templ`, `SyncBar` in
  `ui/graph/view.go`) carries a hidden `drop` field (comma list, from the
  review's `q.drop` in `canvas/handler.go` `mapView`); the handler reads
  `c.FormValue("drop")` into the redirect's `syncQ`.
- Empty promote prints only "promote: done": CLI `move` (`cmd/stackr/
  stack.go`) prints no rows for a plan with no changes and no blockers, then
  asks and posts. Fix: `move` uses `showPlan`; no changes and not blocked
  prints `nothing to change: <env> already runs release <n>` on stderr and
  returns without posting (sync's "nothing to sync" rule, same shape).
Size: web 6 files (plus regenerated templ), CLI 2 files.

### B. Re-promote does not deploy a tile added later
Code: `flow/promote/plan.go` `planImages` diffs the env's current release pins
against the target release pins (`release.Diff(cur, pins)`). On a by-hand stack
whose env already runs release N, re-promoting N is an empty diff, so a tile
added to the env afterwards (by hand or by sync) that N pins is never
deployed.
Decision: in `planImages`, for every desired slug in `pins` with no change
from the diff and no replica container at all (`Tiles.Replicas` empty; run
kinds excluded with `tile.RunToCompletion`), add `Change{Kind: "image", Tile:
slug, Note: "not deployed yet"}` and set `w.redeploy[slug]`. A stopped tile
keeps its containers (`container.Stop` stops, never removes), so it is not
touched. The deploy itself is `Redeploy`, which already runs the pin.
Size: same file as A2 (one worker), plus a promote test.

### C. CLI params
- Did-you-mean: `params get app.mdoe` says `no param app.mdoe at this level`;
  `params rm` of a wrong name posts a DELETE that 404s. Fix: a `closest(want,
  have []string) string` helper in `app.go` (Levenshtein, distance 2 or less;
  cobra's helper is unexported, so about 15 lines); `get` uses the list it
  already fetched; `rm` fetches the level's list first, refuses an unknown
  name with the suggestion, and the confirm names the real param.
- Blank name shows `name: …`: the API answers field errors as
  `name: Give the stack a name.` and `request` maps API fields to flags only
  for limits (`flagNames`); positional names keep the field prefix. Fix:
  `flagNames` also strips `name: ` (and `slug: `), so `stackr stack create ""`
  prints `error: Give the stack a name.`. Test with the fake server.
- `params rm` redeploys silently: `service.DeleteParam` calls `redeployScope`
  and returns nothing; the API answers 204. Fix: `DeleteParam` returns
  `[]Redeploy` (use `o.redeploy`, as `SetParams`), the API handler answers 200
  with the list (regenerate `docs/openapi.json`), the CLI prints the same
  `redeploying <tile> (<env>): stackr job log …` lines `set` prints (one
  shared helper).
Size: server 3 files plus openapi, CLI 3 files.

### D. Rename-link (`moved:`) matching the same slug across orgs
Traced: `flow/orgconfig/diff.go` `moved` matches against the org's own stack
list (`orgLive` uses `stacks.List(ctx, og.ID)`); `store/stacks.go
GetBySlug` filters `org_id`; envs by stack, tiles by env. No cross-org match
exists in the code.
Decision: dropped. Reopen with a repro (org, file, slugs). If the item meant
"a rename leaves directory links on the old slug" (ux-pass follow-up), that is
a different fix and is not in this plan.

### E. ux-pass "Not in this pass"
- More org roles: needs darthvader. DECIDE 171 set "everyone is an owner for
  now"; opening member and viewer is a product call (what a member may do).
- UI typeface: needs darthvader for the pick. Default if unanswered: keep the
  system font stack (`ui/tailwind.config.js` `fontFamily`), no work.
- Undo: dropped. No undo model; confirms cover it.
- Shell completion: cobra's `completion` command is already on (nothing
  disables it in `cmd/stackr/main.go`). Resource-name completion needs the
  API during completion: deferred, speculative.
- `--json` contract cleanup: deferred, no reported break and no spec.
- Help examples: decided, in the CLI worker. `Example:` on the journey verbs
  only: `login`, `stack create`, `env create`, `tile add`, `tile set`,
  `params set`, `promote`, `env sync`, `job log`. One or two lines each.

### F. PROXY protocol
Traced: no code, doc or setting names it. Caddy config is built in
`leaf/domain/caddy.go` (`server()` adds `trusted_proxies` from the
`trusted_proxies` setting).
Decision: deferred. Nothing in front of the rig speaks PROXY and the k8s
backend ingress will not use Caddy's listener. Smallest version if wanted: a
`proxy_protocol_from` setting (`leaf/settings/catalogue.go`), wired in
`internal/service/wiring.go` and emitted as a `listener_wrappers`
`proxy_protocol` entry with `allow` ranges in `caddy.go`. Three files plus a
test. Say "go F" to add it.

### G. DECIDE list review
215 items. Every "builder took the lean" item stands as built; nothing to do.
Items still marked "Lean (b), later" are triaged:
- Do now: 129, GitHub callback trusts only the state nonce. Fix in this plan
  (worker W5): `connector.Begin` records the starting user id in the pending
  row's config JSON; `Complete` takes the session user and refuses a
  mismatch with the existing "does not match a pending connector" text. No
  schema change.
- Needs darthvader (product): 21 and 171 roles (see E); 127 `proxy_custom`
  stored but never read (wire it into the Caddy build, or drop the field);
  133 Roll back vs Promote wording when the target env derived its own
  release; 88 and 104 resolve verbs for the params editor and tile settings
  (a feature, not a fix).
- Deferred, no current harm: 17 (parked jobs re-run blind; A1 makes the
  re-run safe), 107 log levels, 109 instance drawer tabs, 112 domain detach
  tile check (env.write gates it), 118 card footer bits, 101 lane labels.

## Waves

### Wave 1 (five workers in parallel)

W1. Jobs: owed deploys persist (A1)
Files: `flow/jobs/jobs.go`, `flow/jobs/jobs_test.go`,
`internal/service/jobs.go`, `internal/service/envsync_test.go`.
Behaviour: as A1. Tests: `jobs_test.go`: a handler that sets `r.Job.Payload`
then returns `errs.Unset`; the parked row carries the new payload.
`envsync_test.go`: extend `TestEnvSyncResumesAfterPark`: while waiting, the
job row's payload has `owed` with the tile ids; after the secret is set the
job ends done with both containers run. Done: `make test`, `make lint`.

W2. Promote plan: slice source running, re-promote deploys (A2, B)
Files: `flow/promote/plan.go`, `flow/promote/promote_test.go`,
`flow/promote/sync_test.go`.
Behaviour: as A2 and B. Tests: sync with a stopped source instance blocks
with the text above (fake Docker, container state not running); a running one
passes (`TestSyncReachableSlice` stays green); promote of the same release
into an env with a never-run tile that the release pins adds the `image` row
and deploys it; a stopped tile is left alone. Done: `make test`, `make lint`.

W3. Params delete answers what it redeploys (C, server half)
Files: `internal/service/params.go`, `internal/api/handler/v1/data.go`,
`docs/openapi.json` (regenerated), one service test file (new
`internal/service/params_test.go` or an existing one).
Behaviour: `DeleteParam` returns `[]Redeploy`; the route answers 200 with the
list; `redeployScope` goes if nothing else uses it. Test: delete a param read
by a running tile, the answer names the tile, env and job. Done: `make test`,
`make lint`.

W4. Web sync review keeps its state (A3 web)
Files: `internal/web/handler/canvas/env.go`, `envsync.go`, `handler.go`,
`view.go`, `envsync_test.go`, `internal/ui/graph/view.go`,
`internal/ui/graph/graph.templ` (and its regenerated `graph_templ.go`).
Behaviour: as A3 bullets one and two. Tests: after a bar or drawer deploy the
redirect carries `sync=<from>` and the job view's refresh keeps it; a bar
refusal with `drop=api` redirects with `drop=api`. Done: `make test`,
`make lint`, `make templint`.

W5. Connector callback matches the starting user (G, DECIDE 129)
Files: `leaf/connector/connector.go` and its test, `internal/service/org.go`
(`BeginConnector`, `CompleteConnector` signatures), the callers
`internal/web/handler/canvas/create.go` (begin and callback),
`internal/web/handler/setup/handler.go` (begin),
`internal/api/handler/v1/orgs.go` (begin; API keys pass the key's user), and
their tests. `docs/openapi.json` only if the route shape changes (it should
not).
Behaviour: as G. Test: a callback from another signed-in user is refused;
the starter completes. Done: `make test`, `make lint`, `make templint`.

### Wave 2 (one worker, after W3)

W6. CLI (A3 third bullet, C client half, E help examples)
Files: `cmd/stackr/stack.go`, `cmd/stackr/app.go`, `cmd/stackr/scope.go`,
`cmd/stackr/cmds.go`, `cmd/stackr/cli_test.go`.
Behaviour: `move` prints "nothing to change" and posts nothing on an empty
unblocked plan; `params get` and `params rm` suggest the closest name;
`params rm` prints the redeploy lines from W3's answer; `flagNames` strips
`name: ` and `slug: `; `Example:` on the nine journey verbs. Tests (fake
server as `TestPromoteAsks`): empty plan posts nothing and prints the line;
`params get app.mdoe` suggests `app.mode`; `params rm` of an unknown name
refuses with the suggestion and no DELETE; `params rm` prints the redeploy
line; `stack create ""` error has no `name:` prefix; `--help` of `promote`
shows the example. Done: `make test`, `make lint`.

### Wave 3 (integration, ship, QA, fix loop)

Integration (one worker): `make test`, `make lint`, `make templint`; a
regenerated `docs/openapi.json` matches `--dump-openapi`; `git status` shows
only the files the workers own. Fix anything red here before shipping.

Ship: `make release RELEASE=v0.6.0-dev.13`, then
`scripts/rig.sh upgrade 0.6.0-dev.13`. Confirm the panel reports the version.

Rig QA (one worker). Rig https://stackr-test.vulpe.dev, admin@test.com /
Test1234!, org `smoke`. Stacks `shop`, `web`, `infra`, `byhand` are never
edited. QA makes its own by-hand stack `qa-leftovers` with ladder envs `dev`
and `staging` and removes it at the end (tiles, envs, stack). CLI through a
key minted in the org drawer, `stackr login --with-key`.
- A1: in `dev`, image tile `api` (nginx) with env `TOKEN=${{ params.app.token
  }}`, secret set in dev only. Sync `staging` from `dev`, Deploy. The job
  waits on the secret. Restart the panel container on the rig host (`docker
  restart stackr`, over the same ssh `rig.sh` uses). Set the secret in
  staging. The job ends done and `api` runs in staging; the job log shows no
  "changed since the review".
- A2: in `dev`, managed `pg` plus slice `db` on it; stop `pg`. Sync staging
  from dev: the plan shows the `is not running; start it first` blocker and
  Deploy is refused. Start `pg`, sync again, Deploy, the slice lands.
- A3: after the sync deploy the drawer's sync tab still shows source `dev`.
  Open the canvas review with one tile dropped, Deploy from the bar with a
  stale sig (change a port in dev first): the redirect keeps the dropped
  tile. CLI: `stackr promote <n> --stack qa-leftovers --env staging` when
  staging already runs `<n>` prints `nothing to change` and queues no job.
- B: dev has `api` and `worker`, release `<n>` promoted to staging before
  `worker` existed in staging. Add `worker` to staging by hand without
  deploying. Promote `<n>` again: the plan lists `image worker not deployed
  yet` and `worker` runs after the job.
- C: `stackr params get app.mdoe --env dev` suggests `app.mode`;
  `stackr params rm app.mode --env dev -y` prints a `redeploying` line for
  the running tile; `stackr stack create ""` prints `error: Give the stack a
  name.`.
- G 129: unit test only (needs GitHub).
Record each check as pass or fail with the exact output.

Fix loop: each QA failure goes back to its wave worker's files (same
ownership), re-run `make test`, `make lint`, `make templint`, bump to
v0.6.0-dev.14 (and onward), `make release`, `scripts/rig.sh upgrade`, re-run
the failed checks only. Stop when every check passes. Then update this file's
Status line with the shipped version and QA result.

## Not in this plan
Roles, typeface, `proxy_custom`, DECIDE 133, resolve verbs: darthvader's
call. Undo, shell resource completion, `--json` cleanup, PROXY protocol,
rename-link across orgs: dropped or deferred as stated above.

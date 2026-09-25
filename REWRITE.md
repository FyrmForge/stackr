# stackr — the rewrite

Agreed 2026-09-22. stackr is rebuilt from scratch, one layer at a time, with
business logic in services **only**. The service-extraction work on the old
code stops here.

## Why

The old code is ~87k lines of hand-written Go plus 13k lines of templ, about 45
features, and was built in ~2 months. The extraction sweep
(`docs/plans/service-extraction/`) showed the rules live everywhere except the
services: in handlers, in `main`, in infra packages, in SQL. Thirteen call
sites start a deploy without going through `DeployService`. Five copies of
"does this tile hold data" disagree, and that disagreement has already caused
two data-loss incidents.

This plan also drops Swarm, swaps Traefik for Caddy, cuts auth to two roles
and cuts v1 to about a third of today's features. Each of those alone is a big
change to the old code. Put together they add up to most of a rewrite, so we
do the rewrite properly instead.

Nothing is deployed anywhere, so there is no data to migrate.

## Method

1. New branch. Delete everything. Run a fresh `hamr new`.
2. Keep a git worktree of the last old-code commit open next to it.
3. Copy code in from the worktree only when it earns its place (see
   "What comes over").
4. **No "strip the old repo down".** Trimming the old code keeps the tangled
   parts we don't want.

### How the builder works (agreed 2026-09-24)

The builder never opens the old worktree. Its context holds this plan, the
step's task list and the extracts, nothing else.

- **Extracts.** Every row of "What comes over" becomes one small file in
  `docs/rewrite/extracts/`, made by a separate narrow sub-agent that reads
  only that row's files. Five-line header: source path, commit, what was
  taken, what was cut, where the cut parts belong. Three filters, in order:
  the row (only the listed files, the take and leave columns); the layering
  rules (no store call, no Docker call outside the wrapper, no auth check, no
  status decision; each cut marked `// extract: dropped X, belongs in
  leaf/Y`); size (an extract bigger than its source was copied, not filtered,
  and goes back). A human skims every extract before the builder sees it.
- **Sessions.** One fresh builder session per build step, one stacked PR per
  step. Planning, the extracts and the wipe happen in the planning session
  and its sub-agents; building never does.
- **The wipe.** A sub-agent deletes everything on the `rewrite` branch except
  `.claude/`, `.mcp.json`, `CLAUDE.md` and `.gitignore`, runs `hamr new`
  (in a temp dir and moves it in if `hamr new` refuses a non-empty dir),
  copies this plan, the task lists and the extracts into `docs/rewrite/`.
  Checked before push: `make build`, `make lint`, `make test` pass; `git
  diff master --stat` shows only the old code gone and the scaffold added.
- **The worktree path** stays out of the new `AGENTS.md`. Anything the
  builder needs and cannot find in the plan or an extract is a plan gap,
  raised, not guessed.
- **Progress file.** `docs/rewrite/PROGRESS.md` is the only source of "where
  are we". Every session, planning or building, fresh or after a compact,
  reads it first and never asks. Whoever works ticks each item as it
  finishes (`[x] step 0 / task 3: AGENTS.md examples fixed`) and commits the
  file with the code. Anything that needs darhvader goes under a `DECIDE:`
  block at the bottom, with the options, instead of being guessed; work
  continues on everything that does not depend on it.

## Rules from the first commit

These go into `AGENTS.md` on day one. Most of the old repo's drift came from
them never being a repo standard.

1. **Services are the only home for business logic.** Handlers, the API, the
   CLI, `main`, the store and the Docker package decide nothing about the
   domain.
2. **The compiler enforces the layering.** The store, the infra wrappers,
   the leaves and the flows all live under `service/internal/`, so Go
   refuses to build any import of them from outside `service/`. Handlers see
   one thing: the orchestrator. No guard tests to maintain.
3. **Dependency injection:** `main` calls `service.New(config)`, and the
   service builds its own store and Docker client inside. Tests pass a fake
   Docker in through an exported interface. The real one stays internal.
   Authorisation is not a service concern: it runs in middleware (see
   "Auth"). Services take a user only for audit fields.
4. **The store is CRUD plus type mapping.** The only rules it holds are schema
   constraints: foreign keys, unique, not null. It sets no defaults of its
   own, decides no ordering policy and makes no status decisions. Example:
   the environment ladder (dev below staging below prod) was an `ORDER BY`
   in the old store. Now environments carry an explicit `position` column
   and `leaf/environment` owns "is X above Y"; promote asks it.
5. **The Docker package receives fully resolved specs.** Variables are already
   filled in, so it never needs a domain rule, and no rule gets stuck below
   the services.
6. **Handlers are dumb.** They bind input, check form shape, call one service
   method and render the result. When a screen needs a decision, the service
   returns it (for example a `CanDeploy` field). Enforced two ways: a written
   repo rule, plus a skill that audits handlers. The skill is a spot check;
   the compiler rule is the hard wall.
7. **Errors are typed.** Never branch on an error's wording (old bugs B32 and
   B33 did).
8. **Anything built once is constructed once.** No double wiring in `main`
   (old bug B0).
9. **Anything not written down behaves as it does today, for domain rules
   only.** Who may deploy, what promote copies, variable precedence, slug
   rules: the old code is the spec, and this plan lists only what changes.
   Old `.templ` files count as spec as much as `.go`: when a screen shows a
   decision, find where it is computed and move that to a service.
   **Mechanics that Swarm used to do have no "today".** Rollout, replicas,
   service DNS, health gating, load balancing: every one of those is
   rebuilt and must be written in this plan (see "Infrastructure"). Not
   written down means not built, never "guess what Swarm did".
10. **Everything that touches a tile's container goes through the job
    queue** (see "Job queue"): deploy, promote, rollback, restart, stop,
    backup, image-watch redeploy. Nothing starts any of them another way.

## Service tree and repo layout

Services form a strict tree. The tree is mirrored in the repo and enforced
by the compiler where it can be and by `depguard` in `make lint` where it
cannot. Handler → orchestrator → flow or leaf → store / infra wrapper.
Nothing skips a level.

```
service/
  orchestrator.go       New(config) and the Orchestrator: the only thing
                        main, the API and the handlers see. One method per
                        user-facing verb: CreateTile, StartTile, Deploy,
                        Promote, CreateVolume, RenameOrg, ...
  tile.go, org.go, ...  the small verbs, one file per area; most are one
                        line calling a leaf
  internal/store/       CRUD, one file and one small interface per table
  internal/docker/      the Docker wrapper, no rules
  internal/proxy/       the Caddy admin client
  internal/git/         clone, checkout
  internal/s3/          backup destinations
  internal/leaf/<name>/ one row kind and the one thing in the world it
                        stands for
  internal/flow/<name>/ the big verbs that sequence several leaves
```

- **A leaf is the real thing.** It owns one row kind *and* the world
  object that row represents: `volume` = the row + the Docker volume,
  `environment` = the row + its network, `domain` = the row + its Caddy
  route + the tile's ingress network, `tile` = the row + its containers
  (replicas and the pause container; start, stop, remove, logs, the VIP
  rules, `State()` read from Docker first, the row second, `Exec()` for
  engine commands), `image` = built images on the box, `volume` = its row
  + the Docker volume. Leaves: org, stack, user, environment, tile, image,
  params, settings, volume, domain, credential, connector, managed,
  release, job, backup (destination, schedule, run). Every table has
  exactly one leaf; a table nobody owns is a plan bug.
- **A leaf reads and writes only its own table.** Its constructor gets its
  table interface and the slice of the Docker wrapper it needs, nothing
  else. It never calls another leaf and never a flow. Facts from other
  tables come in as arguments: `environment.Delete(env, tileCount)`,
  `params.Resolve(org, stack, env, tile)`. The leaf still owns the
  decision; the caller only fetches.
- **A leaf runs a spec, never builds one.** `tile.Start(spec)` takes a fully
  resolved spec. Spec building and the swap order live in `flow/deploy`.
  The old `status` column on tiles goes away; state is derived from the
  container and the last job row, so it cannot drift.
- **A flow sequences several leaves.** It computes and passes down:

  ```
  row  := tile.Create(cfg)
  vars := params.Resolve(org, stack, env, row)
  net  := environment.Network(envRow)
  vol  := volume.Ensure(volRow)
  spec := build(row, vars, net, vol)   // plain struct, in the flow
  tile.Start(spec)
  ```

  Flows: jobs, deploy, promote, backup, managed, container (restart, logs).
  Flows hold no Docker handle; the leaves do. Flow → flow only on listed
  edges, never a cycle: **promote → deploy**, **deploy → managed**. Every
  flow that starts work does it through `flow/jobs`.
- **Leaf or flow?** One question: does it stand for one row kind and its
  world object? Leaf. Does it sequence several? Flow.
- **Orchestrator.** The single door. A passthrough verb is one line. A rich
  verb calls a flow. Handlers never reach a leaf or a flow; the compiler
  stops them (both are under `internal/`).
- **Enforced how.** Everything under `service/internal/` cannot be imported
  from outside `service/` (compiler). Leaf → flow is an import cycle
  (compiler). Leaf → leaf and flow → flow are a `depguard` rule (lint).
  Every cross-service call is listed in the plan; nothing gets added that is
  not listed.
- **Store** is the floor the tree stands on, not a branch of it: one package,
  one `Tx`, so a flow can write three tables in one transaction. One file
  per table; a table file only writes SQL against its table. Joins are the
  exception, used as little as possible: a join lives in the query file of
  the flow that owns the question, with a one-line reason, never in a table
  file. Default is per-table queries composed in Go by the flow.
- **Tests** use `service.New(cfg, service.WithDocker(fake))`, where the fake
  satisfies one exported `service.Docker` interface. Leaf rules test as
  near-pure functions since their inputs are arguments.

## Job queue

One durable job table, `flow/jobs`. Deploys, promotes, rollbacks, restarts,
stops, backups and image-watch redeploys are all jobs. Cheap ones finish in
a second; they still queue so the per-tile lock sees them.

- **N workers**, default 2, one admin setting. That is the only cap on
  parallelism on the box, builds included.
- **A job never enqueues and waits for another job.** Promote is one job
  that calls the deploy flow's function in-process (the listed
  promote → deploy edge). Two promotes on two workers waiting for two
  deploy jobs would wait forever. One job, one lock, one row.
- **Lock set per job.** A job carries the set of tiles it touches: a
  single-tile job one tile, a promote every tile in the env. A worker picks
  the oldest runnable job none of whose tiles is busy. Two jobs never touch
  the same tile at once; disjoint sets run in parallel. The set is computed
  by one function so a priority lane later is a one-function change.
- **Railway rule** for a newer job on the same tile: a waiting job is
  replaced (state `superseded`, never `failed`); a running job still in its
  build phase is cancelled and the newer one starts; a running job already
  swapping containers finishes first, then the newer one runs. Never abort
  mid-swap.
- **Parking stays.** A deploy that hits an unset param goes to state
  `waiting` with the param name on the row. The queue polls waiting jobs
  every few seconds and re-checks whether the param now resolves. No
  poke from `leaf/params`: a leaf never calls a flow. The Railway rule
  applies to waiting jobs too.
- **Every job is a row to poll** (B25). A job runs under its own context
  with a 30-minute cap, detached from the HTTP request that made it. The
  supersede rule is one function next to the lock key.

## Volumes

- A volume is **its own thing, not a tile**: own `volumes` table, own
  `leaf/volume`, own `volumes:` block in the stack file. Scoped to its
  environment (as today); promote never carries a volume across
  environments; prod gets its own. A managed instance owns its volume(s)
  and that volume is env-scoped like the instance tile (DECIDE 194). A
  service tile *mounts* a volume. The UI still draws a volume as a card:
  stacked under the tile that mounts it, on its own when unmounted.
- `leaf/volume` has the only `HoldsData` in the repo. The old code had
  five and lost data twice.
- A deploy, rollback, promote or tile delete never removes a volume or a
  managed instance. A volume is **orphaned** when its entry leaves the
  stack file, or when the managed instance that owns it is removed; a
  mounting tile going away only unmounts. Orphaned: the row stays, flagged
  with a timestamp, the data stays. Re-adding the same slug re-adopts the
  orphan. One setting,
  orphan retention in days (default 30); a daily job backs an expired
  orphan up to the default backup destination, then deletes it. Explicit
  delete in the UI or CLI at any time. There is no `force` flag anywhere:
  nothing a promote does is destructive.
- **Every install has a local backup destination** (a directory under
  stackr's data dir), the default for every backup. S3 destinations are
  extras. A destination is one interface, two implementations.

## Promote and releases

**Round three (2026-09-22/23).** Everything in this section is agreed.

- **A release is a snapshot, not a commit.** Table `releases`: id, stack,
  number (per stack, `#41`), created_at, created_by. Table
  `release_tiles`: release, tile slug, repo, branch, commit, image id
  (empty for image and managed tiles). The config repo is one of the
  repos: its row pins the commit the config came from. Mapped by slug
  because dev and prod tiles are different rows. `environments.release_id`
  = what the env runs; `jobs.release_id` = what that deploy job shipped.
  There is no `deployments` table: a deploy is a job, and "what does this
  tile run" is its last finished deploy job. "What commit of shop-api is
  in prod" is one lookup.
- **The release row is the basis regardless of deployment model.** Every
  env has two knobs, in the file's env section and overridable in the UI:
  `from` (a branch, or `promote` = releases arrive only from the env below
  in the list) and `auto` (on: a push builds and lands here, or the env
  below's new release is promoted here, with no click; off: it waits for
  a button or the API). Branch envs default to auto on. Two spellings
  compile to those knobs: `ladder: [dev, staging, prod]` + `head: main`
  (head goes to the first rung, the rest are `from: promote`), or
  `branch:` per env. Examples: solo prod = `prod: from main, auto`;
  classic ladder = dev auto, staging auto, prod manual; env per branch =
  every env tracks its own branch, nobody promotes. This replaces the old
  per-env `apply_policy`. PR envs are an env `from <pr branch>` with the
  old `base_env` column, made and removed by the connector.
- **One release per push.** A push of five commits builds the head once.
  The config comes from that same commit, so in the branch model each env
  runs its own branch's file.
- **The file makes the envs.** A push to the config branch first creates
  every env the file's `ladder:` (or `environments:`) names that the
  stack lacks, bottom rung first, with the file's `from`/`branch`,
  `auto` and `color`; then the release is derived as below. The file
  never deletes an env (DECIDE 189). So a freshly bound stack runs from
  its first push with no clicks.
- **The release carries the config.** `stackr-compose.yml` lives in the
  stack's config repo and declares every tile, including tiles whose code
  is in another repo (`git_url` on the tile). Tile repos carry code only.
  Variable and secret **values** stay per env, out of the file; only
  declarations move.
- **One verb: promote.** "Promote #41 to prod" applies the config's prod
  overlay from that release, then deploys its images, one tile at a time,
  in one job. Config changes ride the ladder like code. No separate apply,
  no plan rows.
- **Build on push** lands in every env whose `from` is that branch: build
  every tile in that repo whose `watch_paths` changed (default: the build
  context), apply that env's overlay, write the release. A push to a tile's own repo rebuilds that
  tile only and copies everything else from the previous release. Build or
  parse failure = no release. "Build this commit" is the same job from a
  button or the API.
- **Image reuse.** A tile whose paths did not change keeps the previous
  release's image id. So promoting or rolling back an env restarts only
  the tiles whose image or config actually differ.
- **Rebuild only from an explicit action** (push, button, API). A variable
  change, volume attach, settings save or restart redeploys the image on
  disk from the env's current release. No "restart from branch head" path
  exists. B34 and old Q5 closed: everywhere.
- **Dry run.** Promote takes `dryRun` and returns the diff (tiles created,
  changed, orphaned, images moved). The CLI and UI show it, then call for
  real.
- **Rollback is a promote.** Env rollback = promote the previous release.
  Per-tile rollback = new release copied from the current one with that
  tile's image swapped, then promote. Every change to what runs is a new
  release; there is no other path.
- **Multi-repo** changes nothing in promote: it only compares two snapshots.
- **Terraform model: release = desired state, DB rows = live state.** A
  UI or CLI edit (a setting, a domain, a limit) changes live state
  directly: redeploy on the current image, no release, no commit, no
  patch. It is drift. Promote = plan then apply: diff the release against
  live state, show it (the dry run), reconcile. Hand edits show in that
  diff as being put back; prod runs its hand edits until then. **Drift
  never promotes:** a release only ever comes from git or a build. No
  staged UI edits, no patches on top of the file, no `ui_edits` key.
  Drift painting in the UI is later UX; the model already knows the delta.
- **Stacks with no config repo** are live state only. Their releases are
  image pins (image tiles, image watch bumps).
- **A release records the digest**, never just the tag. The file says
  `postgres:16`; the release says `postgres@sha256:…`. Promote moves exact
  bits, so `nginx:latest` in dev is the same `nginx:latest` in prod a week
  later.

### Image watch

Two modes, both v1. `update_policy: manual | auto` per tile; no "notify"
policy, the chip shows either way.

- **Mode 1, digest.** The tag stays, the bits behind it move (`latest`,
  `16`, `stable`). Ask the registry for the digest behind the tag; changed
  = update.
- **Mode 2, tag policy.** New tags appear next to the old (`myapp:1.2.4`).
  `tag_policy: semver ^1.2` on the tile: list the repo's tags, sort by
  semver, pick the newest that matches; new pick = update.
- **Fully pinned, no policy:** nothing happens, by choice. Bump by editing
  the file and committing.

The process, one system job:

1. Scan list: every image tile in every env, deduped by
   `registry/repo:tag`, with the tiles that use it. One registry call per
   unique image, not per tile (Diun's rate-limit lesson).
2. Timer: every N minutes (admin setting, default 60), plus "check now"
   per tile and per stack.
3. Ask the registry with the tile's pull credential: HEAD the manifest
   for mode 1 (resolved to this box's arch), list tags for mode 2.
4. Compare with each env's current release. Cache the answer on the image
   row; the UI never calls a registry.
5. Nothing changed: done. Registry errors go on the image row and retry
   next round, never surface as an update.
6. Changed: for each env whose tiles use it and that takes releases
   directly (a branch env or the bottom rung), one release row = the env's
   current release with that tile's digest replaced.
7. Chip on the tile: "update available". No notification system in v1;
   the chip is the signal.
8. `auto`: the release lands now through the normal promote job (pull by
   digest, health gate, rollout). `manual`: it waits for the chip's button.
9. The release is promotable up the ladder like any other.
10. Rollback = promote the previous release, which pins the previous
    digest. Old images stay until image cleanup.

Not doing: writing tags back to git (Flux style), per-tile intervals,
registry webhooks (later optimisation, same steps from 6). Changelog links
on the chip are UI work.
- **The stack file keeps:** `include`, `shared` tiles, stack-level domain
  reservations, the `defaults` cascade (server → org → stack → env →
  tile), `pr_envs`, `params` (collections; a secret is declared by name
  and type only, never its value), per-env overlays, and every tile
  key. Tile `proxy:` is a block of **named** extras, safe by construction:
  `basic_auth`, `websockets`, `max_body`, `timeouts`, `headers`, `methods`
  (WebDAV/CalDAV verbs), `strip_prefix`. Raw Caddy config stays
  admin-only, outside the file. **Cut:** `moved:` (orphaning covers the
  data-loss case; re-adding a slug re-adopts), `ui_edits`, `apply_policy`
  (replaced by `from`/`auto`), `traefik_override`.

## Org config file

**Step 7 (planned 2026-09-25).** darthvader put org config files back in v1
(DECIDE 180). It is the stack pattern one level up, in v0's shape: the org
is bound to a repo, every plan is a row an owner approves or rejects, and a
binding can be set to apply on its own (DECIDE 181 and 186, darthvader's
calls).

- **Binding.** Four columns on `orgs`, the same four `stacks` has:
  `config_connector_id`, `config_repo`, `config_branch`, `config_path`
  (default `stackr-org.yml`), plus `config_auto` (apply every unblocked
  plan without a click; off by default). An owner binds from the org
  drawer, the API or the CLI (v0 was panel-only). The repo is typed as an
  https URL or `owner/name` and stored as the URL, for org and stack
  alike: today the stack drawer asks for `owner/name` and the connector
  lookup needs the URL, so a bind from the drawer clones nothing. A stored
  connector id wins over the host lookup.
- **The file.**

  ```yaml
  version: 1
  org: Acme                   # the name; a slug change is a rename
  params:                     # org scope, the stack file's grammar
    email:
      api_key:
        type: secret          # declared; the value is never in the file
      sender:
        type: param
        value: noreply@acme.test
  defaults:                   # the org rung of the settings cascade
    cpu_limit: 1
  env_colors:
    prod: red
  stacks:
    shop:                     # the slug; the name is the key
      repo: https://github.com/acme/shop
      branch: main            # default: the repo's default branch
      path: stackr-compose.yml
      connector: c0fab773     # default: the org's connector for that host
    blog:
      path: stacks/blog.yml   # a stack file inside the org repo
  domains:                    # org domain resources (DECIDE 191)
    - host: acme.example.com
      include_env_on_default: false
      acme_email: ops@acme.test   # optional
  moved:                      # renames, read first; drop once applied
    - from: stack.weblog
      to: stack.blog
  ```

  Two sources and nothing else: remote (`repo`, the stack's own git repo)
  and local (`path`, a file in the org repo). A stack entry only says
  where its file is (DECIDE 182).
  Strict decode, `version: 1`, `org:` required, the stack file's hints for
  removed keys. Not in the file: inline stacks (v0 applied an existing one
  with `force=true` and never showed its changes in the org plan; the
  rewrite has no apply-without-release path) and storage shares (not a
  v1 object). DECIDE 182. Shared managed instances are declared in a
  stack's own file, never here (DECIDE 193).
- **The diff.** `flow/orgconfig` parses the file and diffs it against a
  `Live` snapshot the orchestrator hands in: the org, its stacks with
  their bindings, the org's params, settings and
  env colors. Pure: no store, no clone. `moved:` runs first (DECIDE 184):
  `from` exists and `to` does not → rename (`stack.<slug>`, the only
  kind), and the rest of the diff sees the stack under its new slug; `to` exists and `from` does not → already moved, no change,
  the entry may stay; both or neither exist → blocker. Changes: `org:`
  slug differs → rename; a param declared but missing → create (a param's value is set,
  a secret's never; "declassify: never" holds; a secret with no value is
  a note, not a blocker); params are never deleted by the file (v0's
  rule, DECIDE 185); `defaults` and `env_colors` set when the key is
  present; a stack missing → create + bind; a stack whose binding differs
  → rebind; a stack gone from the file is left as it is: the file never
  deletes a stack, hand-made or file-made (DECIDE 188; delete it in the
  UI or CLI). Blockers: the rename squats a domain or collides, a stack's
  host has no connector.
  Domains (DECIDE 191): an entry missing → create an org domain
  resource; env flag or ACME differs → update; gone from the file → left
  alone; the host taken elsewhere or squatting another org → blocker.
- **A plan is a row.** Table `org_config_plans`: id, org (FK, cascade),
  commit, summary, plan (the diff as JSON), status, error, created_at,
  decided_at. Statuses `pending`, `clean` (no changes), `error` (the file
  did not parse, or the apply failed; the message in `error`),
  `superseded`, `applied`, `rejected`. Storing a plan supersedes the org's
  older pending and clean rows. Two v0 flaws fixed: a file that does not
  parse is stored as `error`, never `pending`; and the table has a real
  foreign key, so deleting the org deletes its plans. `leaf/orgplan` owns
  the table.
- **Planning** = clone the org repo (`repos/orgconfig-<org>`, under the
  repo lock), read the file at head, diff, store the row with that
  commit. Three triggers, one function: the push webhook (a push to the
  org's repo on its branch queues a plan job, lock `orgconfig:<org>`, so
  the clone never runs inside GitHub's request), Plan now in the drawer
  or `stackr org plan` (synchronous, as `PlanPromote` is), and a rebind.
  Preview diffs a supplied file and stores nothing: `stackr org preview
  -f` for CI, `--detailed-exitcode` as before.
- **Approve = one job**, lock `orgconfig:<org>`, owner-only
  (`orgplan.approve`), refused while the plan has blockers (B20) or is not
  pending. The job refetches the file at the plan's commit (v0 re-read the
  branch tip, so an approval could ship pushes nobody reviewed), re-diffs
  against live state, then walks the changes in order through the
  orchestrator's own verbs: `RenameStack` for `moved:`,
  then `RenameOrg`, `SetParams`, `SetOrgSettings`,
  `SetOrgEnvColors`, `CreateStack` + `SetConfigRepo`, and
  `CreateDomainResource` / `UpdateDomainResource` for `domains:`. Each
  bound stack's own file then rides push → release → promote; a stack the
  apply creates or rebinds gets the webhook's push job at its branch head
  right away, so one push creates and binds every stack the org file
  declares; each stack's own push then makes its envs from its file's
  `ladder:` (DECIDE 189) and lands the first release. There is no
  second approval, the rewrite has none for stacks. Success marks the
  plan `applied`; a failure marks it `error` with the message and what
  applied stays (the next plan shows what is left). One apply per plan:
  a second approve of the same plan is refused. Reject marks it
  `rejected`.
- **Auto.** With `config_auto` on, the plan job approves its own plan
  when the plan is pending and unblocked, so a push lands without a
  click. The file never deletes a stack, so auto never tears one down; it
  does rename the org and create or rebind
  stacks. Off, the plan waits for an owner.
- **Export.** `stackr-org.yml` from live state: the name, params (values
  for params, declarations for secrets), defaults, env colors, and the
  config-managed stacks as repo references and the org's domain
  resources.
  Read level, as v0.
- **Verbs.** `SetOrgConfigRepo` (owner, `org.config.bind`; empty repo
  unbinds and rejects the pending plan), `PlanOrgConfig` and
  `PreviewOrgConfig` and `OrgPlans` and `OrgPlan` (owner,
  `org.config.bind` level, as v0), `ApproveOrgPlan` → `Job` and
  `RejectOrgPlan` (owner, `orgplan.approve`), `ExportOrgConfig` (read,
  `org.config.export`), `SetOrgSettings` and `SetOrgEnvColors` (owner,
  `orgdefaults.set`; closes DECIDE 166). All six authz verbs are already
  registered.
- **UI.** The org drawer gets a Config tab: v0's Config as code section
  (Export link; connector select, repository, branch, path, an Auto
  apply switch; Save and plan / Plan now), the latest plan under it drawn
  as the promote dry run is, with Approve and Reject for owners, the
  queued job while it runs, and
  the last plans as rows (v0's plans page, inside the tab). The org
  canvas shows v0's banner while a plan is pending ("Config plan
  pending") or errored ("Config invalid"). The connector card gets a
  config edge to the org, as it has to a stack. The new-org wizard is
  back (DECIDE 187): v0's pages, a branch question (by hand or from a
  config file), then connector → config (bind; the plan on the same
  step, approve or reject; the file names the org) → team → done, or
  name → connector → domain → team → done by hand, one to one with v0
  (the domain step needs the org domain, DECIDE 190). An unfinished org
  sends its owner to the summary
  and shows anyone else a holding page. The drawer's Config tab is for a
  rebind after setup.

## Param store and refs

**Round three (2026-09-23).** Everything in this section is agreed. Replaces
the old `variables` table and the four-owner cascade.

- **The name is param store.** Not "vars", not "env". Two kinds of entry,
  `param` and `secret`, in one store. Kind is fixed at creation.
- **Collections.** Entries live in named collections: `email.api_key`,
  `email.sender`. A collection is one scope's grouping; there is no
  cross-scope collection. Stack file shape:

  ```yaml
  params:
    email:
      api_key:
        type: secret          # value never in the file
      sender:
        type: param
        value: noreply@test.com
  ```

- **Scopes: org, stack, env.** No tile scope. A lower scope overrides the
  same collection.name from above, so `email.api_key` set at stack and
  again at env `prod` gives prod its own value. Tiles ref the name once
  and get the right value per env.
- **Tile env is not the store.** A tile's `env:` block is a list of keys
  whose values are literals or refs, owned by the tile row and the stack
  file. `flow/deploy` resolves every ref and hands the Docker layer fully
  resolved values. The old tile-owned variable rows and the mirrored env
  blob do not come over.
- **Values are literal.** A param value never contains a ref. No nesting,
  no recursion, no cycle detection.
- **Ref grammar** `${{ ... }}`, one resolver, used by deploy, previews and
  CLI export:
  - `${{ params.<collection>.<name> }}`: resolves env, then stack, nearest
    wins. **Stops at stack.** Org is a trust boundary and is never reached
    by accident.
  - `${{ org.params.<collection>.<name> }}`: reads org, deliberately. No
    override from stack or env.
  - `${{ self.<output> }}`: the consumer tile's own outputs. Replaces
    writing your own slug, which broke on rename.
  - `${{ tile.<slug>.<output> }}`: a sibling tile in the consumer's env.
    Same ref gives the dev api in dev, the prod api in prod. On a slice
    tile it resolves to the consumer's own binding (see "Managed tiles").
  - `${{ env.name }}`: the consumer's env name. Allowed only in a slice
    tile's `provision_from`, where it goes through the managed tile's
    `env_pairs`. The `${{ stack.<slug>.<output> }}` and
    `${{ org.<slug>.<output> }}` tile refs are gone with scope: a shared
    instance is reached through a slice tile. `params` is the only
    reserved slug. (DECIDE 194, 2026-09-25)
  - `${{ stackr.<NAME> }}`, `${{ org.backups.<name> }}`: as today.
- **Tile outputs are the built-ins only.** Nothing reads another tile's
  env. Service tile: `host`, `port`, `url` (service DNS on the env
  network), `public_domain`, `public_url` (first non-redirect domain;
  unset refuses the deploy). A slice publishes its engine's set (postgres:
  `DATABASE_URL`, `PGHOST`, `PGPORT`, `PGDATABASE`, `PGUSER`,
  `PGPASSWORD`; s3 as today), minted for each consumer's own cred.
  Referencing a slice is what gets the consumer its binding.
- **Where refs are allowed:** env values, command override, domains, a
  volume's backup `dest` (`org.backups.` refs only), and a slice tile's
  `provision_from` (`env.name` and `params.` refs only). Domains take `params.`
  refs only (`api.${{ params.domains.base }}`), never tile outputs (cycle)
  and never secrets. Nowhere else. Org storage shares
  get their own tile key instead of a ref in a volume line.
- **Unresolved refs.** An unset param parks the tile as `waiting` (see
  "Job queue"); setting the param triggers the redeploy. Every other
  failure (typo, unknown tile, no binding, secret in a domain) fails the
  deploy with the message. Nothing unresolved ever reaches a container.
- **Declassify: never.** Writing `type: param` over a secret is refused.
  Delete it and create a param. Param → secret is allowed, one way. Config
  apply declares secrets but never writes their values.
- **Name grammar.** Env keys on a tile are real env var names
  (`[A-Za-z_][A-Za-z0-9_]*`), same rule in file, API and UI. Collection and
  param names are slugs (`[a-z0-9_]+`), same validator as tile slugs. Dots
  are separators, never part of a name.
- **As today, no decision needed:** generating a random secret value on
  demand; secret values refused to readers without the secrets permission
  (never loaded, not redacted); encryption at rest.

Later: linking a stack collection to an org collection (symlink style, so
tiles keep `params.` refs); `env.` refs beyond `env.name` if anyone needs
them.

## Managed tiles

In v1: Postgres and S3 instances, slice tiles, and a binding per consumer.

- **The instance container is a plain tile** (`kind = managed`). It
  deploys, restarts, mounts its volume, gets health, logs and a network
  through `flow/deploy` like any tile. There is one deploy path. The old
  `infra/managedtiles` had its own `Deploy`/`Start`/`Stop`/`Remove`; that
  duplication does not come over.
- **Allow list, not scope** (DECIDE 194, 2026-09-25). A managed tile is
  env-scoped like any tile and says who may connect with `allow:`, a list
  of `org:stack:env:tile` patterns (a trailing `*` swallows the rest, a `*`
  in the middle matches one segment). The first segment is the tile's own
  org: no cross-org sharing. No list means the tile's own env only.
  `env_pairs:` maps a consumer's env name to one of this stack's envs; a
  consumer env not in the map blocks its promote. A PR env with no pair of
  its own maps as its base env, the way the promote plan falls back to the
  base env's file section; its address for `allow:` stays its own slug
  (`smoke:shop:pr-12:api`). The per-instance network from "Infrastructure"
  is what makes an instance reachable from another stack.
- **A slice is a tile** (`kind: slice`) in the consumer's stack file: one
  database or bucket many tiles use. `provision_from: <stack>:<env>:<tile>`
  names the managed tile in the same org (`${{ env.name }}` in the env
  segment goes through `env_pairs`); `default_access: read|write`
  (default write). The instance side declares nothing per slice. Every
  consumer that refs `${{ tile.<slice>.<output> }}` gets its own binding
  (own user, own outputs) at the slice's default access, or at what its
  `slice_access:` entry (`from`, `access`) names. The per-consumer `slices:` list,
  `scope` and `SetInstanceScope` are gone.
- **One interface per engine, `ManagedTile`:** `Definition()` (image,
  command and config, port, volumes (one or more), deps, defaults, what the
  instance needs), `Ready()`, `Provision()`, `Drop()`, `Bindings()`
  (a consumer's credentials at read or write), `Backup(method)`,
  `Restore(method, target)`; an engine may offer several methods (Postgres:
  `dump` in v1, `pitr` later; see "Backups"). Engines produce commands;
  `flow/managed` hands them to `leaf/tile.Exec()`. The flow never holds a
  Docker handle. Scaling is not on the interface yet; a `ponytail:`
  comment marks the slot.
- **Registry:** one `map[name]ManagedTile`. Adding an engine is one file
  and one line. Plugins later write into the same map.
- **Placement:** `flow/managed` (`engine.go` with the interface and the
  map, `postgres.go`, `s3.go`) holds the rules once, engine-blind: one
  provision per slice tile, one binding per consumer, what happens to the
  data when the slice tile goes (`on_remove`). Who may provision is
  `can()` in middleware like every other verb. `leaf/managed` owns
  `managed_instances` (engine, admin creds, endpoint, allow list, env
  pairs) plus the provisions and bindings tables. `leaf/tile` never sees the
  word Postgres; it knows the kind exists for the per-kind whitelist (B26).
- **Wiring:** instance deploy: `flow/deploy` sees `kind = managed`, asks
  `flow/managed` for `Definition()`, builds a normal spec, runs it, then
  waits on `Ready()`. Slice tile deploy: no container; `flow/managed`
  provisions the database or bucket once with the owner cred. Consumer
  deploy: `flow/deploy` asks `flow/managed` for the consumer's binding; it
  mints the user at its access, writes the binding row and its outputs,
  returns; the consumer joins the instance's network and deploy resolves
  its `${{ tile.<slice>.<output> }}` refs as normal (see "Param store and
  refs"). `flow/managed` never starts a container.

### Scaffold fixes (step 0)

The hamr scaffold's `AGENTS.md` contradicts itself. It says "keep handlers
thin", but both of its code examples give the handler the store and save to
the database inside it. Agents copy the examples, not the rule. Fix it in this
repo only:

- Rewrite both examples so the handler calls a service.
- Validation: handlers check form shape only. Services own domain rules.
- No single giant store interface (the old one had 236 methods). Split it by
  table.
- Migrations stay editable until the first real install. The "frozen
  baseline, additive only" rule applies only after that.

## Backups

**Round three (2026-09-23).** Everything in this section is agreed.
Filesystem snapshots were explored and parked (see "Later").

- **The unit of backup is the volume, not the tile.** A schedule attaches to
  a volume tile. A service tile with two volumes has two schedules or none.
  A managed instance owns its volume, so its backup is a backup of that
  volume with a smarter method. The orphan job (see "Volumes") uses the
  same path.
- **The engine picks the method.** Plain volume: `pause` (default) or
  `stop` the mounting tile, tar through a helper container, as today;
  `live` accepts a torn copy. Managed Postgres: `pg_dump` while serving, no
  downtime. Managed S3: pause or stop + tar of its volume, like a plain
  one. The schedule row carries a `method` column from day one; it is not
  derived from the tile kind, so an engine can offer a second method later
  without a migration.
- **Declared on the volume** in the stack file, same fields as today's tile
  block, kind gone:

  ```yaml
  volumes:
    pgdata:
      backup:
        dest: ${{ org.backups.hetzner }}
        schedule: "0 3 * * *"
        keep: 7
        mode: pause
  ```

  `dest` omitted = the install's local destination. `mode` is ignored when
  the engine dumps. Panel self-backup stays admin-only, outside stack files,
  as today.
- **Destinations:** local directory on every install (default) plus S3
  extras, one interface. Ownership and visibility as today: per org, or
  admin-global and explicitly shared; an unshared one reads as not found.
- **Encryption at rest, v1:** every destination gets a random key at
  create, stored encrypted like any secret. Archives are `age`-encrypted
  with it, streamed between gzip and upload. Restore on the same install
  needs nothing typed. The panel self-backup cannot use a stored key (the
  master key is inside the archive): the installer prints a **recovery
  passphrase** once, and that is the one thing admins must save. Local and
  S3 share one code path.
- **Restore:** as today (pre-restore backup first, download, verify the
  archive before anything is wiped, stop, wipe, untar or `DROP SCHEMA` +
  psql, start), as a job with the per-volume lock. New: **cross-volume
  restore**, a run from one volume into another of the same kind and
  engine, e.g. prod dump into the dev volume. The restore job's input is a
  struct (source run, target volume), so a point-in-time target can be
  added later.
- **Retention:** keep-last-N, cron per schedule, run rows, as today.
- **Four shapes that keep PITR cheap later:** a managed instance may own
  more than one volume (data now, WAL later); the engine builds its own
  container command and config (`archive_mode` later); methods are per
  engine and per schedule; restore takes a struct, not two ids.
- **Not built, decided:** btrfs snapshots (one subvolume per volume, loop
  file or dedicated device, `nodatacow` for DB tiles; stock VPS images are
  ext4 so the loop file would be the common case); Postgres PITR (WAL-G or
  pgBackRest run by stackrd, Postgres archives to a local WAL volume, a job
  ships it; storage for a 1 GB DB with a week kept: 2 to 7 GB); Postgres
  replicas and managed-tile HA (single node is not HA; rides with
  clustering); owner-held backup passphrase (age keypair, public key on the
  box, nothing decryptable from the server, per destination). All in
  "Later".

## Auth

- Two roles in v1: **stackr admin** (everything) and **org owner**
  (everything inside their org).
- **One check, in middleware.** The rules live once in a small `authz`
  package with one function `can(user, verb, resource)`. Middleware calls
  it; handlers never re-decide; leaves and flows know nothing about auth.
  Both routers (web and API) use the same middleware and the CLI goes
  through the API, so every door gets the same answer. The old code had two
  diverging copies (`handlers/api/v1/auth.go`,
  `handlers/middleware/orgctx.go`).
- **URLs are nested:** `/:org/:stack/:env/:tile/...` for the API and the
  UI. Middleware resolves each slug in order, 404s early, checks org
  membership once. Handlers below already have org, stack, env and tile
  loaded and never re-fetch them.
- **Why middleware works for v1:** the only question is "admin, or member
  of the org in the URL", and the URL carries the org.
- **Ceiling, written down:** per-asset grants later ("deploy staging but
  not prod", "read variables but not secrets") need facts on the row, which
  middleware does not have. Then the same `can()` is also called from the
  orchestrator with the row in hand. Same function, new call site, no rule
  change.
- `can()` loads the user's role and org memberships once per request and
  reuses that set. A list view's per-row verdicts come from the same loaded
  set, never one store read per row.
- Job workers call flows with no user. Nothing to authorise there.
- API keys carry their user's abilities, live. Demote the user and the key
  loses the power too. Keys have no scopes in v1.
- API keys are org-bound (the reason old migration 002 exists). A key never
  carries more than its minter holds at mint time (B36).
- Raw Caddy config snippets are stackr-admin only.
- A param ref can never resolve another org's secret.

### Auth mechanics (as today, listed so nobody forgets to build them)

- First account on a fresh install becomes stackr admin. The installer
  prints the setup URL. Onboarding is gated until done.
- Invites: an org owner invites by email; the link creates the account and
  the membership in one step, burns the invite atomically, expires after 7
  days (B14, B15).
- CLI login: browser approves, the CLI receives a one-time code and swaps it
  for an API key at an exchange endpoint. The raw key never touches the
  browser.
- Passwords: minimum length only. Change requires the current one. No reset
  flow in v1; an admin can set a new one.
- Disabling a user follows the same rule as demoting one and closes its
  sessions and keys (B16).
- Sessions come from hamr's session manager. Nothing to build.
- Streaming surfaces (container logs, build logs) go through the same
  middleware as any request.

## Infrastructure

- **No Swarm.** Single node, plain containers, bridge networks. Everything
  below that Swarm used to do is our own code (rule 9).
- **Networks:** plain Docker networks, one per environment. Tiles in an
  environment reach each other by tile name; nothing crosses environments.
  One small network per shared-tile link. One **ingress network per exposed
  tile** (created when its first domain is attached, removed with the last):
  the tile's replicas and the proxy join it, nothing else does. The proxy
  never joins an environment network, so it can only reach what is public.
- **Service DNS and internal load balancing (the VIP):** every tile, even
  with one replica, gets a **pause container** on its environment network:
  a do-nothing image that only holds an IP, with the tile's name as its
  network alias. Docker DNS resolves the tile name to that IP. iptables DNAT
  rules in the host network namespace rewrite traffic for that IP to the
  live replica IPs (what Swarm and k8s do with IPVS). Replicas come and go,
  the name and IP never change, no client caches a dead replica. stackrd
  owns the rules: it runs with host networking and `NET_ADMIN` (the
  installer grants it), rewrites the rule set on every replica change, and
  rebuilds all rules from the database on start. Always on, no
  single-replica shortcut: a zero-downtime deploy is a two-replica moment
  even for a one-replica tile, and one rollout path beats two.
- **Public load balancing:** Caddy's own. The route's upstream list is the
  tile's replicas on the ingress network. Caddy runs the per-replica health
  checks and drains in-flight requests before an upstream is dropped. No
  VIP on the ingress side.
- **Deploys:** zero-downtime by default, our own rollout, one replica at a
  time: start the new container on both networks, wait until it is healthy,
  add it to the VIP rules and the Caddy upstream list, remove the old one
  from both, stop it. **This is new work:** the old health gate was
  Swarm-only. The new one polls the container's health status with a
  deadline of 60s + the tile's healthcheck start period (the old numbers).
  A tile with no HEALTHCHECK counts as healthy once it has been running
  for a grace period; the tile can set a health path in its config to get
  a real check. **Either way the gate reads two fields from the same
  inspect call:** healthy (or running past the grace period) **and**
  restart count still zero. A crash-looping container is "running" between
  crashes; any restart during the gate fails the deploy and the old
  container stays. The grace period for a tile with no HEALTHCHECK is
  10s.
- **Rollout for anything that mounts a volume is stop-then-start.** A
  service tile with a mount and every managed instance never has two
  containers on one volume: stop the old, start the new, gate, done. Such
  a tile is capped at one replica; the file refuses `replicas > 1` on it.
  Only mount-free tiles get the overlapping rollout above.
- **Build logs:** a file on disk per job plus a live stream, as today.
- **Replicas:** several copies of one tile on the node, behind the VIP
  internally and Caddy publicly. Scaling is the same rollout loop with a
  different target count.
- **Proxy:** Caddy, used as a Go library, runs as `stackrd proxy`: the same
  binary as the panel, in its own container. A panel restart does not drop
  tile traffic. The proxy is a config sink and nothing more: no Docker
  socket, no Docker knowledge. stackrd, which has the socket, connects the
  proxy container to each ingress network and pushes routes through Caddy's
  admin API (`internal/proxy`). The proxy container has one named volume
  for Caddy's data and config dirs (certs, autosaved config), created by
  the installer, so a restart comes back with its routes and never redoes
  ACME; stackrd also re-pushes the full config whenever it sees the proxy
  container start. The proxy is on no
  environment network, and a route's upstreams are stable replica names, so
  nothing about a swap needs the proxy to know Docker.
- **One package talks to Docker.** It takes a "what should run" description.
  That keeps the door open to swapping it for nomad, k3s or our own agents
  later.
- **Built images stay on the machine.** A single node needs no built-in
  registry.

## Domain resources

**darthvader 2026-09-25 (DECIDE 190): v0's model, ported as it is.**

- **One table, three levels.** `domain_resources`: instance (the server,
  seeded from the installer's root domain), org, stack. `host` is unique
  across the server. Each row carries `include_env_on_default` and an
  `acme_email`. A stack's reservations (`domains:` in the stack file)
  are its stack rows; the JSON column goes.
- **Visible, nearest first.** A stack sees its own rows, then its org's,
  then the instance's. `AutoHost` builds a tile's name from the nearest:
  `tile[.env].stack.org.<instance host>`, `tile[.env].stack.<org host>`,
  `tile[.env].<stack host>`; the env label is dropped on the stack's
  default env unless the resource says `include_env_on_default`. The
  default env is the ladder's top rung (darthvader 2026-09-25, DECIDE
  192). A tile domain row remembers the resource that named it
  (`resource_id`), and a resource that still names one cannot be
  deleted.
- **The stack file.** A tile domain is a literal, a `params.` ref,
  `auto: true` (the nearest resource names it) or `apex: <resource
  host>` (the tile takes the resource's host itself). The promote plan
  resolves both when it plans; no visible resource is a blocker.
- **Squat.** A host whose first label is another org's slug is refused,
  on resources and on tile domains alike; the reverse check on org
  rename stays. An org's own stacks never count against it.
- **Where they are made.** The wizard's domain step (prefill
  `<slug>.<instance host>`; Finish makes an undeclared org row when the
  org sees none), the org drawer's Domains section, `POST
  /domain-resources`, `stackr domain add`. Owner level. Deleting a
  resource that names live tiles is refused.
- **Managed tiles' `PublicBase`** comes from `AutoHost` too.

## v1 scope

- Deploy from git and from an image. Image watch, digest and tag-policy
  modes (see "Image watch").
- Environments, **promote (the main focus)**, rollback, releases (see
  "Promote and releases").
- Config-as-code: the stack file `stackr-compose.yml` (see "Promote and
  releases") and the org file `stackr-org.yml` (see "Org config file";
  darthvader put it back in v1 on 2026-09-25, DECIDE 180).
- PR environments: an env `from <pr branch>` with a `base_env`, made and
  removed by the connector.
- The param store: params and secrets in collections at org/stack/env,
  tile env as literals plus refs (see "Param store and refs").
- Domains and automatic TLS on Caddy, plus admin-only extra Caddy config:
  unprotected routes, WebDAV (e.g. ownCloud), routes to LAN IPs. Domain
  resources at instance, org and stack level with automatic hostnames
  (`auto:`, `apex:`), v0's model as it is (see "Domain resources";
  darthvader 2026-09-25, DECIDE 190).
- Orgs, users, the two roles, API keys.
- Volumes.
- Managed tiles: Postgres and S3 instances, slices, bindings (see "Managed
  tiles").
- Git connectors, GitHub App only in v1: one per org, resolved from the
  tile's `git_url` host. The connector clones, registers push webhooks
  itself, and is the base for checks and PR comments later. No per-repo
  tokens or deploy keys; no public-repo path without a connector. A
  `git_url` with no connector for its host is a typed error in the promote
  diff. More providers later.
- Registry credentials for pulling private images (the pull-credential half
  of the old `infra/registry`; the built-in registry half is later).
- Container logs and restart.
- Zero-downtime deploys and replicas.
- Backups: schedules on volumes, local and S3 destinations, `age`-encrypted
  archives, cross-volume restore (see "Backups").
- Installer and self-upgrade.
- API and CLI first. The UI comes after them.
- Settings: one catalogue of knobs, every surface enumerates the same one.
- Org delete and rename rules (has stacks, last org, squat check) live in
  `leaf/org`, unchanged in behaviour.

**Binaries:** `stackrd` (panel; `stackrd proxy`; `stackrd agent` later),
`stackr` (CLI), `stackr-install`. `proxyrelay` is dropped with port
forwarding.

## Build order

Each step is usable before the next one starts.

| # | Step | Done when |
|---|------|-----------|
| 0 | **Scaffold and docs** | Fresh `hamr new` on the new branch; `AGENTS.md` fixed (above); the rules written down; the handler-audit skill exists. |
| 1 | **Groundwork** | The promote and release model ("Promote and releases") is turned into the `releases`, `release_tiles`, `environments` and `jobs` schema first. Then the rest of the schema, then the package layout above; `service/internal/store` and `service/internal/docker` exist; the `depguard` rules; `service.New` and the orchestrator; `authz.can()` and the middleware; typed errors; `flow/jobs`; test harness (real SQLite in a temp dir, fake Docker). |
| 2 | **Docker wrapper** | Containers, networks, volumes, images, builds, logs, exec, driven only by resolved specs. No domain rule in it. |
| 3 | **Services** | Every v1 feature above exists as orchestrator methods, including managed tiles, Caddy route config and `stackrd proxy`. |
| 4 | **API and CLI** | The API is the only door. The CLI is a plain HTTP client with no rules of its own. |
| 5 | **Installer and self-upgrade** | v1 installs on the test VM from nothing and upgrades itself. |
| 3b | **Cron and function tiles** | Run-to-completion kinds, `runs` table, `flow/run`, schedule and pause, API and CLI verbs. Added 2026-09-24: v0 had them from day one and step 1 fixed the kinds without them. `docs/rewrite/tasks/step-3b.md`. |
| 3c | **Traffic** | conntrack sampler, per-pair bytes/s in memory, SSE; drawn as lanes by the canvas. `docs/rewrite/tasks/step-3c.md`. |
| 6 | **UI** | templ+htmx, server-rendered; see "UI stack" and `docs/rewrite/ui-plan.md`. Everything is a graph: four canvases (home, org, stack, env), one canvas element; anything else is a drawer or a dialog, never both. hamr's dev tooling stays. Old templates are reference for *what* a screen shows, never for *how*; no copy-paste. The step starts with the shared component set and every screen is built from it, so each UI behaviour lives in one place. |
| 7 | **Federation** | See "Later options". |
| 8 | **Multi-node** | See "Later options". |

Web handlers are dumb handlers like the API's: they call the orchestrator,
map the result to a view struct, render. A UI that cannot reach the store
cannot hold business logic. That backs up rule 1 for free.

## UI stack

**Settled 2026-09-23.** templ+htmx, server-rendered, the server owns the
state. No JS framework: longevity (htmx and templ barely move; every SPA
drags npm churn) and the panel has little client state. The canvas, if it
ever comes, gets its own island then.

- **Folders:** `ui/components/` (one templ file per component: form,
  table, status badge, dialog, log pane, job status), `ui/pages/{org,stack,
  env,tile}/` mirroring the URL scheme (layout plus partials per page),
  `ui/static/js/` (custom elements, see below).
- **Every partial is a route.** htmx swaps target a partial's URL. One
  helper reads `HX-Request` and serves the full page or the fragment from
  the same handler. `hx-push-url` on navigation, so deep links and the back
  button work.
- **URL is the state.** Tabs, filters, selected env: query params. Nothing
  to lose on refresh.
- **Live data is SSE.** htmx's SSE extension on job status, logs and deploy
  progress; the server pushes rendered fragments. No polling.
- **View structs.** templ takes small view types built by the handler;
  it never sees a domain type or the orchestrator.
- **JS rules, so it cannot become spaghetti:**
  - JS exists only as a **custom element** (`<log-pane>`, `<confirm-
    dialog>`): one file, one class, one tag. No global functions, no inline
    handlers, no scripts inside templ files. The browser's
    `connectedCallback`/`disconnectedCallback` handle htmx swaps; no
    after-swap init hooks.
  - **No JS fetches.** htmx and SSE do all server traffic. An element
    touches only its own DOM; data it needs arrives as attributes.
  - **No JS imports JS.** Elements talk upward with DOM events only.
  - **htmx attributes are authored in templ only**, on the wrapper
    component or its parent, never inside a custom element's own DOM. An
    element that wants a server call fires an event; the templ parent
    carries the `hx-trigger`. No shadow DOM (htmx cannot see into it).
  - One templ component wraps one custom element (`@LogPane(view)` emits
    `<log-pane …>` with attributes from the view struct). `templint` gets
    one allowlist entry per tag.
  - **TypeScript, type-checked, not bundled.** `tsc` emits plain ES modules
    loaded with `<script type="module">`; `tsc --noEmit` runs in `make
    lint`. No Node at runtime, no bundler.
  - **Budget:** an element over ~150 lines is a smell, over 300 is refused.
  - **Whitelist:** seven tags. Session A: `log-pane`, `confirm-dialog`,
    `flash-toast`, `theme-toggle`. Graph (`docs/rewrite/ui-plan.md`):
    `graph-canvas`, `graph-node`, `side-drawer`. Adding one is a plan
    item. The graph elements do geometry only; every card is templ HTML
    carrying htmx, so the JS never fetches.
- htmx and its SSE extension are vendored, pinned, ~20 KB total.

## What comes over from the old code

Old paths, all under `internal/stackrd/` unless shown otherwise. Copy
selectively. Nothing comes over wholesale.

| From | What to take | What to leave |
|------|--------------|---------------|
| `infra/runtime` | the plain-container path (`ContainerSpec`), network/volume/image calls, buildx and log calls | `service.go` and `swarm.go` (all Swarm); the parsers parked there to dodge import cycles go where they belong |
| `infra/deploy` | git clone/checkout, image naming | the engine: rules, status mapping, supersede logic |
| `internal/installer`, `internal/installspec` | host install steps, the root/domain grammar | `swarm init` |
| `config/secrets` | encryption at rest (AES-GCM) | — |
| `config/varref` | the `${{ }}` parser, the never-run-unresolved contract, `UnsetError` vs hard error, binding-gated managed outputs, the endpoint outputs, cross-scope network joining; rewritten inside `leaf/params` against the grammar in "Param store and refs" | the `vars`/`secrets` buckets, tile-owned variables as outputs, nested refs and cycle detection, literal-URL dep sniffing, its separate home below the services |
| `service/release.go`, `service/tilediff.go` | "which config keys moved decides which side effects a write earns" (restart vs redeploy vs route rewrite) | the queue keying and the promote-by-commit flow; the release model is new |
| `config/stackconf` | the file grammar (`stackconf.go`), merge of includes and env overlays, `plan.go`'s diff (tile create/update/delete, domains, slices), `deps.go`, `backups.go`, `slices.go` | `apply.go`'s job wiring and Swarm calls, `staging.go` (staged UI edits), `moved.go`, `job.go`, apply policy, `traefik_override`; the applier becomes the promote flow's reconcile step |
| `infra/githubapp` | `githubapp.go`: app auth, installation tokens, webhook receive and verify, clone URL | `ci.go` and `feedback.go` (CI gate and PR comments are later) |
| `infra/workqueue` | the durable-job idea, re-done as `flow/jobs` | the code |
| `service/tilelifecycle.go`, `service/tilevalidate.go` | create/update/delete of a tile and the per-field refusal vocabulary | — |
| `config/runpolicy` | the registry of runnable tile kinds and what each accepts; it is what produces the resolved spec the Docker wrapper takes | — |
| `service/access.go`, `service/revoke.go` | the existing `can()` and "close sessions and keys when standing changes" | the second copy in the handlers |
| `config/envops` | environment clone and teardown | — |
| `service/container.go` | the one guard over start/stop/remove/terminal | — |
| `infra/managedtiles` | `engine_postgres.go`, `provision.go`, `provision_s3.go`, `ready.go` as the shape of the `ManagedTile` interface; the slice naming in `sqlnames.go` | its own `Deploy`/`Start`/`Stop`/`Remove` (second deploy path), `fork.go`, browse and stats |
| `service/managedinstance.go` | the scope rules (env / stack / org) | — |
| `store/db/migrations/002_api_key_org.up.sql` | `api_keys.org_id`: keys stay org-bound | — |
| `config/settings` | `keys.go` and the cascade (`Resolve`/`Check`/`Merge`), moved into `SettingsService` | the form binder |
| `internal/deploystate` | "is this deployment still live" — one home, the model to copy | — |
| `infra/imagewatch` | registry digest polling, the registry client | — |
| `service/imagewatch.go` | reference only | the old notify/auto policy; see "Image watch" |
| `infra/backup` | S3, tar and exec mechanics, the restore order (pre-restore backup, verify before wipe, mutex per volume), scratch-space preflight | `authz.go` (auth is middleware only); `VolumeFor`, `restorable`, the prune rules (these go to `leaf/backup`); per-tile schedules (they move to the volume) |
| `service/svcerr`, `service/scheduler` | error types, the cron runner for schedules | — |
| `internal/netaddr` | as is | — |
| `store/db/migrations/001_initial.up.sql` | the starting point for v1's schema: keep the tables v1 needs, with their columns as they are (the file was dumped from a migrated DB, so no column was lost) | the tables for features outside v1; check that foreign keys are actually enforced, because the old store cascaded deletes by hand |
| `infra/proxy`, `service/proxy` | reference only: basic auth, trusted proxies, ACME, env-aware domains | the Traefik file writer |
| `internal/cli` | reference only: command names and UX | the code |
| `handlers/web/**/*.templ` | reference only, for step 6 | the code |

## Spec and tests from the old repo

The sweep docs are the spec for the new services. Read them from the old-code
worktree:

- `docs/plans/service-extraction/12-business-logic-plan.md`: the joins (§3)
  and the bug register (§4).
- `docs/plans/service-extraction/shards/sweep-*.md`: per-area evidence.
  `sweep-h-deploy.md` holds the engine-vs-service deploy rule map.

Bugs from the register that become v1 test cases:

| Bug | Test in the new code |
|-----|----------------------|
| B34 | A param or settings change on a running tile, in any env, restarts it on the image from the env's current release, never the branch head. |
| B2 | A rollback through the API is refused by the same rules as the panel's. |
| B4 | Writing `type: param` over an existing secret is refused, at every scope, before any row is touched. |
| B35 | Params have a merge verb. `stackr params set` next to a masked secret succeeds and keeps the secret. |
| B5 | `${{ params.x.y }}` resolves env before stack and never reaches org. |
| B37 | A secret the viewer may not see is never loaded, not just redacted afterwards. |
| B3 | Every store write round-trips: write through the service, read back, assert each field. |
| B1, B21, B22 | The CLI sends only what the user gave it and never blanks fields on the client side. `-y` skips the prompt; it never means force. |
| B14, B15 | An invite is burned atomically before the join and refused if already used. One invite path, with an expiry cap. |
| B16 | One "can this account lose its powers" rule covers both demote and disable. |
| B24 | A typo in a number field is refused, never read as "not set". |
| B25 | Every deploy is a job row to poll, with a 30-minute cap. A job outlives its HTTP request on purpose; what is banned is work with no row. |
| B26 | One tile-create path, one whitelist of what a tile kind may carry. |
| B29 | Redeploy is gated by the same rule as deploy, not a pre-check that disagrees with it. |
| B36 | A key minted by a user never holds more than that user holds. |
| B20 | "What blocks this promote" has one answer, and the API and the panel show the same one. |
| B31 | One nil-vs-false convention for HTTPS. |
| B0 | Boot wiring builds each service once, before anything calls it. |

## Later (not v1)

**Features:** managed-tile forks, SQL/S3 browse and stats, more managed
engines and engine plugins, CI gate, GitLab and other
connectors, per-repo git tokens and deploy keys, built-in registry, storage
shares, collection links (stack collection → org collection), an `env.`
ref scope, volume moves, metrics (CPU, memory, disk, slice stats), search,
share links, notifications and mail, port forwarding, audit log,
fine-grained permissions, btrfs volume snapshots, Postgres PITR, managed-tile
HA (with clustering), owner-held backup passphrase (see "Backups").

**Infrastructure options.** Recorded only, not designed:

- **Federation.** A full stackr on every node. At launch, one master takes the
  writes and the rest are read replicas. Accounts work across nodes, orgs are
  pinned to a node, and moving an org means zip + rsync. No cross-node
  traffic or replicas.
- **Agent** as a `stackrd agent` mode of the same binary and image (as it is
  today), so agents always update along with the panel.
- **Exit nodes:** containers able to use more than one outgoing IP.
- **Real multi-node:** either an existing platform (small k3s, nomad) or our
  own: networking (permission-based like Kubernetes, or virtual-ethernet like
  Docker), bridged over WireGuard, plus our own registry and our own
  replica/rollout handling. The controller (web/api owning the DB) gets
  replicated across nodes. Multi-cloud (home + DigitalOcean + Hetzner) comes
  after that.
- **In-process proxy flag** for tiny boxes. Only if someone runs one where an
  extra Go process matters.

## Open questions, settled when their step comes

- **The promote model:** see "Promote and releases"; finished in step 1.
- **The UI stack:** settled, see "UI stack".
- **Per-environment grants** (e.g. deploy dev, read prod): when fine-grained
  permissions land.

## Status

Draft by Opus, reviewed 2026-09-22. Eleven gaps settled in round one. Round
two (logic review) settled: the tree (orchestrator door, leaf = row + world
object, flows compute and pass down), auth in middleware, stacks as a leaf,
volumes env-scoped, managed tiles in v1, promote designed before the
schema; every container op is a job and promote runs deploy in-process;
rule 9 split into domain rules (as today) and Swarm mechanics (written
here or not built); service DNS is a per-tile pause container plus host
DNAT rules, always on; public balancing is Caddy's own over a per-tile
ingress network; the proxy is a socket-free config sink that stackrd wires
up; the health gate also requires zero restarts so crash loops never pass.
Round two is closed. Round three settled promote and releases (release
snapshots with digests, config in the release, one verb, per-env
`from`/`auto`, Terraform drift model, file keep/cut list, PR envs in v1,
GitHub connector only, image watch with two modes, orphaning instead of
force, local backup destination), the param store (name, collections,
scopes, ref grammar, tile outputs, declassify never, name grammar) and
backups (volume as the unit, engine picks the method, block on the volume,
encrypted archives, cross-volume restore; snapshots, PITR, HA and
owner-held keys parked with their shape kept) and the UI stack (templ+htmx,
SSE, custom elements only, TypeScript checked not bundled). Fable's
consistency review (2026-09-23) fixed nine contradictions: stop-then-start
rollout for anything with a mount, lock set per job, volumes are their own
table and leaf (drawn as a card, not a tile), leaves for release/job/
backup/connector and no `deployments` table, auth struck from flows,
`leaf/tile.Exec()`, chip-only image watch, `dest` ref allowed, grace
period and proxy volume written. Design rounds are closed. 2026-09-24: the
builder's working method is written under "Method" (extracts per row, fresh
session per step, wipe by sub-agent). Next: per-step task lists, then the
extracts, then the wipe.

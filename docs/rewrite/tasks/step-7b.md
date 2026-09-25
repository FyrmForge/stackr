# Step 7b: managed tiles, allow list and slice tiles

Read first: `docs/rewrite/PROGRESS.md` DECIDE 194 (the five calls; every
rule below is there), 193 and 30, `AGENTS.md` "Layering rules",
REWRITE.md "Managed tiles" and "Param store and refs". Code this step
changes: `internal/service/internal/flow/managed/` (`engine.go`,
`flow.go`, `postgres.go`, `s3.go`), `leaf/managed`, `leaf/params/ref.go`
(the resolver), `flow/promote/stackfile.go` + `plan.go` (grammar, apply),
`flow/deploy` (the consumer and slice deploys), `internal/service/managed.go`
+ `tile.go`, `flow/graph`, the tile drawers, `cmd/stackr`, the API.

What it is: a managed tile (postgres, s3) says who may connect with an
allow list of `org:stack:env:tile` patterns. A slice is a tile of its own
in the consumer's stack: one database or bucket that many tiles use. Each
consumer gets its own cred on the slice with read or write access. Scope
is gone.

Branch `rewrite-step-7b`, stacked on `rewrite-step-7`.

## The grammar

Infra stack:

```yaml
base:
  tiles:
    pg-db:
      kind: managed
      engine: postgres
      env_pairs:
        dev: staging
        staging: staging
        production: production
      allow:
        - testorg:shop:*
        - testorg:blog:*:api
```

Shop stack:

```yaml
base:
  tiles:
    api-db:
      kind: slice
      provision_from: infra:${{ env.name }}:pg-db
      default_access: write
    api:
      image: ghcr.io/acme/api:1.4
      env:
        DATABASE_URL: ${{ tile.api-db.DATABASE_URL }}
    reporter:
      kind: cron
      schedule: "0 3 * * *"
      slice_access:
        - from: api-db
          access: read
      env:
        DATABASE_URL: ${{ tile.api-db.DATABASE_URL }}
```

Rules:

- `allow:` patterns are four segments, `*` allowed; a trailing `*`
  swallows the rest (`testorg:*`); a `*` in the middle matches one
  segment. The first segment must be the tile's own org slug; `*` or
  another slug is refused at parse. No list = the tile's own env only.
- `env_pairs:` maps a consumer's env name to one of this stack's env
  names. A consumer whose env name is not a key is a blocker on its
  promote plan ("infra's pg-db has no env pair for dev"). No map = the
  consumer's env name must exist in this stack as written.
- `provision_from:` is `<stack>:<env>:<tile>` in the consumer's own org.
  `${{ env.name }}` and `${{ params.<c>.<n> }}` refs are allowed in it;
  the env segment goes through the instance's `env_pairs`. The target must
  be a managed tile whose allow list matches the slice tile's address.
- A PR env (`base_env` set) that has no `env_pairs` key of its own maps as
  its base env, the way the promote plan already falls back to the base
  env's file section; its address for `allow:` stays its own slug
  (`smoke:shop:pr-12:api-db`; the allow check is one per slice tile, never per consumer).
- `default_access:` `read` or `write`, default `write`.
- `slice_access:` on any consumer tile: `from` is a slice tile slug in the
  same env, `access` read or write. A consumer that refs a slice without a
  `slice_access` entry gets the slice's default.
- The consumer's refs stay `${{ tile.<slice>.<output> }}`; the outputs are
  the engine's bindings (postgres: `DATABASE_URL`, `PGHOST`, `PGPORT`,
  `PGDATABASE`, `PGUSER`, `PGPASSWORD`; s3 as today) minted for that
  consumer's cred.
- Gone: the per-consumer `slices:` list on tiles, `shared:` (still refused,
  now with "use a slice tile"), `scope`, `${{ stack.<slug>.<output> }}`,
  `${{ org.<slug>.<output> }}`.

## Tasks

1. **Store.** `managed_instances` loses `scope_kind`, `scope_id`, gains
   `allow` (JSON list) and `env_pairs` (JSON object); `provisions` becomes
   the slice's unit: `tile_id` (the slice tile, FK CASCADE) replaces
   `consumer_tile_id`, keeps `instance_id`, `db_name`, owner creds,
   `public`, `on_remove`; new table `bindings` (id, provision_id FK
   CASCADE, consumer_tile_id FK CASCADE, access CHECK read|write,
   db_user, db_password encrypted, outputs encrypted JSON, created_at;
   UNIQUE (provision_id, consumer_tile_id)). `volumes.scope_kind` stays
   (org storage shares use it) but a managed tile's volume is env-scoped.
   `tiles.kind` CHECK gains `slice`; a slice tile stores `provision_from`
   as written and `default_access` in its columns (two new columns, no
   JSON). Migration `003_rest` edited in place, up and down; `store`
   types; `cascade_test.go` covers slice → bindings and consumer → binding.
   Done when: round-trip tests; cascade test green.

2. **Address and allow.** `leaf/managed` (or a small `address` package
   under `flow/managed`): `ParseAllow(orgSlug, list) ([]Pattern, error)`
   (own-org rule), `Match(patterns, addr Address) bool`,
   `ParseTarget(s) (Target, error)` for `provision_from`. Pure, table
   tests: trailing star, middle star, own-org refusal, exact match, the
   examples in DECIDE 194. Lives in `internal/service/internal/address`
   (the lint config keeps flows from importing each other; `slug` sits
   there for the same reason). A tile slug is a DNS alias, so `pg_db` is
   refused; the grammar uses `pg-db`. An `allow:` list replaces the
   own-env default, it does not add to it (DECIDE 195).
   Done when: tests green; a fuzz-free but exhaustive table.

3. **Stack file.** `flow/promote/stackfile.go`: `allow`, `env_pairs` on
   managed tiles (refused on other kinds), `kind: slice` with
   `provision_from` and `default_access`, `slice_access` on any tile;
   `slices:` removed (a file with it fails with "slices: is gone; declare
   a slice tile, see DECIDE 194"); `shared:` message updated. `plan.go`:
   a slice tile's target is resolved at plan time through the org's
   stacks, the target's `env_pairs`, and its allow list; unknown stack,
   env pair missing (after the PR env → base env fallback), tile not
   managed, or not allowed are blockers with
   the exact reason; the plan's `Change` rows show a slice as `slice
   api-db → infra/staging/pg-db (write)`.
   Done when: table tests for every blocker and the happy path; the file
   round-trips through the promote apply into tile rows.
   As built: `slice_access` is refused on managed and slice tiles and lives in a new `tiles.slice_access` JSON column.
   As built: `Load` takes the org slug, so `allow` is checked at parse; `${{ env.name }}` landed here, the plan needs it.
   As built: three more blockers: "infra has no environment qa", "infra/staging/pg-db has no instance yet; deploy it first", "provision_from needs params.x set first".
   As built: a moved target or default access redeploys every tile that refs the slice; allow and env_pairs rows write the instance and redeploy nothing.

4. **Resolver.** `leaf/params/ref.go`: `${{ env.name }}` (`KindEnv`,
   from `Snapshot.Env`); `KindStack` and `KindOrg` tile refs removed
   (`Snapshot.Stack`, `Snapshot.Org` go; `org.params.` and
   `org.backups.` stay); `${{ tile.<slug>.<output> }}` on a slice tile
   resolves to the consumer's binding outputs (`Source.Managed` becomes
   `Source.Slice`, `Attached` means a binding exists for this consumer).
   `Where` learns the slice tile is allowed in `provision_from` only.
   Done when: resolver tests; the never-run-unresolved contract holds.
   As built: tile and self output names are env-key shaped (`DATABASE_URL`); param names stay lower-case.
   As built: `stack.<slug>.x` and `org.<slug>.x` fail Parse with `params.Removed`; the graph's ref ghost for them is gone.
   As built: a managed tile is no ref source at all; `managed.New` takes the binding store, `Bound` maps slice tile id → binding, `Outputs(binding)` decodes it; a slice source's `Network` is task 5's.

5. **Deploy and placement.** `flow/managed`: `Provision(instance, slice)`
   on slice tile deploy (create the database with the owner cred, once);
   `Bind(instance, slice, consumer, access)` mints the consumer's user
   (postgres: `GRANT` read-only or full on the database; s3: a key with
   read or read-write policy on the bucket) and returns the bindings;
   `Unbind`; `Drop` on slice tile removal per `on_remove`. `flow/deploy`:
   a slice tile has no container; its deploy is Provision + status; a
   consumer's deploy asks for its binding, joins the instance's network
   (the per-instance network from "Infrastructure"; a slice in another
   stack joins the same one), resolves refs. Consumer removed → Unbind;
   slice tile removed → Drop or keep. Instance's allow list edited →
   nothing is torn down; the next consumer deploy that no longer matches
   fails with the reason (ponytail: no eager revoke).
   Done when: service tests with the fake docker: infra + shop from the
   two files above; api gets a write user, reporter a read user; both on
   the instance's network; removing reporter drops its user; removing
   api-db with `on_remove: drop` drops the database.
   As built: `on_remove` is a slice tile key (stack file, `tiles.on_remove`, DECIDE 199), default keep; `provisions` has no copy. An ephemeral env always drops.
   As built: a slice's database or bucket is `<stack>_<env>_<slice>` (`-` for s3), never suffixed; a name another row holds is refused (DECIDE 197).
   As built: build defaults (branch, dockerfile, context) moved from `leaf/tile` Create into `Validate`, so a git-built cron re-plans clean.
   As built: the instance network is `stackr-managed-<instance id>`; the instance's alias on it, `<slug>-<first 8 of its id>`, is the postgres host.
   As built: s3 hands out the root key for owner and every binding; `access` is recorded, not enforced.
   As built: plan and deploy share `address.Resolve`; a consumer deploy re-resolves every slice it uses and fails with the plan's blocker text.
   As built: removing an instance tile that still holds slices is refused; its network goes after its container.
   As built: `AttachSlice`/`DetachSlice` refuse until task 6; the graph skips a slice tile's own card until task 7.

6. **Verbs, API, CLI.** `SetManagedAllow(ctx, tileID, list)`,
   `SetManagedEnvPairs(ctx, tileID, pairs)`, `CreateSliceTile(ctx, envID,
   slug, from, defaultAccess)`, `SetSliceAccess(ctx, consumerID, sliceID,
   access)`, `Bindings(ctx, sliceID)`; `SetInstanceScope`, `AttachSlice`,
   `DetachSlice`, `Slices` removed with their routes and CLI. Routes under
   the tile; `stackr tile allow <tile> --add/--rm`, `stackr tile env-pairs
   <tile> a=b ...`, `stackr slice add <env> <slug> --from --access`,
   `stackr tile access <consumer> --slice <slug> --access read|write`;
   `docs/openapi.json` regenerated; `docs/rewrite/verbs.md`.
   Done when: api tests incl. a member 403 on allow edits; cli tests.

7. **UI.** Managed tile drawer: Allow list (rows, add pattern with the
   own-org prefix filled, remove) and Env pairs (rows, add) replace the
   scope select; slice tile drawer (`kind: slice`): target, default
   access, consumers with their access and a change control, `on_remove`;
   consumer tile drawer: the old Slices tab becomes Access: the slices it
   refs with their access. Graph: a slice tile is a card in its env with
   `EdgeRef` from each consumer and `EdgeShared` to the instance (a ghost
   card when the instance is in another stack, as today's ghost). Wizard
   and org drawer untouched.
   Done when: drawer tests for the three drawers; `make templint` clean.

8. **VM proof.** Two stacks bound to two repos (the test repo and a
   second branch of it as the infra stack): infra pushes pg-db with the
   allow list and env pairs; shop pushes api-db + api + reporter; the
   promote plan shows the slice row; deploy; api writes a table, reporter
   can read it and cannot write (psql through `stackr tile exec` or the
   container); shop's staging env resolves to infra staging through env
   pairs; an allow entry removed → the next shop deploy blocks with the
   reason; remove reporter → its user is gone; playwright: the three
   drawers.

9. **Done gate.** `make build`, `make lint`, `make test`, `make templint`;
   PROGRESS.md ticks; local commit; stacked PR only on "commit and push".

## Not in this step

Network policy inside an env (which tiles of one env see each other),
cross-org allow, moving a slice to another instance, per-slice quotas.
Each is Later or has a DECIDE item.

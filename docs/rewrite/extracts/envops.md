Source: config/envops/ (envops.go, clone_test.go, teardown_test.go, ops_test.go, envops_test.go)
Commit: c2423f0
Taken: what an environment clone copies and resets, what teardown removes and in what order
Cut: every store and sibling-service write, the Docker/proxy/netpool calls, the managed-instance provisioning, the scheduler reload
Cuts belong to: flow/env-clone, flow/env-teardown, leaf/tile, leaf/variable, leaf/domain, leaf/resource

## Rules as today (spec)

- A clone copies tiles, their variables (secrets included) and their
  provisioned slices; it never copies domains or volume data.
- Every copied tile gets a fresh identity and loses everything that belonged
  to the old environment (network, home node, run history).
- A base db password appearing anywhere in the copy is rewritten to the
  clone's new one, in one pass after every password is minted.
- An `auto` domain copies as *intent*: one freshly generated hostname, not
  the base tile's host.
- Teardown runs tiles → env slices → env variables → tile overlays → env
  overlay → env row → scheduler reload, and the two overlay releases gate the
  row delete.
- Ephemeral envs drop their slices; every other type detaches and keeps the
  data. Volumes are kept either way.

### What a clone copies

Base env → new env, config only. Nothing is started. Taken as the rule set,
not the loop: the leaf owns the `environments` row, so the base env's tiles,
variables, domains and provisions all arrive as arguments and the new rows go
back to the flow to write.

**Tiles** — the row is copied with a fresh identity:

- new id, new webhook token, new managed-db password
- `created_at`/`updated_at` = now; `last_run_at`, `last_status`, `last_output`
  cleared
- `shared_net_name` and `home_node` cleared. Both belong to the environment
  the tile came from, not to the tile: the first carried a PR preview onto
  production's database network, the second pinned the copy to the original's
  machine, whose volume it does not have. Placement and the net pool fill them
  in on the first deploy in the new env.
- volume tile: `volume_name` cleared (fresh empty volume) and
  `attached_tile_id` remapped through the base→clone id map
- managed tile: new credentials minted **first**, old→new password kept in a
  rewrite map so the service tiles can be rewritten in the second pass

**Passwords** — every occurrence of a base db tile's old password, in a tile's
env blob and in every copied variable value, is replaced by the clone's new
one. Hosts and slug aliases are left alone: they are per-env-network.

**Variables** — every variable of the base tile, *secrets included*. The env
blob only carries the non-secret ones, so copying the tile row alone silently
drops every secret the tile needs. `${{ ... }}` references are copied verbatim
— they resolve against the clone's own environment, so rewriting them here
would break them. Each copied secret is audited (actor `system:env-clone`):
the plaintext now lives in a second place and nothing used to record that.

**Domains** — not copied. Hostnames are globally unique. What carries is the
*intent*: if any domain of the base tile has `auto` set, the clone gets one
freshly generated hostname. First `auto` wins, one per tile.

**Volumes** — never. A clone gets an empty volume, never the base env's data.

**Provisioned slices** (a logical database or bucket inside a shared instance)
— per-consumer, so the clone gets its own with its own credentials, never a
second consumer of the base env's data. Two kinds, both best-effort:

- consumer-owned (the provision row names a consumer tile): provision fresh
  from the same instance, then repoint that tile's variables from
  `tile.<oldSlug>.` to `tile.<newSlug>.`
- env-level, config-declared (no consumer, not orphaned, has a resource slug):
  clone the slice into the new env **under the same slug**, so the config's
  refs keep resolving without rewriting, then bind every cloned tile whose env
  blob mentions `tile.<slug>.`

Best-effort is deliberate: provisioning needs the instance running, and a
clone that mostly works beats a clone that fails outright. A reference left
pointing at the base env's resource fails loudly at deploy — the resolver
refuses an unbound resource — rather than quietly sharing another
environment's data. A repoint write that is dropped is *not* cosmetic: the
variable keeps pointing at the base env's database and nothing says why.

**Layout** — card positions transfer. Slug-keyed node ids copy verbatim
(clones keep slugs); `resource:`, `job:` and `forward:` ids are uuid-keyed and
point into the source env, so they are skipped and those cards auto-place.
Cosmetic: a failed layout copy must never fail a clone.

### What teardown removes, in what order

The order is load-bearing, as today:

1. per tile — detach from the env network, drop the proxy route, reclaim that
   tile's slices
2. env-level slices — no consumer tile, so step 1 never sees them
3. env-owned variables
4. per-tile shared overlays — a **second** pass, after every service is gone
5. the env overlay
6. the `environments` row (tiles cascade)
7. scheduler reload — the cascade took this env's cron and backup rows with
   it, and without the reload the orphaned entries keep ticking against tiles
   that are gone
   `// extract: dropped Sched.Reload, belongs in flow/env-teardown`

Steps 1 and 2 reach Docker and the proxy:
`// extract: dropped envnet.TearDown and PX.DropTile, belongs in flow/env-teardown`

Step 4 is separate from step 1 because draining detaches every service on the
network: releasing a shared instance's overlay while its consumer tiles are
still up would detach services that still exist. Org-scoped instances are
skipped — hosted in this env but shared across the org's stacks, owned by the
org config.

Steps 4 and 5 **gate** step 6. The pool's free list is computed from live
rows, so deleting the env row while the network still carries the old tenant's
services hands that network, with those services, to whatever claims next —
possibly another org. Nothing self-heals; the pool sweep runs at boot only.
The operator has to see the error, and it has to name the overlay. Retry is
"delete again": every step is idempotent on an already-empty env.

Step 3 exists because variables have no foreign key to environments — nothing
in the database removes them when the env goes. Without it every closed PR
leaves a live minted secret under an owner id that no longer resolves, and
they accumulate for as long as the server runs. Delete by owner, not by name.

Volumes are kept, matching a single tile delete.

## Kept code

```go
// cloneTile resets one base tile's row onto the clone: same config, fresh
// identity. Returns the managed tile's old password so the second pass can
// rewrite the siblings that mention it, "" for a tile that has none.
// extract: fact "the base env's tiles" now passed in
// extract: fact "which base tiles carry an auto domain" now passed in
// extract: dropped store.CreateTile, belongs in flow/env-clone
// extract: dropped audit.Record for each copied secret, belongs in flow/env-clone
func cloneTile(t *Tile, envID string, now time.Time) (oldPassword string) {
	t.ID = uuid.New().String()
	t.EnvironmentID = envID
	t.WebhookToken = uuid.New().String()
	t.CreatedAt, t.UpdatedAt = now, now
	t.LastRunAt, t.LastStatus, t.LastOutput = sql.NullTime{}, "", ""
	// extract: dropped t.Status = "idle", no status column writes in a leaf
	// Both belong to the environment the tile came from, not to the tile.
	t.SharedNetName, t.HomeNode = "", ""
	if t.IsVolume() {
		// Fresh empty volume attached to the cloned counterpart. Clones never
		// share the base env's data.
		t.VolumeName = ""
	}
	if t.IsManaged() {
		// Minted before any sibling is rewritten, or a service tile copied
		// first still holds the base env's password.
		oldPassword = t.DBPassword
		mintDB(t)
	}
	return oldPassword
}

// rewrite replaces every base db password with the clone's new one. Applied to
// a tile's env blob and to every copied variable value. Hosts and slug aliases
// are untouched: those are per-env-network.
func rewrite(s string, passwords map[string]string) string {
	for old, fresh := range passwords {
		s = strings.ReplaceAll(s, old, fresh)
	}
	return s
}

// layoutCarries: slug-keyed positions transfer verbatim (clones keep slugs);
// uuid-keyed ones point into the source env, so those cards auto-place.
func layoutCarries(nodeID string) bool {
	kind, _, _ := strings.Cut(nodeID, ":")
	return kind != "resource" && kind != "job" && kind != "forward"
}

// reclaim decides what happens to a slice when its environment goes.
//
// An ephemeral env's slices are dropped outright, whatever on_remove says: a
// PR env's logical database has no life after the PR, and provisions carry no
// foreign key, so leaving one leaks both a row pointing at a deleted tile and
// a database inside the shared instance nothing will ever reclaim. A static
// env detaches instead and keeps the data.
// extract: fact "this env's type" now passed in
// extract: dropped DBs.DropDB / DBs.Detach, belongs in flow/env-teardown
func reclaim(envType string) string {
	if envType == "ephemeral" {
		return "drop"
	}
	return "detach"
}
```

## Notes for the builder

- `leaf/environment` owns the `environments` row and nothing else. Both
  operations here are flows; what comes over is the decision table above plus
  these four helpers. Every `o.Store.*`, `o.Tiles.*`, `o.Vars.*`,
  `o.Domains.*` and `o.PX.*` call is a cut.
- The clone is two passes on purpose: mint every managed password first, then
  rewrite, or a service tile copied before its database still holds the base
  env's password.
- The auto-domain step is an *intent* copy, not a row copy. Do not let the
  new `EnsureAuto` equivalent run more than once per cloned tile.
- Teardown's release-before-row-delete is the one place an error must abort
  the rest. Everything else in teardown is best-effort — a container that is
  already gone must not strand the rows.
- The two slice passes (per-tile, then env-level) must both stay. Dropping the
  env-level one leaks every config-declared database.
- DECIDE: the old `Ops` struct carried nine services because clone and
  teardown reach across six tables. In the new layering that is one flow with
  six leaf calls, which is the point — but it means the flow, not the leaf,
  owns the ordering rules above.

Size: source 718 lines, extract 213 lines

# service/managedinstance.go

- **Source:** `service/managedinstance.go` (+ `service/managedinstance_test.go`)
- **Commit:** `c2423f0`
- **Taken:** the scope rules — how `scope_kind`/`scope_id` are set for env, stack and org scope, who may attach to an instance, what a scope change may and may not do, and what happens to slices when a consumer or the instance goes.
- **Cut:** everything else in the file — create/update/validate plumbing, the staging and config-file gates, deploy and status writes, the notifier, and the managed-resource/output/binding CRUD pass-throughs.
- **Cuts belong to:** `flow/deploy` (deploy and the status column), `leaf/managed` (the instance row, the provisions and resource rows), `leaf/tile` (the ordinary tile teardown), `flow/config` (staging and the file gate), middleware `can()` (who may).

## Scope

```go
// Scope resolution. The scope id is derived from the instance's own stack,
// never taken from the caller, so no request can point an instance at another
// tenant. An unknown scope name is refused, not coerced: the panel used to
// fall through to "env", so a typo silently narrowed the sharing of an
// instance other stacks were already provisioning from.
func ApplyScope(t *Instance, stackID, orgID, scope string) error {
	switch scope {
	case "", "env":
		t.ScopeKind, t.ScopeID = "env", ""
	case "stack":
		t.ScopeKind, t.ScopeID = "stack", stackID
	case "org":
		t.ScopeKind, t.ScopeID = "org", orgID
	default:
		return invalid("scope", "must be env, stack, or org")
	}
	return nil
}
// extract: dropped the store lookup of the stack, belongs in leaf/managed —
// the flow passes the stack and org ids in.
```

The pair is all this row sets: `scope_kind` plus a `scope_id` that is the stack
id, the org id, or empty for env scope. The lookup that reads the pair to
decide what an environment can see is not in this row.

```go
// A scope change is not a settings edit:
//   - it never recreates the container — scope governs who may provision from
//     the instance, and the running container knows nothing about it;
//   - it is never exempt from the config-file gate, even for an org-scoped
//     instance, because moving one in or out of org scope makes it appear or
//     vanish from the config snapshot mid-flight;
//   - it is gated and written in one step, because splitting them let the
//     settings persist while the container was never recreated.
```

```go
// The org-scope exception, for settings only. A stack's config file cannot
// declare an org-scoped instance (`shared:` stops at stack scope), so those
// stay panel-owned even on a config-managed stack. The API had no such
// exception, so an org-scoped instance took a settings edit from a browser and
// 409'd the identical CLI edit.
func fileUnowned(t *Instance, stackIsConfigManaged bool) error {
	if t.ScopeKind == "org" {
		return nil
	}
	if stackIsConfigManaged {
		return ErrManagedConflict
	}
	return nil
}
```

## Teardown

```go
// Deleting an instance destroys the data of every consumer holding a slice on
// it, so the default is to refuse and make the caller say it means it. force
// is the caller's, never a constant — the CLI used to force on every path,
// which made this unreachable.
if len(held) > 0 && !force {
	return conflictf("%d consumer(s) hold slices on this instance; "+
		"detach them first, or force the delete to destroy the data with it", len(held))
}

// Order matters: the slices go before the container does, so the engine drop
// can still reach it. Dedupe by slice name — several consumer rows can share
// one slice (attach-to-existing), and dropping it once is enough. A drop that
// fails is logged, not fatal: the rows are reaped either way.
seen := map[string]bool{}
for _, p := range held {
	if seen[p.Name] {
		continue
	}
	seen[p.Name] = true
	dropSlice(t, p.Name) // failures logged, teardown continues
}
// extract: dropped the container remove and the ordinary tile teardown
// (containers, route, schedules, the row), belongs in flow/deploy + leaf/tile.

// Then reap whatever still points at the instance: a slice whose engine drop
// failed (stopped instance, engine with no drop hook) must not outlive the
// thing that provided it. Both tables, not just provisions — the resource row
// is what a consumer's ${{ tile.<slice>.<output> }} resolves through, and one
// left behind names a provider tile id that no longer exists.
// extract: dropped the two delete loops, belongs in leaf/managed.
```

## Rules as today (spec)

- **Three scopes, one column pair.** `env` (scope id empty), `stack` (the
  instance's own stack id), `org` (its stack's org id). Anything else is an
  invalid-`scope` error.
- **The scope id is derived, never supplied.** It comes from the instance's
  stack, so a request cannot point an instance at another tenant.
- **Scope widens who may attach, nothing else.** An env-scoped instance serves
  its own environment; a stack- or org-scoped one is provisionable from the
  wider set. Changing it never touches the container.
- **Same-env only for attaching to an existing slice** (its url secret is
  env-scoped) — that rule and the per-consumer slice cut live in
  `extracts/managedtiles.md`.
- **One slice per consumer,** derived from the consumer's slug; the exception
  is attach-to-existing, where several consumer rows share one slice — which is
  why teardown dedupes by slice name.
- **A consumer going away keeps its slice** (orphaned) and revokes its binding,
  so references stop resolving; the slice is only destroyed by an explicit
  drop, or with its instance.
- **An instance going away refuses while slices are held,** unless forced; then
  drops every distinct slice before the container, and deletes every provision
  and resource row still pointing at it.
- **Settings on an org-scoped instance are exempt from the config-file gate;
  its scope never is.**

## Notes for the builder

- **Target:** `service/internal/leaf/managed` owns the `managed_instances` row
  (engine, admin credentials, endpoint, `scope_kind`/`scope_id`) and the
  provisions table. `service/internal/flow/managed` holds the rules above once,
  engine-blind.
- **Facts as arguments.** The old service reached for the store mid-rule (stack
  lookups, provision listings, resource listings). The flow loads them and
  passes ids and rows in; nothing in these rules needs a store handle.
- **No auth here.** "Who may provision from this instance", "who may delete it"
  and "who may change its scope" are `can()` in middleware, like every other
  verb. The old file had no auth check either — its gates are about the config
  file owning the stack, which is a different question and belongs to
  `flow/config`.
- **No status writes, no enqueue, no notifier.** Deploying the instance
  container and writing its status is `flow/deploy`'s single path.
- **`Bindings()` collision:** in this file it meant the `resource_bindings`
  rows a consumer tile holds; in `extracts/managedtiles.md` it is the
  `ManagedTile` method returning a slice's credentials. Both land in
  `flow/managed`; keep the names apart.
- The old file's reason for existing — four surfaces each doing a partial
  teardown and each leaking something different — is already answered by the
  rewrite's one-flow-per-verb rule. Do not port the service.

Size: source 666 lines, extract 150 lines

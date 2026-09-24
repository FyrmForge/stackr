# tile-crud

Source: `service/tile.go`, `service/tile_test.go`
Commit: c2423f0
Taken: the step order of create / update / delete / rename, the slug rules, the
defaults a create fills, the delete refusals and the teardown order, and what an
update triggers (route rewrite vs redeploy vs cron reload).
Cut: every store read of another table, every Docker/overlay call, every proxy
call, every job enqueue, every scheduler reload, the config gate's staging
branch, the status-column writes, the plain reads and setters.
Cuts belong to: `leaf/tile`'s world object (containers, proxy), the flow (facts
as arguments, enqueues, derived state), `leaf/stagedchange`, authz middleware.

Target: `service/internal/leaf/tile`.

This is the row `tilelifecycle.md` said was missing: that extract holds the
validation gate and the runtime actions only, and explicitly told the builder
that no slug rule, no kind rule, no delete refusal and no "what an update
triggers" lived in its files. They live here.

## Kept code

Create. The order is the content: the name is checked **before** the defaults
are applied, which is what fills `t.ID` — so the uniqueness check below compares
against an empty id on a create and against the real one on a re-check.

```go
func Create(t *Tile, f Facts) error {
	// extract: dropped the config gate + staging branch, belongs in flow/deploy;
	// extract: fact "this stack is config-managed" now passed in
	if err := nameTile(t, f); err != nil { return err }
	applyDefaults(t)
	if err := checkAttach(t, f); err != nil { return err }
	return Validate(t, f) // the gate in tilelifecycle.md, unchanged
}
// extract: dropped CreateTile/cron reload/target redeploy, belongs in flow/deploy
```

The slug. It is the tile's DNS alias inside its environment and its key in the
config file, so it has to be derivable, not reserved, and free **in this
environment** — not in the stack, not in the org.

```go
func nameTile(t *Tile, f Facts) error {
	t.Name = strings.TrimSpace(t.Name)
	if t.Name == "" { return invalid("name", "a tile needs a name") }
	if t.Slug == "" { t.Slug = Slugify(t.Name) }          // caller may supply one
	if t.Slug == "" { return invalid("name", "a name needs at least one letter or number") }
	if ReservedSlug(t.Slug) {
		return invalid("name", "\""+t.Slug+"\" is reserved for variable references; pick another name")
	}
	// extract: fact "the tile already holding this slug in this environment" now passed in
	if f.SlugHolder != nil && f.SlugHolder.ID != t.ID {
		return svcerr.Conflictf("a tile named %q (%s) already exists in this environment",
			f.SlugHolder.Name, t.Slug)
	}
	return nil
}
```

Defaults. Every surface used to fill these differently — the webhook token was a
uuid on one path and 24 random bytes on the other, and the panel gave a 30
minute timeout to services, which have no run to time out.

```go
func applyDefaults(t *Tile) {
	now := time.Now().UTC()
	if t.ID == "" { t.ID = uuid.New().String() }
	if t.WebhookToken == "" { t.WebhookToken = RandomHex(24) }
	// extract: dropped Status = "idle" — no status column, state is derived
	if t.CreatedAt.IsZero() { t.CreatedAt = now }
	t.UpdatedAt = now
	if t.SourceType == "" {                 // an image ref is the only hint there is
		if t.ImageRef != "" { t.SourceType = "image" } else { t.SourceType = "git" }
	}
	if t.SourceType == "git" {
		if t.GitBranch == "" { t.GitBranch = "main" }
		if t.DockerfilePath == "" { t.DockerfilePath = "Dockerfile" }
		if t.BuildContext == "" { t.BuildContext = "." }
	}
	if runToCompletion(t) && t.TimeoutMinutes == 0 { t.TimeoutMinutes = 30 }
}
```

A volume's attach target: four conditions, one message.

```go
func checkAttach(t *Tile, f Facts) error {
	if !t.IsVolume() || t.AttachedTileID == "" { return nil }
	// extract: fact "the tile the attach id resolves to" now passed in
	if f.AttachTarget == nil || f.AttachTarget.EnvironmentID != t.EnvironmentID ||
		f.AttachTarget.Kind != "service" || f.AttachTarget.IsManaged() {
		return invalid("attach_tile_id", "attach a volume to a service in the same environment")
	}
	if !strings.HasPrefix(t.MountPath, "/") {
		return invalid("mount_path", "mount path must be absolute (e.g. /data)")
	}
	return nil
}
```

What an update triggers. `Update` takes a **full row, not a patch**: the three
callers (panel form, API JSON merge, config apply) each map their own request
shape onto a loaded row and hand the whole thing over, and none of them gets to
decide which side effects their write earns. The diff against the stored row is
what picks them — `extra` names changes that are not columns on this row, so a
caller that also moved something else earns the side effects for it.

```go
func afterWrite(t *Tile, changed Changed) []Effect {
	if !changed.Any() { return nil }
	var out []Effect
	if t.Kind == "cron" && changed.NeedsCronReload() { out = append(out, CronReload) }
	switch {
	case t.IsManaged():
		// nothing: a managed instance's container is rebuilt by the applier's path
	case t.Kind == "service":
		out = append(out, RouteRewrite)            // ALWAYS, even when nothing else fires
		if !changed.ProxyOnly() { out = append(out, Redeploy) }
	case runToCompletion(t):
		// schedule, command and timeouts are read off the row at each run
		if changed.NeedsBuild() { out = append(out, Redeploy) }
	}
	return out
}
// extract: dropped the enqueue + proxy call themselves, belongs in flow/deploy
```

Rename — the only path that moves a slug (`UpdateTile` takes a `TileConfig`,
which cannot express identity). Each step is here because skipping it broke
something:

1. tear the containers down **first** — the service name is built from the slug,
   so a rename leaves the old service running under the old name for ever;
2. rename the row;
3. rewrite the route — the route file is keyed on the tile id so the file
   survives, but its contents carry the slug; the config path used to *remove*
   the route and never write it back, which left a renamed tile serving nothing
   until the next domain edit or a full resync.

It deliberately does **not** redeploy: its only caller is a config apply, which
records what it deployed and would be missing a deploy queued from in here.
`// extract: dropped the teardown and the route write, belongs in flow/deploy`

Delete, then teardown. The refusal is checked **before** staging, not after: a
staged delete that can never apply is worse than a refusal, because the refusal
is visible now.

```go
func Delete(t *Tile, f Facts) error {
	if t == nil { return svcerr.ErrNotFound }
	// extract: fact "this stack is config-managed" now passed in — Conflict, tested
	if t.IsVolume() && t.AttachedTileID != "" && f.AttachOwner != nil {
		// Only while the owner is still there: nothing clears a volume's attach
		// id when its tile goes, and refusing on the id alone left such a volume
		// undeletable for ever.
		return svcerr.Invalidf("", "detach the volume from %s before deleting it", f.AttachOwner.Slug)
	}
	return nil
}
```

TearDown is Delete without the gate or the staging branch — the half a config
apply and a staged-delete apply have already decided to perform. Order, with the
reason each step is where it is:

1. hold the volume's former attach target, before the row goes;
2. containers and overlay down (so nothing is still writing);
3. drop the route (so nothing is still reachable);
4. **orphan, not drop**, the shared-db provisions this tile consumed — the data
   belongs to the database, not to the consumer that went away; the API's own
   teardown skipped this and left rows pointing at a tile id that no longer
   resolved;
5. delete the row;
6. reload the schedule tables — the delete cascades this tile's cron and backup
   rows, and without the reload the orphaned entries keep ticking against a tile
   that is gone;
7. redeploy the former attach target — the bind is in the service definition, so
   the mount only goes when the container is recreated (deleting a volume by the
   tile route never did this, only the volume route did);
8. if this tile is **not** a volume, clear `attached_tile_id` and `mount_path` on
   every volume attached to it. The tile row does not cascade to its volumes, so
   the pointer used to outlive its target and the attached-guard above could not
   tell a dead owner from a live one.

Steps 2, 3, 4, 6 and 7 are all `// extract: dropped, belongs in flow/deploy`.
Steps 1, 5 and 8 are the leaf's own table and stay.

## Rules as today (spec)

New refusal rows — none of these are in `tilelifecycle.md`'s table; field names
are request keys, `—` means the refusal names no field. Rows marked **fact** need
a value the leaf cannot read itself.

| Field | Message | Condition |
|---|---|---|
| `name` | a tile needs a name | create only, `Name` blank after trim |
| `name` | a name needs at least one letter or number | slug blank after slugify (create **and** rename) |
| `name` | `"<slug>"` is reserved for variable references; pick another name | `ReservedSlug(slug)` (create **and** rename) |
| — | `Conflict`: a tile named `"<name>"` (`<slug>`) already exists in this environment | **fact** another tile in the same **environment** holds the slug |
| `attach_tile_id` | attach a volume to a service in the same environment | **fact** volume's attach target missing, in another env, not `kind == "service"`, or managed |
| `mount_path` | mount path must be absolute (e.g. /data) | volume with an attach id, `MountPath` not `/`-prefixed |
| — | `Conflict` (the config gate's) | **fact** the tile's stack is config-managed and the write is structural or a field the file owns |
| — | detach the volume from `<owner slug>` before deleting it | **fact** volume with an attach id whose owner tile still exists |
| — | `ErrNotFound` | `t == nil` on create, update, delete or rename |

- **Slug uniqueness scope is the environment.** Not the stack, not the org.
- **Kind rules: there are none here.** `Update` takes a full row and never
  compares the old kind to the new one, so nothing in this file refuses a kind
  change or restricts which kinds may be created. `tilelifecycle.md` pointed the
  builder here for them; the honest answer is that the old code had none.
- **Slug grammar is not defined here either.** `tile.go` calls `repo.Slugify`
  and `repo.ReservedSlug` and defines neither. What it owns is: derive from name
  when the caller supplied no slug, refuse an empty result, refuse a reserved
  one, and the environment-wide uniqueness. The grammar and the reserved list
  are an **open item in `store/repo`** — not something this extract dropped.
- **Create and rename diverge on the empty name.** Create refuses it ("a tile
  needs a name"); rename does not, it only refuses once slugify comes back
  empty. Pin the divergence rather than merging them.
- **Route rewrite is unconditional for a service; redeploy is not.** A settings
  save on a service always rewrites the route, because the route carries basic
  auth, the security headers and the override, and it hashes the password as it
  writes — an API patch that set any of them used to do nothing at all and stored
  the password in clear for nobody to read.
- **`extra`** lets a caller add change keys that are not columns on this row, so
  a config apply's domain edit earns the redeploy it deserves. Empty from both
  HTTP surfaces.
- **Test-pinned, five behaviours:** a delete on a config-managed stack is a
  `Conflict` and nothing is torn down first; the attached-volume refusal fires on
  **both** surfaces and its message must be non-empty; a nil tile is
  `ErrNotFound`; a volume whose owner is gone **is** deletable; a teardown
  orphans exactly the volumes attached to the deleted tile and no others.

## Notes for the builder

- **An update cannot write every column it is handed.** `UpdateTile` takes a
  `TileConfig`, so status, `shared_net`, `home_node`, the digests and the slug
  are not expressible on that path at all — before this was fixed, a full-struct
  write compiled, returned nil and reverted nothing: visibly fine, silently a
  no-op. A rewrite whose update also takes a whole row must either refuse or
  explicitly ignore the columns it cannot write, and say which.
- `[]Effect` / `CronReload` / `RouteRewrite` / `Redeploy` above are this
  extract's spelling for the branch's outcomes, not names the old code had. The
  branch shape is the spec; the return type is the builder's to pick.
- **Four unresolved predicates.** `afterWrite` branches on `DiffTiles` and on
  `Changed.Any()` / `NeedsCronReload()` / `ProxyOnly()` / `NeedsBuild()`. Their
  bodies live in another file that this row was not allowed to read. The branch
  *shape* above is faithful and complete; the field→predicate mapping is an open
  item and must not be guessed.
- **Five cross-table facts become arguments:** the tile already holding the slug
  in this environment, the volume's attach target, the volume's attach owner (on
  delete), whether the tile's stack is config-managed, and the siblings list used
  to orphan volumes. `// extract: fact X now passed in by the flow`
- **`Status = "idle"` on create is dropped**, with the rest of the status-column
  writes. If the rewrite still wants "a tile that has never deployed reads as
  idle", that is a derivation, not a default.
- **`applyDefaults` mutates, like `Validate` does.** Create is
  name → defaults → attach → validate, and two of those four write to the row.
  A pure-validator rewrite has to place both mutating steps explicitly.
- Dropped wholesale, one note for the lot: the plain reads (`Get`, `ListForEnv`,
  `ListForStack`, `ListAll`, `BySlug`), the raw `Save`, the four column setters
  (`SetSharedNet`, `SetHomeNode`, `SetImageDigest`, `SetStatus`) and all six
  staged-change pass-throughs. The staged-change six are a different table and
  want their own leaf; the setters are either derived state or the flow's.

Size: source 579 lines (`service/tile.go`; `tile_test.go` 148 lines read, not counted), extract 266 lines.

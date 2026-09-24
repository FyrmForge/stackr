Source: service/container.go (no test at this commit)
Commit: c2423f0
Taken: the one system-container guard over start / stop / remove / terminal
Cut: the service struct and constructor, the cluster calls that do the work
Cuts belong to: leaf/tile (world-object container methods), flow/tile (the four verbs)

## Rules as today (spec)

One guard, four verbs. It used to be on stop, half of it on remove, and absent
from start and the terminal — so a root shell could be opened inside the panel
container and an old node agent started underneath the running one.

- System container: start, stop and exec always refused. Remove refused **only
  while running** — an already-exited one is a previous generation left by an
  upgrade, and refusing it is what left every node accumulating agent corpses
  no operator could clear. Anything else: all four allowed.
- Who may: **nobody**. A fact about the container, not the caller; a server
  admin is refused too. `// extract: no actor check, belongs in authz middleware`
- Which containers: the panel, proxy and node agent. The world object decides
  which; this rule owns only the wording.
- Fails closed. An unreachable node answers "system", and remove reads an
  inspect error as "running". Refusing costs a retry; allowing costs the panel.
- Messages, verbatim, all 409 Conflict: `<names> cannot be started|stopped|
  opened a terminal into from here`, `<names> cannot be removed while they are
  running`.

## Kept code

```go
// The world object decides which containers these are; this is the wording.
const systemNames = "the panel, proxy and node agent containers"

// refuseSystem is the guard. Start ("started"), Stop ("stopped") and
// EnsureExecAllowed ("opened a terminal into") call it and then their world
// method; the terminal is the guard alone.
func (t *Tile) refuseSystem(ctx context.Context, node, id, verb string) error {
	if t.world.ContainerIsSystem(ctx, node, id) {
		return svcerr.Conflictf("%s cannot be %s from here", systemNames, verb)
	}
	return nil
}

// Remove bends the guard for an already-exited system container.
func (t *Tile) Remove(ctx context.Context, node, id string) error {
	if t.world.ContainerIsSystem(ctx, node, id) {
		d, err := t.world.InspectContainer(ctx, node, id)
		if err != nil || d.State == "running" {
			return svcerr.Conflictf("%s cannot be removed while they are running", systemNames)
		}
	}
	return t.world.StopRemove(ctx, node, id)
}
```

## Notes for the builder

- Docker calls stay inside `leaf/tile`'s world object, the only place allowed;
  callers get the verbs, never the cluster. Nothing here writes a status
  column or enqueues anything.
- One guard called from four places, not four copies — that split was the bug.

Size: source 75 lines, extract 62 lines

# Spec building

Source: `infra/deploy/engine.go` (reference only: `infra/runtime/runtime.go`, `ContainerSpec`)
Commit: c2423f0
Taken: tile row + resolved values → container spec — field mapping, defaults, labels, ports, volumes, limits, healthcheck, restart, the dependency wait's position
Cut: git, build, push, status, supersede, queue, placement, Swarm rollout
Cuts belong to: `flow/deploy` (already extracted in `deploy.md`), `leaf/image`, `flow/jobs`

Target: `service/internal/flow/deploy`. `deploy.md` promised `volumeBinds`,
`publishedPorts`, `parseKV`, `splitLines`, `splitCommand`, `serviceCommand` and
`warnUnsupported` to flow/deploy and did not carry them — this is where they
land. Spec building is a plain struct built in the flow: no store call, no
Docker call, no status write. Every fact arrives as an argument.

The old code builds a Swarm `runtime.ServiceSpec`. The rewrite's runtime takes
`runtime.ContainerSpec` (see `extracts/runtime.md`), so this is a mapping, not a
port. Two shape deltas the table below is keyed to:

- `Devices` is `[]Device` (already parsed), not `[]string`. Parsing is leaf/tile's.
- restart is `RestartAlways bool`, not a `RestartPolicy` string.

## Order of operations

The spec is the last thing built, and the sequence in front of it is
load-bearing. From `pipeline`, after the image exists:

1. **Non-keep-alive stops here.** A cron's deploy ends at the built image — the
   scheduler one-shots it later. Starting a container here would be a second
   copy running off-schedule.
2. Ensure the environment network; take its name and the tile's service name.
3. Effective CPU/memory limits (tile values folded with the environment's caps).
4. Reconcile provisioned dependencies (recreate a dropped db/bucket, republish
   its secret) — **before** resolving values, so a self-healed secret is visible
   to this same deploy.
5. Dependency wait: if a dependency is parked on an unset value, park this tile
   on the same name instead of starting it against a dependency that is not
   there. (`DepWaiting` — the dep-line grammar is in `runtime.md`, the function
   is assigned to flow/deploy by `deploy.md`, and its position in the sequence
   is this line. There is no fourth place to look.)
6. Resolve env values. An unresolved reference is fatal — never run a tile with
   one.
7. Expand volume lines through the same resolver (a `${{ org.MEDIA_ROOT }}` bind
   must never reach Docker literal).
8. Expand and tokenize the command.
9. Materialize `files:` into a per-deploy folder and append its read-only binds
   — **after** the volume expansion pass, because those host paths are literal
   and must not be re-expanded.
10. Resolve storage attachments to volumes with the right driver opts (an
    auto-created bare volume is silently an empty local dir, not the share).
11. Networks, then the spec.

## The one kept function

```go
// Spec is the container this tile should run. Everything it needs is an
// argument: the flow resolved it, this only arranges it.
//
// extract: dropped the arguments' sources, each belongs in flow/deploy —
// name (envnet.ServiceName), image (the build/pull result), netName
// (envnet.Ensure), cpu/mem (settings.ForTile().EffectiveLimits), env
// (varref.Resolve().Lines()), binds (volumeBinds + varref.ExpandStrings +
// file binds + storage binds), deployID (the job row), ports
// (publishedPorts, whose warnings the caller logs), devices (leaf/tile's
// parse of the tile's devices column).
func Spec(tile *repo.Tile, name, image, netName, deployID string,
	env, binds, cmd []string, ports map[string]string, devices []runtime.Device,
	cpuLimit float64, memLimitMB int,
) runtime.ContainerSpec {
	return runtime.ContainerSpec{
		Name:        name,
		Image:       image,
		Cmd:         cmd,
		Env:         env,
		Volumes:     binds,
		Ports:       ports,
		NetworkName: netName,
		Aliases:     []string{tile.Slug, TileAlias(tile.ID)},
		CPULimit:    cpuLimit,
		MemLimitMB:  memLimitMB,

		User:       tile.User,
		ShmSizeMB:  tile.ShmSizeMB,
		Privileged: tile.Privileged,
		Devices:    devices,

		RestartAlways: tile.RestartPolicy == "always",

		HealthCmd:          tile.HealthcheckCmd,
		HealthIntervalS:    tile.HealthcheckIntervalS,
		HealthTimeoutS:     tile.HealthcheckTimeoutS,
		HealthRetries:      tile.HealthcheckRetries,
		HealthStartPeriodS: tile.HealthcheckStartPeriodS,

		Labels: map[string]string{
			runtime.LabelTile:   tile.ID, // DECIDE: key name, see the note below
			runtime.LabelDeploy: deployID,
		},
	}
}

// extract: dropped Replicas + Pinned/HomeNode/NodeGroup (placement.For, plus
// the "a tile holding a volume cannot run >1 replica" refusal) and
// RegistryAuth — Swarm only, and the rewrite has no Swarm. The one-mounter-
// per-volume rule is worth keeping wherever replicas come back.
// extract: dropped warnUnsupported: it existed because Swarm's task spec has
// no --device and no --privileged. Containers have both, so the two fields map
// straight from the row. Privileged stays a gated field — the caller decides
// who may set it — not a silently honoured column.
```

## Field → spec mapping

`ContainerSpec` field (per `extracts/runtime.md`) ← source, and what happens on the way.

| Spec field | Source | Mapping / default |
|---|---|---|
| `Name` | env + stack + tile slug | Built by envnet, passed in. Stable across deploys — the roll replaces the same name. |
| `Image` | the pipeline's result | Rollback/promote: the recorded tag. `image:` tile: the tile's ref, erroring if empty. Git: the pushed reference, never the local build tag. |
| `Cmd` | `tile.Command` | `""` → `nil` (image default). Otherwise value-expanded, then tokenized like a shell word-splits — **no `sh -c`**: Docker appends `Cmd` to the entrypoint and images like loki/prometheus take flag lists on their own binary. |
| `Env` | resolved values | `KEY=VALUE` lines from the resolver. An unresolved reference fails the deploy before the spec is built. |
| `Labels` | tile id, deploy id | Old keys: `stackr.app=<tile id>`, `stackr.deployment=<deploy id>`. The wrapper adds `stackr.managed=true` itself. Nothing else goes in — the tile-id label is what retire, prune-by-label and the metrics reconciler key on, and image pruning uses the same label on images. |
| `Volumes` | attached volume tiles, then `tile.Volumes`, then `files:`, then `storage:` | In that order. Volume tiles: `<docker volume name>:<mount path>`, **skipped when the mount path is empty** (attached but not yet placed; `name:` is a malformed bind). Free-text lines are trimmed, `#` comments dropped, then value-expanded. File binds are appended `:ro` and are already literal. Storage lines resolve to local-driver volumes with opts. |
| `Ports` | `tile.PublishedPorts` | `host:container[/udp]` per line → `{host: container}`. Bad lines warned and skipped, never fatal. Empty → `nil`. There is no separate expose column anywhere: the wrapper derives the exposed set from this map inside `portBindings`, so a container port that is not published is not exposed either — it is reachable on the environment network regardless. |
| `Aliases` | `tile.Slug`, tile id | Two DNS names on the environment network: the slug and a per-tile alias. The proxy resolves tiles by alias, so these are in the spec, not attached afterwards. |
| `NetworkName` | the environment's network | Ensured before the spec is built. The wrapper defaults an empty value to the shared `stkr` network; the deploy path always sets it. |
| `CPULimit` / `MemLimitMB` | `tile.CPULimit`, `tile.MemLimitMB` folded with the environment's settings | The tile asks, the env caps — one `EffectiveLimits` call decides, and the *result* is the argument. `0` = unlimited on both. |
| `User` | `tile.User` | Straight through, `""` = image default. |
| `ShmSizeMB` | `tile.ShmSizeMB` | Straight through, `0` = Docker's 64 MB. |
| `Privileged` | `tile.Privileged` | Straight through now that there is no Swarm. Gate who may set the column. |
| `Devices` | `tile.Devices` | Lines parsed by leaf/tile into `Device{Host, Container, Perms}`; container path defaults to the host path, perms to `rwm`, both must be absolute. |
| `RestartAlways` | `tile.RestartPolicy` | `"always"` → true, everything else → false (the wrapper's default is `unless-stopped`). See the restart note below — the old string's `on-failure`/`no` have no bool to land on. |
| `HealthCmd` and the four knobs | `tile.Healthcheck*` | Verbatim, five fields, no defaulting here. Empty `HealthCmd` = no healthcheck; each knob at `0` = Docker's default. `HealthStartPeriodS` is read a second time by the health gate, which stretches its deadline by it — a slow-boot tile that declares a start period must not be cut down by the fixed timeout. |

## Kept helpers

```go
// publishedPorts parses "host:container[/udp]" lines into the port map. A bad
// line is skipped, not fatal — one typo in a text column must not block a
// deploy.
//
// extract: dropped the io.Writer it warned into. Spec building has no log sink;
// return the warnings and let flow/deploy write them to the job log.
func publishedPorts(s string) (map[string]string, []string) {
	lines := splitLines(s)
	if len(lines) == 0 {
		return nil, nil
	}
	out, warn := map[string]string{}, []string(nil)
	for _, l := range lines {
		host, cont, ok := strings.Cut(l, ":")
		if !ok || host == "" || cont == "" {
			warn = append(warn, fmt.Sprintf("skipping bad published port %q (want host:container[/udp])", l))
			continue
		}
		out[host] = cont
	}
	return out, warn
}

// splitLines is the grammar of every multi-line tile column: trim, drop blanks
// and "#" comments.
func splitLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(l)
		if l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out
}

// splitCommand tokenizes a command string into argv: whitespace separates,
// single/double quotes group, backslash escapes the next rune (outside single
// quotes). No expansion of any kind — Docker gets the words verbatim.
func splitCommand(s string) ([]string, error) {
	var argv []string
	var cur strings.Builder
	inWord := false
	quote := rune(0) // active quote char, 0 = none
	escaped := false
	for _, r := range s {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case r == '\\' && quote != '\'':
			escaped = true
			inWord = true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
			inWord = true
		case r == ' ' || r == '\t' || r == '\n':
			if inWord {
				argv = append(argv, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if escaped || quote != 0 {
		return nil, fmt.Errorf("unterminated quote or escape in %q", s)
	}
	if inWord {
		argv = append(argv, cur.String())
	}
	return argv, nil
}

// extract: dropped volumeBinds' store read (list the env's tiles, take the
// volume tiles attached to this one), belongs in flow/deploy. Rule: attached
// volume tiles first as "<docker volume>:<mount path>", skipping any with no
// mount path, then splitLines(tile.Volumes).
// extract: dropped parseKV (splitLines, then Cut on "=") — build args, not
// spec, belongs in leaf/image.
```

## Notes for the builder

- **Expand once, in the right order.** Values, volume lines and the command all
  go through the resolver; the materialized `files:` host paths and the resolved
  storage volumes do not. Appending file binds before the expansion pass
  re-expands a literal host path — that is the bug this ordering exists to
  prevent.
- **Networks have one slot.** `ContainerSpec` carries one `NetworkName` plus
  `Aliases`, and the old spec carried a list: the environment overlay *with*
  aliases, plus every shared-instance network the resolver asked for, *without*
  one. The rewrite has to decide: extra networks joined after the run (the
  pre-Swarm behaviour), or `Networks []NetAttach` on the spec. Do not lose them
  silently — a tile that talks to a shared managed instance needs the second
  network to resolve its host.
- **Aliases belong in the spec, not after it.** Joining a network after start is
  a window where the proxy cannot resolve the tile.
- **`/udp` survives by accident.** `publishedPorts` cuts on the first `":"`
  only, so `8080:53/udp` yields container value `"53/udp"` and the wrapper's
  port parser sees the proto. A reimplementation that splits on `/` first, or
  validates the container value as an integer, silently drops UDP.
- **The healthcheck is Docker's, not ours.** `HealthCmd` runs as `CMD-SHELL`.
  The old code had a hand-rolled health gate and replaced it with the daemon's;
  keep the start period feeding the deploy's own wait deadline, that is the only
  part the gate still owns.
- **DECIDE: the tile-id label key.** The old key is `stackr.app`, and it is the
  join key for retiring old containers, pruning images by label, and reading
  metrics — three consumers, one spelling. Nothing is deployed anywhere, so no
  wire compatibility argues for keeping it: `stackr.tile` is the rewrite's
  vocabulary. Pick one and set it in exactly one place; whichever it is, the
  spec must keep setting it or those three break quietly.
- **DECIDE: restart under a single bool.** The old column held `""`,
  `always`, `unless-stopped`, `on-failure` and `no`. `RestartAlways` collapses
  that: a tile that asked for `no` (a one-shot that must stay dead) now runs
  under `unless-stopped` and comes back on a daemon restart. Either narrow the
  column to the two values the bool can express, or widen the spec field back to
  a policy string — silently promoting `no` to `unless-stopped` is a behaviour
  change nobody asked for.
- **Cron stops at the image.** The keep-alive check comes before any of this. A
  tile whose kind does not keep alive never builds a spec here.

Size: source 1437 lines, extract 267 lines.

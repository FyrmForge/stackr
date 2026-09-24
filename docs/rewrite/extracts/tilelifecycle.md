# tilelifecycle

Source: `service/tilelifecycle.go`, `service/tilevalidate.go` (+ their `_test.go`)
Commit: c2423f0
Taken: the refusal vocabulary of a tile write (field → message → condition), the
validation step order, the three in-place normalisations, the runtime actions
(stop / restart / toggle / run-now / stop-run) reduced to their rules.
Cut: every store read of another table, every Docker call, every job enqueue,
every auth check, every status-column write, the `cron_runs` pass-throughs,
the `Actor` value type.
Cuts belong to: `leaf/tile`'s world object (Docker), the flow (facts as
arguments, enqueues, derived state), `leaf/cronrun` (`Actor`, run rows), authz
middleware.

Target: `service/internal/leaf/tile`.

## Kept code

The refusal shape. Every refusal names the request key the caller can fix,
because "400 bad request" on a form with forty fields is not an answer.

```go
func invalid(field, msg string) error { return svcerr.Invalid{Field: field, Msg: msg} }
```

The tile-column list convention: one item per line, blanks dropped. Used by
`files`, `devices`, `depends_on`, `storage`.

```go
func splitLines(s string) []string {
	out := []string{}
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}
```

The kind predicate the runtime actions all branch on: kinds that finish rather
than serve. They have runs, not replicas, which is why stop, restart and run
mean different things for them.

```go
func runToCompletion(t *Tile) bool { return t.Kind == "cron" || t.Kind == "function" }
```

Validation is one gate over a finished row, not over a patch: a rule like "a
git source needs a git_url" is about the tile the write leaves behind, not
about which keys the request happened to carry. Order matters only in that the
first refusal wins.

```go
func Validate(t *Tile, f Facts) error {
	pol, runnable := runpolicy.For(t.Kind)
	// extract: facts connector, stack org, storage shares and pinned now passed in by the flow
	if err := validateSource(t, f, pol, runnable); err != nil { return err }
	if err := validateVolume(t); err != nil { return err }
	if err := validateLimits(t); err != nil { return err }
	if err := validateLists(t); err != nil { return err }   // also normalises, see below
	if err := validateStorage(t, f); err != nil { return err }
	if err := validateRunKind(t, pol, runnable); err != nil { return err }
	return validatePlacement(t, f)
}
```

Three writes Validate makes to the row it is handed. These are spec, not
refusals — a caller that treats Validate as read-only loses them.

```go
// canonical form of the enum's default; asserted by TestValidateAcceptsAPlainService
if t.UpdatePolicy == "" { t.UpdatePolicy = "off" }

// restart policy is normalised in place by the same call that validates it
rp, err := runtime.NormalizeRestart(t.RestartPolicy)
if err != nil { return invalid("restart", err.Error()) }
t.RestartPolicy = rp

// basic auth is a pair or nothing: a user with no password is an open door
// wearing a lock, a password with no user is dead weight
if t.BasicAuthUser == "" { t.BasicAuthPassword = "" }
```

## Field → refusal message → condition

`ErrNotFound` for a nil tile precedes every row below. Rows marked **fact** need
a value the leaf cannot read itself.

| Field | Message | Condition |
|---|---|---|
| `image` | an image source needs an image (set git_url to switch to a git build) | runnable kind, `SourceType == "image"`, `ImageRef == ""` |
| `git_url` | a git source needs a git_url | runnable kind, `SourceType == "git"`, `GitURL == ""` |
| `git_url` | use a GitHub URL: https://github.com/owner/repo or git@github.com:owner/repo | `!ValidGitURL(GitURL)` |
| `connector` | no such connector | **fact** `ConnectorID != ""` and no connector row with that id |
| `connector` | that connector belongs to another organization | **fact** connector's org ≠ the tile's stack's org |
| `update_policy` | update_policy must be off, notify or auto | value not one of `off` / `notify` / `auto` |
| `update_policy` | update_policy watches an image source | `UpdatePolicy != "off"` and `SourceType != "image"` |
| `wait_for_ci` | wait_for_ci needs a git-built source | `WaitForCI` and `SourceType != "git"` |
| `port` | a `<kind>` has no endpoint; port does not apply | `!pol.AllowsIngress` and `ContainerPort != 0` |
| `volume_name` | letters, digits, _ . - only | volume tile, `VolumeName != ""`, fails `ValidVolumeName` |
| `max_size_mb` | must not be negative | volume tile, `MaxSizeMB < 0` |
| `limits.cpu` | cpu limit must not be negative | `CPULimit < 0` |
| `limits.memory_mb` | memory limit must not be negative | `MemLimitMB < 0` |
| `shm_size_mb` | shm_size_mb must not be negative | `ShmSizeMB < 0` |
| `timeout_minutes` | timeout_minutes must not be negative | `TimeoutMinutes < 0` — negative only, no upper bound |
| `healthcheck` | healthcheck knobs must not be negative | any of interval / timeout / retries / start-period `< 0` |
| `replicas` | replicas: a whole number, at least 1 | `Replicas < 0` |
| `files` | (the parser's own error) | any line fails `ParseFileMount` |
| `devices` | (the parser's own error) | any line fails `ParseDevice` |
| `depends_on` | (the parser's own error) | any line fails `ParseDep` |
| `restart` | (the parser's own error) | `NormalizeRestart` refuses the value |
| `basic_auth_password` | basic auth needs a password as well as a user | `BasicAuthUser != ""`, `BasicAuthPassword == ""` |
| `storage` | a volume tile has nothing to mount a share into | `Storage != ""` on a volume tile |
| `storage` | (the parser's own error) | any line fails `ParseAttachment` |
| `storage` | org share `<name>` not found | **fact** line is an org ref, no share with that slug in the stack's org |
| `storage` | storage `<slug>` not found | **fact** no share with that slug |
| `storage` | (the attach rule's own error) | **fact** the resolved share fails `ValidateAttach` for this tile |
| — | `ErrNotFound` | **fact** the tile's stack is missing while resolving an org share |
| `schedule` | (the cron parser's own error) | `pol.RequiresSchedule` and `Cron` does not parse |
| `schedule` | schedule applies to cron tiles only | `!pol.RequiresSchedule` and `Cron` is non-blank |
| `command` | command does not apply to a `<kind>` | `!pol.AllowsCommand` and `Command` is non-blank |
| `run_on_deploy` | run_on_deploy applies to function tiles only | `RunOnDeploy` and `Kind != "function"` |
| `user` | user applies to service tiles only | runnable, `Kind != "service"`, `User != ""` |
| `privileged` | privileged applies to service tiles only | runnable, `Kind != "service"`, `Privileged` |
| `devices` | devices apply to service tiles only | runnable, `Kind != "service"`, `Devices` non-blank |
| `healthcheck` | healthcheck applies to service tiles only | runnable, `Kind != "service"`, `HealthcheckCmd != ""` |
| `replicas` | this tile holds a volume, so it can only run one replica: two writers on one volume corrupt it | **fact** `Replicas > 1` and the tile is pinned (holds a volume) |
| — | a volume has nothing to stop; detach it from its tile instead | `Stop` on a volume tile |
| — | a `<kind>` has no long-running container to restart; use run instead | `Restart` on a cron or function |
| — | nothing deployed to restart | `Restart` and the tile has no deployed service name |
| — | only cron tiles have a schedule to pause | `ToggleCron` on anything but `kind == "cron"` |
| — | run applies to cron and function tiles | `RunNow` on a kind that is not run-to-completion |
| — | `ErrNotFound` | `StopRun` with a run id that is missing **or** belongs to another tile — ownership is checked before the runner is looked at, so "is this mine" never depends on how the process happens to be configured |

## Create/update/delete order as today

These two files hold only the **gate** that both create and update pass
through, and it is one gate for both: `validateSource → validateVolume →
validateLimits → validateLists → validateStorage → validateRunKind →
validatePlacement`, first refusal wins, applied to the finished row.

It is deliberately the *union* of the rules the panel and the API used to apply
separately, not their intersection: where one surface silently coerced a bad
value to a default and the other refused it, the refusal wins — a coerced value
is a write the user did not ask for.

The create/update/delete **sequencing** itself is not in these files: no slug
rule, no kind-change rule, no delete refusal, no "what an update triggers"
appears in either. Those live in `service/tile.go` and need their own row. Do
not reconstruct them from this extract.

The runtime actions, as rules only (the Docker half is cut):

- **Stop** — refuse on a volume; a cron or function parks (it does not stop);
  everything else scales its service to zero. `// extract: ScaleService and the
  managed-engine Stop go to the leaf's world-object methods`
- **Restart** — refuse on a cron or function; refuse when nothing is deployed;
  otherwise force a service update in place, *not* scale 0 then 1, which would
  drop the replica count a stopped tile is meant to keep. A managed instance
  starts rather than restarts, so it comes back at one replica and then
  reconciles the slices a consumer provisioned while it was down.
  `// extract: RestartService / managed Start go to the leaf's world-object methods`
- **ToggleCron** — cron only; flips paused ↔ idle and re-registers the cron
  table. `// extract: dropped enqueue, belongs in flow/deploy` for the
  re-registration.
- **RunNow** — cron and function only; the run row exists before the call
  returns, so a caller that re-renders straight away draws the run that is
  already going rather than an idle badge. `// extract: dropped enqueue,
  belongs in flow/deploy`
- **StopRun** — ownership first, then stop; a run that had already finished
  reports false, which is not a failure. `// extract: dropped enqueue, belongs
  in flow/deploy`

## Notes for the builder

- **`timeout_minutes` has no upper bound on purpose.** One surface used to clamp
  silently to 1440 and that clamp was deliberately *not* promoted to a refusal:
  no other surface had the bound, and a config apply does not run this
  validator, so enforcing it here would refuse a file that applies today.
  `TestValidateDoesNotCapTheTimeout` exists to pin this. Do not add a cap.
- **Six cross-table facts become arguments**, marked **fact** in the table:
  the connector row, the connector's org vs. the tile's stack's org, an org
  share by slug, a share by slug, the resolved share (for the attach rule), and
  whether the tile is pinned. `// extract: fact X now passed in by the flow`
- **DECIDE (darhvader):** `ToggleCron` both *reads* and *writes* a status column
  (`paused` → `idle`, anything else → `paused`). With no status column and state
  derived, pause has nowhere to live as written — it is a stored intent, not an
  observation, so it needs its own column or its own table. Do not translate it;
  raise it.
- Every refusal names a field, and the field names are the request keys, not the
  column names (`limits.cpu`, not `cpu_limit`). Two of them differ from their
  column (`limits.cpu`, `limits.memory_mb`) and two share a field across very
  different conditions (`healthcheck`, `storage`, `devices`, `replicas`). Keep
  the strings verbatim; the API's callers match on them.
- `Validate` mutates the row it checks (three normalisations above). If the
  rewrite wants a pure validator, the normalisation has to move somewhere
  explicit before the gate — it cannot just be dropped.
- Dropped wholesale, one note for the lot: the `cron_runs` pass-throughs
  (`StartRun`, `FinishRun`, `PruneRuns`, `RecordTileRun`, `Runs`, `OpenRun`,
  `OpenRuns`) and the `Actor` value type with its `String()` / `Audit()`
  spellings. Another table, another leaf.
- Dropped: every `UpdateTileStatus` write and its canvas nudge (no status
  column; state is derived), and the "no job runner configured" unavailable
  answer (the runner is the flow's, not the leaf's).

Size: source 623 lines (`tilelifecycle.go` 328 + `tilevalidate.go` 295; tests read, not counted), extract 207 lines.

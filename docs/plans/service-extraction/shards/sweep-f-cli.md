# Sweep F — CLI

The CLI is an HTTP client: it cannot import `repo.Store` or `service/`, so
"business logic outside service/" reads differently here. What this shard
records is **rules the CLI re-implements client-side that the server already
owns** (drift risk), and **rules only the CLI enforces** (bypassable by any
other API client — marked **bug**).

Files read: every non-generated, non-test `.go` file under `internal/cli` and
`internal/cli/cmd` (26 files, 8484 lines).

| File | Lines |
|------|-------|
| internal/cli/client.go | 1135 |
| internal/cli/lifecycle.go | 494 |
| internal/cli/login.go | 169 |
| internal/cli/link.go | 83 |
| internal/cli/config.go | 80 |
| internal/cli/cmd/tile.go | 737 |
| internal/cli/cmd/lifecycle.go | 717 |
| internal/cli/cmd/backup.go | 542 |
| internal/cli/cmd/infra.go | 463 |
| internal/cli/cmd/registry.go | 417 |
| internal/cli/cmd/promote.go | 362 |
| internal/cli/cmd/deploy.go | 361 |
| internal/cli/cmd/plan.go | 345 |
| internal/cli/cmd/vars.go | 290 |
| internal/cli/cmd/members.go | 284 |
| internal/cli/cmd/stack.go | 252 |
| internal/cli/cmd/auth.go | 253 |
| internal/cli/cmd/runtime.go | 247 |
| internal/cli/cmd/defaults.go | 220 |
| internal/cli/cmd/storage.go | 188 |
| internal/cli/cmd/org.go | 187 |
| internal/cli/cmd/proxy.go | 187 |
| internal/cli/cmd/helpers.go | 161 |
| internal/cli/cmd/domain.go | 154 |
| internal/cli/cmd/root.go | 82 |
| internal/cli/cmd/shim.go | 74 |

## Findings

### internal/cli/cmd/lifecycle.go:240, :274 — `-y` is silently promoted to `force`
- **Rule:** deleting/resetting an environment that still has running tiles
  requires an explicit `force`. The CLI sends `force || rt.Yes`, so any
  scripted `stackr env rm -y` / `env reset -y` always sends `force: true`.
- **Category:** deciding an operation is allowed before asking the server /
  duplication across surfaces.
- **Server-side counterpart:** `internal/stackrd/handlers/api/v1/envs.go:53-79`
  (`in.Force` → `envs.Delete/Reset`); the refusal itself is in `EnvService`,
  and the file header at `envs.go:7-9` states force is "the API's version of
  the panel's type-the-slug confirmation".
- **Drifted?:** yes, and against the CLI's own precedent. `internal/cli/cmd/infra.go:233-256`
  fixes exactly this shape for `infra rm` (the `saw` flag, with a long comment
  explaining that `-y` means "do not prompt", not "destroy"), while these two
  verbs still conflate them. Same rule, two opposite spellings in one package.
- **Severity:** high — the server's "environment has running tiles" refusal is
  unreachable from the CLI, which is the bug `infra rm` was already fixed for.

### internal/cli/cmd/tile.go:126-129 — "schedule/command/timeout apply to cron only"
- **Rule:** a plain service tile may not carry a schedule/command/timeout. The
  CLI silently blanks `in.Schedule, in.Command, in.TimeoutMinutes` when no
  `--cron`/`--function` was passed.
- **Category:** validation of domain shape (branching on tile kind).
- **Server-side counterpart:** `internal/stackrd/service/tilevalidate.go:253-268`
  (`validateRunKind`) — `invalid("schedule", "schedule applies to cron tiles only")`,
  same for `command`.
- **Drifted?:** yes, in resolution. The server **refuses**; the CLI **silently
  discards**. `stackr tile create web --schedule "0 3 * * *"` creates a
  schedule-less service and reports success.
- **Severity:** medium.

### internal/cli/cmd/tile.go:644-649 — volume name defaulted from the mount path
- **Rule:** a volume with no `--name` is named after the last segment of
  `--mount` (`/data` → `data`), falling back to `"data"`.
- **Category:** defaulting a field.
- **Server-side counterpart:** **NONE — client-only.** `handlers/api/v1/volumes.go:50-55`
  passes `in.Name` straight into `TileService.Create`, which refuses an
  unnameable tile (`service/tile.go:~430`, `Slugify` → "a name needs at least
  one letter or number").
- **Drifted?:** n/a (no server rule) — but the panel and any other API client
  must supply a name where the CLI invents one, so the same request shape
  behaves differently per surface.
- **Severity:** medium (**bug**-adjacent: a default that exists on exactly one
  surface).

### internal/cli/cmd/lifecycle.go:191-195 — `--range` enum for metrics
- **Rule:** the CLI refuses any window other than `1h|6h|24h` before calling.
- **Category:** validation of domain shape.
- **Server-side counterpart:** `internal/stackrd/service/tiletelemetry.go:92-100`
  (`MetricRange`) — it recognises `6h`/`24h` and **silently falls back to 1h**
  for everything else; `handlers/api/v1/lifecycle.go:100-103` notes this is the
  one shared range parser.
- **Drifted?:** yes in kind: the server has no rejection at all, so this enum is
  enforced only client-side, and adding a window server-side would leave the
  CLI refusing it.
- **Severity:** low (read-only window; no state at risk) but **client-only**.

### internal/cli/cmd/tile.go:247-263 + :309-314 — coupled-pair payload shaping
- **Rule:** `--dockerfile`/`--build-context` and `--cpu`/`--memory` are treated
  as coupled: passing one sends both, and `orZero` turns an unset half into
  `0`/`""`, which clears it.
- **Category:** defaulting fields / deciding the shape of a domain update.
- **Server-side counterpart:** **NONE — client-only.** The PATCH body
  (`handlers/api/v1/apps.go:~260`, `build`/`limits` nested objects) has no
  notion of the pairing; a caller sending only `limits.memory_mb` keeps its cpu.
- **Drifted?:** n/a — but `stackr tile set --memory 512` silently zeroes the cpu
  limit, and no other client does that.
- **Severity:** medium (**bug**: a client-only rule with a destructive side
  effect on a field the user did not name).
  *Corrected 2026-09-21 (`12-business-logic-plan.md` §4, B21):* `cpu: 0` means
  "inherit the cascade" (`config/settings/settings.go:151-159`), not zero CPU —
  so it wipes the tile's own cpu override rather than starving it.

### internal/cli/client.go:365-393 — `SetVarsAt` read-modify-write merge
- **Rule:** "set variables" is a merge: read the whole set, overlay the named
  rows by name (new wins), sort, PUT the whole set back.
- **Category:** orchestration + precedence the server also has an opinion about.
- **Server-side counterpart:** `handlers/api/v1/variables.go:100-120` (`putVars`
  and its env/stack/org twins) — the endpoint **replaces** the set; there is no
  merge endpoint.
- **Drifted?:** no conflicting rule today, but "set = merge" exists only here,
  and the read-then-write is unguarded: two concurrent CLI writers (or a CLI
  and the panel) lose one another's rows with no conflict reported.
- **Severity:** medium.

### internal/cli/lifecycle.go:76-81 and internal/cli/cmd/vars.go:174-189 — the `${{ … }}` grammar, twice
- **Rule:** how a reference expression is spelled. `RefSource.Expr` builds
  `${{ scope[.slug].name }}` from the catalogue; `varRefExpr` independently
  builds `${{ level.(vars|secrets).NAME }}` and folds env scope into `stack`.
- **Category:** duplication across surfaces (and inside the CLI itself).
- **Server-side counterpart:** `internal/stackrd/config/varref` owns the
  grammar; the catalogue route already returns everything needed to render an
  expression but does not return the expression.
- **Drifted?:** the two CLI spellings already disagree on what they cover
  (`Expr` never emits a `vars`/`secrets` bucket, `varRefExpr` always does, and
  it maps an env-scoped row onto the `stack.` prefix). Neither is checked
  against `varref`.
- **Severity:** medium — a syntax change in `varref` breaks both silently.

### internal/cli/cmd/helpers.go:77-111, :116-141 and internal/cli/cmd/backup.go:16-35 — three name→id resolvers
- **Rule:** "a bare name only matches within the linked (or `--stack`) stack; a
  full id matches anywhere", plus a fallback order across `/apps` and `/dbs`.
  Written three times with three different fallback orders (`resolveTile`
  apps-then-dbs; `resolveTileID` scoped-then-unscoped-for-ids-only;
  `backupTarget` dbs-then-`resolveTileID`).
- **Category:** duplication; re-implementing resolution the server owns.
- **Server-side counterpart:** `handlers/api/v1/resolve.go:23-52` (`POST /resolve`,
  `managedtiles.ResolveTarget`) is the server's addressing rule, and the CLI
  already uses it for infra paths — but not for tiles.
- **Drifted?:** yes internally: the three helpers disagree on whether an
  unscoped bare *name* may match, so the same argument resolves differently
  depending on which verb you typed.
- **Severity:** medium.

### internal/cli/cmd/promote.go:202-217 vs internal/cli/cmd/vars.go:31-46 — env slug→id, twice
- **Rule:** resolve an environment by slug (or id) within the linked stack.
- **Category:** duplication inside the CLI.
- **Server-side counterpart:** `handlers/api/v1/resolve.go:180-195`
  (`requireEnvAccess` accepts `org:stack:env`), and several routes accept a
  slug path directly.
- **Drifted?:** no (the two bodies are identical today) — two copies of one
  lookup, one of them reachable from `defaults.go:115`.
- **Severity:** low.

### internal/cli/cmd/plan.go:38-58 — `detailedExitErr` re-derives "is this plan empty"
- **Rule:** work = changes whose `Scope` is empty; a plan with only
  declared-but-unset rows is not work; gen-secrets count as work.
- **Category:** branching on domain state / duplication.
- **Server-side counterpart:** `config/stackconf/plan.go:287-289` (`Plan.Empty()`)
  and `:294-297` (`Change.Declared()`), wire-encoded at
  `handlers/api/v1/config.go:236-238` (a declared row gets `Env` cleared and
  `Scope` set).
- **Drifted?:** no — the CLI's `Scope == ""` test is an exact mirror of
  `Declared()` as encoded. But it is the third place the emptiness rule lives,
  and it relies on an encoding detail rather than a published flag.
- **Severity:** low.

### internal/cli/cmd/plan.go:71-103 — `readBundle` include rule and size cap
- **Rule:** an `include:` entry must be a relative path that stays inside the
  main file's directory; the whole bundle must be under 1 MB.
- **Category:** validation of domain shape (path rule).
- **Server-side counterpart:** the containment rule has **NONE — client-only**
  (the server resolves includes out of the posted `files` map and never touches
  a filesystem, `config.go:146-150`). The size cap exists at
  `config.go:136-140` but is **2 MB**, and the comment there deliberately names
  the CLI's 1 MB and explains the difference (JSON escaping).
- **Drifted?:** the caps differ on purpose and are documented on both sides. The
  containment check guards the *local* filesystem, not server state, so it is
  not a bypassable server rule.
- **Severity:** low / informational.

### internal/cli/cmd/deploy.go:355-361 — `parsePort` range check
- **Rule:** a port must be 1–65535.
- **Category:** validation of domain shape.
- **Server-side counterpart:** `handlers/api/v1/forward.go:~forwardPort`
  (`n < 1 || n > 65535` → 400 "invalid port"), identical.
- **Drifted?:** no — byte-for-byte the same rule, spelled twice.
- **Severity:** low.

### internal/cli/cmd/domain.go:66-74 — domain-resource level default
- **Rule:** an unset `--level` means `instance`; a stack-level resource with no
  `--owner` defaults to the linked stack.
- **Category:** defaulting a field.
- **Server-side counterpart:** `handlers/api/v1/domainresources.go:99-104`
  defaults `""`/`"node"` → `"instance"` (identical). The owner fallback has
  **NONE — client-only**: `resolveResourceTenancy` returns 400
  "owner (stack id) required for stack level".
- **Drifted?:** no. The level default is duplicated but agrees; the owner
  fallback is a CLI convenience that cannot mis-authorize (the server still
  checks write access on whatever owner arrives).
- **Severity:** low.

### internal/cli/cmd/infra.go:233-256 — the `saw` flag decides what `force` means
- **Rule:** the server's destructive `force` is sent only when the blast radius
  was actually shown and agreed to (`--force`, or a prompt the user answered);
  plain `-y` does not carry it.
- **Category:** deciding whether an operation is allowed before asking.
- **Server-side counterpart:** `service/managedinstance.go:343-352` (`TearDown`
  refuses while slices are held unless `force`), reached from
  `handlers/api/v1/databases.go:281-282`.
- **Drifted?:** no — this is the *correct* handling, and the comment records why.
  Listed because it is the CLI deciding an authorization-shaped input, and
  because it is the counter-example `cmd/lifecycle.go:240` should follow.
- **Severity:** informational.

### internal/cli/cmd/registry.go:127-133 — `<image>:<tag>` ref grammar
- **Rule:** split an image ref on the **last** colon (the name may hold slashes
  and a registry port).
- **Category:** computing an image ref the server also reasons about.
- **Server-side counterpart:** **NONE — client-only.** The route takes name and
  tag as separate escaped path segments
  (`internal/cli/lifecycle.go:282-285` → `/orgs/:org/registry/images/:name/tags/:tag`),
  so nothing server-side parses a combined ref on this path.
- **Drifted?:** n/a.
- **Severity:** low.

### internal/cli/cmd/members.go:92, :251 — `--role` defaults to `member`
- **Rule:** an unnamed role is `member`.
- **Category:** defaulting a field the server also defaults.
- **Server-side counterpart:** `handlers/api/v1/members.go:176-180` (`validRole`
  falls back to `member`) — but only on the invite path; the member-add path at
  `members.go:54-58` deliberately stopped coercing and now lets
  `MembersService.Invite` refuse a bad role.
- **Drifted?:** partially — because the CLI always sends a role, the server's
  own default never runs for CLI callers, and the CLI's default masks the
  stricter of the two server behaviours.
- **Severity:** low.

### internal/cli/cmd/promote.go:131-196 — `pickRelease` (head / latest / short sha)
- **Rule:** resolve `head`, `latest`, a short sha prefix, or `--from <env>` to a
  full commit; an ambiguous prefix is an error; an unmatched ref is passed
  through unresolved.
- **Category:** computing something from domain state.
- **Server-side counterpart:** `service/release.go:74-76` — the doc comment
  explicitly assigns this to the CLI ("Resolving 'head', 'latest' or a short
  sha is the CLI's job"), and `release.go:105` (`built()`) is the safety net for
  the pass-through case.
- **Drifted?:** no — sanctioned client-side logic with a server-side backstop.
- **Severity:** none (recorded so a later sweep does not re-flag it).

## Clean files

No business logic, or logic already owned by the right side:

- `internal/cli/config.go` — credential file IO only.
- `internal/cli/link.go` — link file IO plus a legacy `project`→`stack` key
  fallback; no domain decision.
- `internal/cli/login.go` — loopback OAuth-ish flow, state check, URL
  normalisation (`https://` prefix). Transport, not domain.
- `internal/cli/client.go` (apart from `SetVarsAt`) — one request per method;
  `DotenvQuote:284` is string formatting, `apiError:545` is status mapping,
  `ForkSlice:698` only widens a timeout.
- `internal/cli/lifecycle.go` (apart from `RefSource.Expr:76`) — thin wrappers.
- `internal/cli/cmd/root.go`, `runtime.go` — wiring, IO, exit codes, tables,
  prompts. `runtime.go:193-212` (`Confirm`) is UX, not authorization.
- `internal/cli/cmd/shim.go` — argv rewriting for the legacy id-first form.
- `internal/cli/cmd/auth.go` — login/link/status; `choose` is a picker.
- `internal/cli/cmd/stack.go`, `org.go`, `members.go` (rest), `proxy.go`,
  `storage.go`, `defaults.go`, `registry.go` (rest) — bind flags, call one
  endpoint, render. Notably `defaults.go:38-49` parses `k=v` without
  validating knob names, leaving that to the server.
- `internal/cli/cmd/backup.go:131-146` — the good pattern: `kind`, `mode`,
  `tz`, `keep` are omitted when unset so the **server** defaults them.
- `internal/cli/cmd/deploy.go:163-222` — `waitForDeployment` uses the shared
  `internal/deploystate.IsTerminal` rather than re-listing terminal statuses.
  The precedent worth copying elsewhere in this shard.
- `internal/cli/cmd/infra.go` kind guards (`:92, :129, :169, :338, :402, :442`)
  and `tile.go:415` — these pick which endpoint to call after `/resolve`
  answered; the server re-checks (`IsManaged()`, route shape). Dispatch, not a
  duplicated rule.

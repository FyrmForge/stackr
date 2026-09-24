Source: internal/cli/cmd/ (21 command files; tests and the arg shim skipped)
Commit: c2423f0
Taken: the command tree — name, flags, one-line purpose, API call, prompt and `-y` behaviour, output shape — plus the UX rules worth keeping
Cut: all Go; cobra wiring, ref resolvers, the legacy `tile <ref> <verb>` shim, `--app` alias
Cuts belong to: cli/ (the new tree). Nothing here belongs in a leaf — the CLI decides nothing, it only asks.

Reference only. This is what the old CLI *did*, so the new one can be judged
against it, not a file to port.

## The rules, before the tree

These are the parts users notice and scripts depend on. They survive the
rewrite even where the commands do not.

**Global flags.** `--json` and `--yes`/`-y` are persistent on the root, so
every subcommand has them without declaring them. Nothing else is global.

**Exit codes.** `0` fine, `2` usage (unknown command, unknown flag, wrong
arg count — the flag-error hook forces 2 before cobra can pick), `1`
everything else. Plans add `3` under `--detailed-exitcode` (see below).
Root with no args prints help and exits `0` — deliberate; the previous CLI
exited 2 and broke people piping `stackr | less`.

**`--json` is a mode, not a formatter.** Data goes to stdout as JSON;
errors go to *stderr* as one JSON object `{error, status, code}` so a
wrapper can branch on status without parsing prose. An empty result is `[]`,
never `null`. Two commands refuse the flag outright rather than lie:
`forward` (a tunnel has no document) and `proxy show` (the product is YAML;
JSON would be re-quoted YAML).

**Tables.** One helper decides: a bordered lipgloss table on a TTY,
headerless tab-separated rows when piped (so `cut -f2` works), and `(none)`
printed on *stderr* when there are no rows — an empty stdout stays empty for
the pipe. `(none)` also fills empty cells. `STACKR_ACCESSIBLE` drops the
decoration.

**Confirmations.** One helper again. `--yes` skips the prompt. With `--json`
or no TTY and no `--yes` it refuses: *"refusing without --yes
(non-interactive): <the question>"* — the question is still printed, so a CI
log says what it would not do. Wording is a full sentence naming the thing
and its blast radius: *"Remove stack acme-shop and everything in it?"*,
*"Restore run r_31 over the live data? This cannot be undone."*, *"Remove
backup schedule b_7? Archives already in the bucket are kept."* Keep that
shape: verb, subject by name, what survives or does not.

**Secrets are never flags.** Every secret input is: flag (documented as
"prefer the prompt"), then env var, then a masked prompt. Masked on a TTY,
one whole line when piped. Argv leaks into shell history and `ps`.

**Resolution.** A directory is linked to stack/env/tile; a bare name
resolves inside that link, a full id resolves anywhere. `--stack`/`--env`
beat the link. When several things match and the session is interactive, a
picker; otherwise the error names the flag to pass: *"several stacks; pass
--stack <id>"*. A corrupt link file is an error, not "unlinked".

## The tree

`v1` / `later` per the REWRITE.md v1 scope. `later` means the verb is
outside v1, not that it was wrong.

### Session

- `login <url> [--with-key <key>]` — v1. Stores the server and a key; with
  no `--with-key` it opens the browser authorize flow. Writes the config
  file 0600.
- `logout` — v1. Drops the stored key.
- `link [--stack --env --tile]` — v1. Binds this directory. Each flag
  missing is picked: a lone candidate auto-picks, several open a select.
- `unlink` — v1.
- `status` — v1. Prints server, user, and the link. Table or JSON.

### stack

- `stack ls` — v1. `GET /stacks`. Table `ID NAME`.
- `stack create <name> [--desc --org]` — v1. `POST /stacks`.
- `stack rename <id> <name>` — v1. Sparse `PATCH` on the stack.
- `stack pr-envs [--enable/--disable --base <env> --ttl]` — v1. Tri-state
  parse so setting one knob does not reset the others; unset flags are
  absent from the body.
- `stack defaults --set k=v --clear k` — v1. `client.Settings` /
  `client.SetSettings`. See `defaults` below; same command shape at stack
  level.
- `stack export [-o file --force]` — v1. `client.ExportStackConfig`. Live
  state back out as
  `stackr-compose.yml`. Prints to stdout by default because where in the
  repo it goes is the author's call. Secrets come out as declarations,
  never values. `--force` to overwrite.
- `stack rm <id>` — v1. Confirms *"Remove stack %s and everything in it?"*.

### env

- `env ls`, `env create <slug>`, `env set <slug> [knobs]`, `env defaults`,
  `env reset <slug>` (`client.ResetEnv`), `env rm <slug>`
  (`client.DeleteEnv`) — v1. `env set` sends only changed flags;
  `env defaults` is `client.Settings` / `client.SetSettings` at env level.
- `env copy <src> <dst>` — later.
- **`env rm` and `env reset` carry bug B1/B21/B22** — see the bug section.

### tile

The centre of the CLI. `tile` is the noun; the API still says `app` on the
wire and `--app` stays a hidden alias.

- `tile ls` — v1. Table `ID NAME STATE IMAGE`.
- `tile create <name> [--image | --git --branch --connector] [--cron
  --schedule] [--port ...]` — v1. `POST /apps`.
- `tile set [ref] <flags>` — v1. The big one: a table of flag → PATCH key →
  parser covering image, git, branch, connector, port, healthcheck,
  build-args, published-ports, proxy override, security headers,
  watch-paths, schedule, command, timeout, allow-overlap, user, shm-size,
  privileged, devices, restart, depends-on, files, storage, update-policy,
  wait-for-ci. **Only flags the user actually changed are sent.** Two flag
  groups (`build`, `limits`) are coupled and go as a whole object when any
  member changed. No flags at all → *"nothing to set; pass …"* as a usage
  error (exit 2), never an empty PATCH.
- `tile get [ref]` — v1. Detail view; `--json` gives the raw object.
- `tile rm [ref]` — v1. Confirms by name.
- `tile stop` / `tile restart` — v1. `tile pause` — later.
- `tile run [ref]` / `tile stop-run` — later (cron tiles are not named in
  the v1 scope).
- `tile attach <instance>` / `tile detach` — v1. Managed-tile bindings.
- `tile provisions` — v1. What this tile is bound to.
- `tile refs` — v1. Which param refs this tile resolves and to what level.
- `tile deployments` — v1. History table.
- `tile rollback --tag <tag>` — v1. `--tag` is **required**: the help says
  *"the last good one is a judgement"*, so the CLI refuses to guess.
- `tile resources` — later (usage numbers are metrics).
- `tile metrics [--range 1h|6h|24h]` — later.
- `tile domain add <host> [--no-https]`, `tile domain auto`,
  `tile domain ls`, `tile domain set [--cert --key --clear-cert ...]`,
  `tile domain rm <host>` — v1. `set` sends only changed flags; `--cert`
  and `--key` must come together; `--clear-cert` explicitly sends empty
  strings — the one place a blank field is correct, because the user asked
  for the clear.
- `tile volume add <name:/path>`, `tile volume ls`, `tile volume rm` — v1.
  `rm` confirms and names the data.

### Deploy and logs

- `logs [ref] [--follow] [--tail 200]` — v1. SSE stream; under `--json`
  each line is an NDJSON object so `jq` works on a live tail.
- `deploy [ref] [--no-wait --timeout]` — v1. Starts a deploy, then polls
  the deployment, backing 1s → 5s, until `deploystate.IsTerminal`. Ctrl-C
  detaches and *says so* — the deploy keeps going server-side. Exit code
  reflects the deploy outcome, not the request.
- `deployment get <id>`, `deployment logs <id>`, `deployment cancel <id>` —
  v1. `cancel` only offered when the deployment is cancellable.
- `forward <ref> --port` — dropped. Port forwarding and `proxyrelay` are
  out. It refused `--json` and used the presence websocket as its startup
  probe.

### Releases and promote

- `releases [--env]` — v1. Table of releases with commit, tag, age.
- `promote [ref] --env <target> [--from <env>] [--plan] [--force]` — v1,
  and the headline verb of the rewrite. `pickRelease` accepts `head`,
  `latest`, or a sha prefix; under `--json` or `-y` it refuses to guess:
  *"a commit is required with --json or -y"*. `--plan` prints the diff and
  applies nothing.

### vars (the param store)

- `vars set KEY=VAL`, `KEY:secret=VAL`, `--secret`, `--generate <NAME>...`,
  `--length` — v1. `--generate` takes bare names, mints server-side, and
  **skips names that already have a value** rather than rotating them.
- `vars get [KEY]` — v1. **Masks secrets even under `--json`.** A value
  comes out only through the audited single-value route.
- `vars ls` — v1. Shows key, level that decided it, and whether this level
  overrides.
- `vars pull [-o file] [--force]` — v1. Writes 0600, refuses an existing
  file without `--force`. The help says outright: *"this is the one command
  that puts secrets on disk."*
- Scope flags carry their own rule: *"a level asked for by name must never
  silently become another."* If `--env staging` names nothing, that is an
  error, not a fall-back to the stack.

### infra (managed tiles)

Path-addressed: `acme:shop:prod:api`.

- `infra create <path> --engine <e> [--version --image]` — v1.
- `infra provision <path> <slice>` — v1. Slices.
- `infra ls` / `infra get <path>` — v1.
- `infra set <path> <flags>` — v1. Only changed flags.
- `infra public <path> [--on/--off]` — v1.
- `infra fork <path> <new>` — later.
- `infra rm <path> [--force]` — v1. `client.DeleteDB(id, saw)`, and **the
  pattern the new CLI should copy** (bug section).
- Hidden legacy scope-word subtrees (`db`, `cache`, …) alias into this.

### domain

- `domain ls` (`client.DomainResources`), `domain add <host> [--level
  instance|org|stack --owner --include-env-on-default]`
  (`client.AddDomainResource`), `domain rm <host-or-id>`
  (`client.DeleteDomainResource`) — v1. `rm` lists first so it can resolve
  host *or* id, then confirms. Table `ID LEVEL OWNER HOST`, with a trailing
  `env-on-default` cell only when set.

### backup

- `backup ls [tile]` — v1. `client.Backups`. Table
  `ID KIND CRON DEST KEEP ENABLED`, keep=0 shown as `all`.
- `backup create [tile] --dest --schedule [--kind --mode --tz --keep
  --disabled]` — v1 for volume kind; `--kind dump` follows managed-tile
  backups. `client.CreateBackup`. `--dest` accepts
  `${{ org.backups.NAME }}`. Only flags given are sent; `--keep` only when
  changed (0 is meaningful).
- `backup set <id> [--dest --schedule --mode --tz --keep --enabled]` — v1.
  `client.PatchBackup`. `--enabled` is string-typed and *parsed*, so
  `--enabled yes` errors rather than silently meaning false. Nothing set →
  usage error.
- `backup rm <id>` — v1. `client.DeleteBackup`. *"Archives already in the
  bucket are kept."*
- `backup runs <id>` — v1. `client.BackupRuns`. Table
  `ID STATUS TRIGGER CREATED SIZE OBJECT/ERROR`; the last column is the
  object key, or the error when the run failed — whichever of the two says
  what happened.
- `backup run <id>` — v1. `client.RunBackup`. Fire and print the run id.
- `backup restore <id> --run <run-id>` — v1. `client.Restore`, then
  `client.LatestRestore` on a 2s loop. `--run` required and the help names
  where run ids come from. Confirms *"This cannot be undone."*, then waits
  so "Restored." means restored; Ctrl-C prints *"stopped waiting; the
  restore continues server-side"*.
- `backup dest ls|add|set|rm` — v1. `client.Destinations` /
  `AddDestination` / `SetDestinationShared` / `DeleteDestination`. Table
  `ID NAME ENDPOINT/BUCKET SCOPE`, scope being `org:<id>` or `server-wide
  (shared)`. `add` dials the bucket server-side before storing, so bad
  credentials fail at the command. Keys via prompt or
  `STACKR_BACKUP_ACCESS_KEY` / `_SECRET_KEY`. `set` flips `--shared` only:
  endpoint, bucket and credentials are what existing archives were written
  with, and editing them in place would orphan those archives silently.

### org

- `org ls` — v1. The slug every path form starts with.
- `org members ls|add <email>|set <user-id> --role|rm` — v1.
  `client.Members` / `AddMember` / `SetMemberRole` / `RemoveMember`. Table
  `EMAIL NAME ROLE USER ID`. `add` is *always* an invite, never a direct
  membership: a user row is created by accepting one. The printed link is
  labelled *"The link is a credential"*.
- `org invites ls|add [--email --role --expires-days]|rm` — v1.
  `client.Invites` / `AddInvite` / `DeleteInvite`. Table
  `EMAIL ROLE STATE EXPIRES ID`. Without `--email` the link works for
  anyone holding it, and the help says so.
- `org defaults` — v1. `client.Settings` / `client.SetSettings`.
- `org export` — later (org config files are later).
  `client.ExportOrgConfig`.
- `org preview|plan|plans|plan-show|approve|reject` — later, same reason.
  `client.OrgPlanPreview` / `OrgPlanNow` / `OrgPlans` / `OrgPlan` /
  `ApproveOrgPlan` / `RejectOrgPlan`; `plans` is a table `ID STATUS
  SUMMARY`. `approve` reads the plan first and confirms **only when it
  deletes something**; it used to confirm blindly because nothing could
  read an org plan. Its output says *queued*, not applied, and prints the
  follow-up command.
- `org registry creds ls|add|rm` — later (built-in registry).
  `client.RegistryCredentials` / `AddRegistryCredential` /
  `DeleteRegistryCredential`. Table `NAME OWNER PREFIX LAST USED ID`;
  `add` prints the secret once with *"not stored and will not be shown
  again"*.

### plan (stack config-as-code)

- `plan preview -f stackr-compose.yml [--detailed-exitcode]` — v1. Posts
  the bundle, stores nothing. `readBundle` resolves one level of `include:`,
  refuses `..` escapes, caps at 1 MB.
- `plan now`, `plan list`, `plan show <id>` — v1.
- `plan approve <id>` — v1. Reads the plan, confirms **only when
  destructive**. Queued, not applied.
- `plan reject <id>` — v1.
- Exit codes under `--detailed-exitcode`: `0` no changes, `2` changes
  pending, `3` error — terraform's contract, because that is what CI
  wrappers already know. A quiet-exit error type exists so a non-zero code
  can be returned with nothing printed.

### defaults (the settings cascade)

- `defaults server|org|stack|env set [--set k=v ...] [--clear k ...]` — v1.
  `client.Settings`, then `client.SetSettings` only when something changed.
  One command shape for all four levels, deliberately: *"the levels are one
  cascade and reading them differently per level is how two of them drift."*
  With neither flag it prints what applies and **which level decided it** —
  table `KNOB APPLIES FROM HERE`, where `HERE` is `inherited` or this
  level's own value. `--clear` gives a knob back to the level above; it is
  a `null` in the body, not an empty string.

### Admin

- `registry ls|add|set|rm` — external pull registries are v1; `set`
  (managed registry TLS domain) is later. `client.Registries` /
  `AddRegistry` / `PatchRegistry` / `DeleteRegistry`. Table
  `NAME KIND URL DOMAIN ID`. `add` prompts for the password when a
  username is given. `set` tests `Changed("domain")` so `--domain ""`
  clears the domain instead of meaning "unset".
- `image ls|tags|rm <image>:<tag>` — later. `client.RegistryImages` /
  `RegistryTags` / `DeleteRegistryTag`. Tables `IMAGE REPOSITORY` and
  `TAG SIZE MB DIGEST`. `rm` splits on the *last* colon (image names hold
  slashes and ports), and the server refuses while a live deployment
  references the tag — a restart would pull a manifest-unknown with
  nothing left to explain it.
- `proxy show|override|entries|entry set|entry rm` — later in this shape;
  the v1 equivalent is admin-only extra Caddy config. `client.ProxyConfig`
  / `PutProxyOverride` / `PutProxyEntry` / `DeleteProxyEntry`. Worth
  keeping: the override *replaces* rather than merges and says so,
  clearing it is its own confirmed action, and `show` prints a leading
  comment line saying whether an override is active.
- `storage ls|add|probe|rm|path add|path rm` — later. `client.ListStorage`
  / `CreateStorage` / `DeleteStorage` / `CreateStoragePath` /
  `DeleteStoragePath`. Notable anyway: the server probes the share before
  the row is saved, and `rm` says *"Data on the share/pool is untouched."*

### Missing from the old CLI

API keys have no verbs — minting and revoking is panel-only. v1 lists API
keys, so the new tree needs `stackr key ls|add|rm`.

## Three bugs not to inherit

**B1 / B21 / B22 — the CLI must send only what the user gave it.** Most of
the old tree gets this right by testing `flags.Changed` before putting a key
in the body: `tile set`, `tile domain set`, `infra set`, `env set`,
`stack pr-envs`, and `registry set` — the last so that `--domain ""` clears
the domain rather than reading as "not given". A weaker second mechanism is
in `backup set` and `backup create`, which test `value != ""` for most
fields and `Changed` only for `--keep`. Do not copy those two: an
empty-string test cannot express *clear this field*, which is the same hole
that forces `tile domain set` to carry a separate `--clear-cert`. One
mechanism, everywhere. The failure mode when neither is used: a user
runs `tile set --port 8080`, the client helpfully serialises its whole
struct, and every field the user never mentioned — healthcheck, command,
devices — arrives as an empty string and is written as "cleared". Nothing
prints. The rule is one line: build a map, put a key in it only when the
corresponding flag changed, and refuse an empty map as a usage error.
Clearing a field is its own explicit flag (`--clear-cert`), never the
absence of a value.

**`-y` is not `--force`.** Two commands broke this. `env rm` and `env reset`
send `force: force || yes`, with the comment that `-y` "answered the prompt,
which is the same deliberate act force is." It is not. `-y` says *do not
stop to ask me*; `force` says *override the server's refusal*. Collapsing
them means a scripted `-y` silently overrides a guard the user never saw.

The fix already exists one file over, in `infra rm`, and it is the shape to
copy: a local `saw` flag starts as `force`, the confirm helper returns early
when `force || yes` and sets `saw = true` **only after a real prompt was
answered**, and `saw` is what goes to the server as force. The comment there
spells out the damage: *"this command used to send force=true on every path,
so the API's held-slices refusal was unreachable from the CLI and a scripted
`-y` destroyed every consumer's data without anything ever printing what it
was about to take."*

The web panel had the same bug from the other side: the promote form posted
`force: true` unconditionally, so the panel overrode a per-environment apply
policy the API and CLI respected. Both surfaces need the same rule, so it
belongs on the server's side of the boundary too: force must be an argument
the caller passes on purpose, and the server must refuse it when the caller
has no right to it.

## Notes for the builder

- The three output modes (TTY table / piped TSV / JSON) and the confirm
  helper are four functions and they are the whole UX. Write them once,
  first; every command then gets consistency for free, which is how the old
  tree stayed coherent across 21 files.
- `--json` implies non-interactive. Do not special-case it per command;
  make it a property of the runtime that the confirm and picker helpers
  read.
- Prompts refuse rather than default. A non-interactive run that cannot ask
  must fail with the question in the message, never assume yes *or* no.
- Every destructive confirmation names the subject and states what survives.
  That sentence is the only thing standing between a script and a deletion;
  it is worth the extra clause.
- Required flags whose absence is a judgement call (`rollback --tag`,
  `restore --run`, `promote` under `--json`) error and name where the value
  comes from. Do not infer "the latest one".
- Long-running verbs (`deploy`, `restore`) must distinguish *stopped
  watching* from *stopped doing*. Both print a line saying the work
  continues server-side.
- DECIDE: the new noun set drops `forward`, `storage`, `image` and the
  Traefik-shaped `proxy`. The hidden legacy aliases (`stacks`, `envs`,
  `domains`, `apps`, `--app`, the `tile <ref> <verb>` arg shim) exist only
  for scripts against installs that do not exist yet — memory says nothing
  is deployed anywhere, so they can go.

Size: source 6523 lines, extract 385 lines

# Sweep I — platform infra

Rubric: `docs/plans/service-extraction/11-business-logic-sweep.md`,
"What counts as business logic". `infra/` is allowed mechanism (talk to
docker, mount a share, send mail). What is recorded below is **policy living
in infra**: eligibility, naming/identity, defaulting, domain-shape validation,
and anything that answers a domain question rather than performing an action.

Severity follows the parent doc's own axis (`11-business-logic-sweep.md:88-93`,
org delete: "Misplaced, **not** bypassable"):

- **High** — two or more surfaces reach the rule, and at least one skips the
  service; or a handler/API calls infra directly for a domain decision.
- **Medium** — misplaced, but one caller repo-wide.
- **Low** — a rule with a real reason to sit here; record only.

## Two corrections to the brief

- **`VolumeFor` is not in `storagetiles`.** It is `infra/backup/backup.go:319`.
  `storagetiles` has `VolumeOpts` (`storagetiles.go:28`), which is probably
  what was meant. Both are judged below, under their real packages.
- **`storagetiles.ParseAttachment` (`storagetiles.go:210`) is an alias**:
  `var ParseAttachment = placement.ParseAttachment`. `infra/placement` is
  **shard H**. The grammar is H's to own; recorded here only as a spread
  witness so the wave plan does not count it twice.

## Files read

| File | Lines |
|---|---|
| `infra/cluster/cluster.go` | 487 |
| `infra/cluster/swarm.go` | 170 |
| `infra/runtime/runtime.go` | 2143 |
| `infra/runtime/service.go` | 1160 |
| `infra/runtime/swarm.go` | 495 |
| `infra/runtime/move.go` | 224 |
| `infra/runtime/deps.go` | 28 |
| `infra/agent/server.go` | 779 |
| `infra/agent/client.go` | 504 |
| `infra/agent/service.go` | 284 |
| `infra/agent/contract.go` | 230 |
| `infra/agent/node.go` | 175 |
| `infra/nodes/nodes.go` | 543 |
| `infra/netpool/netpool.go` | 284 |
| `infra/envnet/envnet.go` | 259 |
| `infra/forward/registry.go` | 56 |
| `infra/proxy/proxy.go` | 981 |
| `infra/proxy/probe.go` | 131 |
| `infra/proxy/trusted.go` | 112 |
| `infra/proxy/protect.go` | 101 |
| `infra/proxy/accesslog.go` | 82 |
| `infra/registry/token.go` | 421 |
| `infra/registry/gc.go` | 271 |
| `infra/registry/registry.go` | 226 |
| `infra/githubapp/feedback.go` | 416 |
| `infra/githubapp/githubapp.go` | 393 |
| `infra/githubapp/ci.go` | 123 |
| `infra/storagetiles/storagetiles.go` | 291 |
| `infra/backup/backup.go` | 791 |
| `infra/backup/work.go` | 252 |
| `infra/backup/authz.go` | 132 |
| `infra/backup/space.go` | 14 |
| `infra/metrics/metrics.go` | 441 |
| `infra/metrics/reconcile.go` | 133 |
| `infra/hostmetrics/host.go` | 168 |
| `infra/mail/mail.go` | 163 |
| `infra/gitlog/gitlog.go` | 96 |

37 files, 13 859 lines.

## Findings

### infra/backup/authz.go:19 — cross-tenant destination permission

- **Rule:** a backup destination is usable by an org only if it is that org's
  own, or admin-global *and* marked shared (`d.VisibleTo(orgID)`); a refusal is
  deliberately spelled "not found" so ids do not leak across orgs.
- **Category:** eligibility / permission.
- **Owner it should have:** `BackupDestinationService`.
- **Also spelled at:** called directly from `handlers/api/v1/backups.go:172`
  and from `service/backupdestination.go:227`. The API path reaches infra for
  a tenant-boundary check without passing through the service that owns the
  table.
- **Severity:** High.

### infra/backup/authz.go:77 — named-destination resolution and precedence

- **Rule:** `${{ org.backups.NAME }}` resolves only to a non-global
  destination of that org; `${{ stackr.backups.NAME }}` only to a global one
  the admin has shared; two matches in reach is an error rather than a coin
  flip.
- **Category:** eligibility + precedence.
- **Owner it should have:** `BackupDestinationService`.
- **Also spelled at:** `service/backupdestination.go:221`,
  `handlers/api/v1/backups.go:166`, `config/stackconf/backups.go:62`. Three
  callers, one of which is a handler.
- **Severity:** High.

### infra/backup/authz.go:49 — backup config validation

- **Rule:** known kind, container mode required for volume backups, schedule
  required and cron-parseable, retention not negative.
- **Category:** validation of domain shape.
- **Owner it should have:** `BackupScheduleService`.
- **Also spelled at:** `service/backupschedule.go:241` (the owner) and
  `handlers/web/handler/settings/handler.go:260` (the panel-database path,
  which calls infra directly — already flagged in the parent doc's
  "Backups" bullet).
- **Severity:** High — bypassable, and the bypass is live.
  *Corrected 2026-09-21 (`12-business-logic-plan.md` §2 #1):* not live. The
  panel-database path calls `backup.Validate` itself at `settings/handler.go:260`
  and reloads at `:267`; it is the panel's own backup, a row with no tile, and
  justified. Misplaced call, not a bypass.

### infra/backup/authz.go:43 — `ValidMode`

- **Rule:** container mode is one of pause/stop/live.
- **Category:** validation of domain shape.
- **Owner it should have:** `BackupScheduleService` (alongside `Validate`).
- **Also spelled at:** `config/stackconf/stackconf.go:973`.
- **Severity:** Medium.

### infra/backup/authz.go:114 — `ParseRef` reference grammar

- **Rule:** what counts as a `${{ scope.backups.name }}` reference at all, and
  which two scopes exist.
- **Category:** naming/identity.
- **Owner it should have:** `BackupDestinationService` (it is the grammar of
  the thing that service resolves).
- **Also spelled at:** `handlers/api/v1/backups.go:165`,
  `service/backupdestination.go:220`.
- **Severity:** Medium.

### infra/backup/backup.go:319 — `VolumeFor`

- **Rule:** which tiles own a volume at all — a volume tile's own, a managed
  instance's derived name, and "a service tile has none, back up its attached
  volumes instead".
- **Category:** answers a domain question about a tile.
- **Owner it should have:** `TileService` (or `BackupScheduleService`, which
  already uses it as a guard).
- **Also spelled at:** `service/backupschedule.go:236` and
  `handlers/web/handler/backups/handler.go:119` — a handler asking infra
  whether a tile is backable.
- **Severity:** High (handler reaches past the service).

### infra/backup/backup.go:159 — `prefixFor` object-key namespace

- **Rule:** an archive's S3 key root is `stackr/<org>/<stack>/<tile>/<backup>`,
  derived and never supplied, because prune deletes everything beneath it.
- **Category:** naming/identity (a tenant boundary expressed as a key prefix).
- **Owner it should have:** `BackupScheduleService`.
- **Also spelled at:** nowhere — private, one caller.
- **Severity:** Low. Record only; the derivation is the safety property.

### infra/backup/backup.go:653 — `restorable`

- **Rule:** a run is restorable only if it belongs to this backup, finished,
  produced an object key, and is not a panel backup (those restore on the host
  with `scripts/restore.sh`).
- **Category:** eligibility.
- **Owner it should have:** `BackupScheduleService`.
- **Also spelled at:** nowhere; `StartRestore` (`work.go:136`) and `restore`
  (`backup.go:679`) both call it.
- **Severity:** Medium — misplaced, not bypassable.

### infra/backup/backup.go:277 — "never prune on a pre-restore run"

- **Rule:** retention is skipped when `run.Trigger == "pre-restore"`, because
  the safety copy is the newest key and pruning would delete the archive being
  restored.
- **Category:** branching on domain state.
- **Owner it should have:** `BackupScheduleService` (it owns the trigger
  vocabulary; `work.go:104` documents "schedule" or "manual").
- **Also spelled at:** the trigger string is minted at `backup.go:697`.
- **Severity:** Medium.

### infra/nodes/nodes.go:326 — `ValidateAddress`

- **Rule:** a node may only join over a private, CGNAT or loopback address,
  because the overlay data plane is plain VXLAN.
- **Category:** eligibility.
- **Owner it should have:** `NodeService`.
- **Also spelled at:** used three times inside this package (`AddNode:349`,
  `Claim:460`, `Redeem:527`). Exported, so a second surface can reach it.
- **Severity:** Medium.

### infra/nodes/nodes.go:348 — `AddNode` (one row per address, name shape)

- **Rule:** one server row per address, refused by name of the row that holds
  it; a server name may not contain a line break; an empty name defaults to
  the address; a new row is `kind=swarm, role=worker, status=pending` and gets
  a join key immediately.
- **Category:** eligibility + defaulting + validation + orchestration.
- **Owner it should have:** `NodeService`.
- **Also spelled at:** called directly from
  `handlers/web/handler/server/nodes.go:67`, which then separately calls
  `service.NodeService.EnsureAgent`. Already flagged in the parent doc's
  "Nodes" bullet; this shard confirms the whole rule set is infra's.
- **Severity:** High.

### infra/nodes/nodes.go:399 — `IssueKey` rotation policy

- **Rule:** minting a new join key first burns every live key the row holds,
  so "new script" is a rotation and not an addition; TTL is one hour
  (`KeyTTL:37`); the key is 24 random bytes bound to the row's address.
- **Category:** permission / lifetime policy.
- **Owner it should have:** `NodeService` (which already owns the writes via
  `Rows.IssueKey` / `BurnKeysFor`, but not the decision).
- **Also spelled at:** `AddNode:383`; also reachable from the servers screen's
  "new script" button.
- **Severity:** Medium.

### infra/nodes/nodes.go:439 — `Claim` authorization

- **Rule:** three refusals stand between a callback and a swarm hijack — key
  inside TTL, request from a private address, node id that is really in this
  swarm and is not already held by another row.
- **Category:** permission.
- **Owner it should have:** `NodeService`.
- **Also spelled at:** the private-address rail is duplicated in `Redeem:526`
  with a different error string and a different disposition (Redeem logs and
  continues, Claim refuses).
- **Severity:** Medium — two spellings of one rail that have already drifted
  in wording.

### infra/nodes/nodes.go:512 — `Redeem` one-time-key policy

- **Rule:** a key is spent once, within its hour, and a public source address
  refuses it; a differing private address is logged and allowed (NAT).
- **Category:** permission.
- **Owner it should have:** `NodeService`.
- **Also spelled at:** see `Claim` above.
- **Severity:** Medium.

### infra/nodes/nodes.go:92 — `Sync` adoption policy

- **Rule:** a swarm node with no row is adopted, but *never while any row is
  pending*; a row whose node has left is kept rather than deleted; one node
  belongs to one row (`match:172`, the `claimed` set).
- **Category:** branching on domain state + orchestration.
- **Owner it should have:** `NodeService`.
- **Also spelled at:** nowhere; one caller (the servers screen).
- **Severity:** Medium.

### infra/nodes/nodes.go:218 — `mirror` field ownership

- **Rule:** swarm's view overwrites node id, hostname, role, status and
  address; the operator's display name is never touched, and an empty name is
  defaulted from the hostname.
- **Category:** defaulting / precedence between two sources of truth.
- **Owner it should have:** `NodeService`.
- **Also spelled at:** nowhere.
- **Severity:** Medium.

### infra/storagetiles/storagetiles.go:131 — `ValidateAttach`

- **Rule:** §2.7's state-placement rule — a managed instance may only attach
  local-backed storage, because database files over NFS/SMB corrupt.
- **Category:** eligibility.
- **Owner it should have:** `StorageService`.
- **Also spelled at:** `service/tilevalidate.go:243` and
  `handlers/web/handler/app/storage.go:51`. A web handler enforcing a
  corruption-class rule directly.
- **Severity:** High.

### infra/storagetiles/storagetiles.go:112 — `Probe`

- **Mechanism or rule:** **both**, and that is the problem. Mounting a
  throwaway container and listing its root is mechanism; "a bad probe means
  remove the volume so a fixed config recreates it with new opts" is a rule,
  and "probe at create/edit time rather than at first deploy" is a workflow
  decision.
- **Owner it should have:** `StorageService` owns *when* to probe and what a
  failure means; the mount-and-list stays here.
- **Also spelled at:** `service/storage.go:110`, `service/storage.go:216`,
  `handlers/web/handler/server/storage.go:33`,
  `handlers/api/v1/lifecycle.go:234`. Four call sites, two of them surfaces.
  (The parent doc already counts three; `service/storage.go` probes twice.)
- **Severity:** High.

### infra/storagetiles/storagetiles.go:149 — `ensureForConsumer` placement rules

- **Rule:** a network share's volume is made on every ready node; a local pool
  is made only on its home node; a consumer attaching local pools on two
  machines is refused; a consumer resolving to a different node than the pool's
  home is refused.
- **Category:** eligibility + branching on domain state.
- **Owner it should have:** `StorageService` (the refusals) — the per-node
  `CreateVolumeOpts` stays mechanism.
- **Also spelled at:** private; reached through `Resolve`, whose only caller is
  `infra/deploy/engine.go:734` (shard H).
- **Severity:** Medium.

### infra/storagetiles/storagetiles.go:256 — `resolveOrgShare` path confinement

- **Rule:** an org-share sub-path is cleaned and any `..` segment refuses the
  attachment ("must stay inside the share"); the org is resolved from the
  consumer's stack, so a share is only reachable from its own org.
- **Category:** validation of domain shape + tenant boundary.
- **Owner it should have:** `StorageService`.
- **Also spelled at:** nowhere. One caller.
- **Severity:** Medium — misplaced, not bypassable, but it is a traversal
  guard sitting below the layer that owns tenancy.

### infra/storagetiles/storagetiles.go:83 — `DropOrgShareVolumes`

- **Mechanism or rule:** mostly mechanism (list nodes, list volumes, remove by
  prefix), but it encodes the org-share *edit dance*: opts are immutable, so an
  edit drops every consumer volume and the next deploy recreates them, and
  **the caller must not save the edit if this fails**. That contract is a rule
  and it is written only in a doc comment.
- **Owner it should have:** `StorageService` / `orgconf` applier should own the
  "drop then save, abort on failure" sequence.
- **Also spelled at:** `service/storage.go:171`, `config/orgconf/storage.go:224`
  and `:241` — three callers, each responsible for honouring the contract on
  its own.
- **Severity:** High.

### infra/storagetiles/storagetiles.go:23 — `ValidBackend` / `Backends`

- **Rule:** the closed set of storage backends.
- **Category:** validation of domain shape.
- **Owner it should have:** `StorageService` (it is the only non-infra caller,
  `service/storage.go:60`).
- **Also spelled at:** the same three literals are re-spelled inside
  `VolumeOpts` (`storagetiles.go:30-53`) as a switch, so adding a backend means
  editing two places.
- **Severity:** Low — one caller, but the duplication is real.

### infra/proxy/protect.go:35 — `basicAuthEntry` cascade and fail-closed

- **Rule:** a tile's own basic-auth user/password wins over the settings
  cascade; protection on without a resolvable password locks the URL with a
  hash nobody knows rather than publishing it; a cascade that cannot be read is
  an error so the caller keeps the route file it already has.
- **Category:** precedence + eligibility.
- **Owner it should have:** `ProxyService` (the `Settings` interface moved the
  *rows*; this decision did not move with them).
- **Also spelled at:** nowhere; one caller (`WriteApp`).
- **Severity:** Medium.

### infra/proxy/proxy.go:963 — `resolverFor` ACME account selection

- **Rule:** a host's certificate is issued on the longest-matching domain
  resource that names an ACME address, else the instance's; resources nest, so
  a stack's account beats its org's.
- **Category:** precedence.
- **Owner it should have:** `DomainResourceService`.
- **Also spelled at:** the same account set is rebuilt in `acmeAccounts:926`
  for the static config, with its own dedupe and sort.
- **Severity:** Medium.

### infra/proxy/proxy.go:542 — `stackMiddleware` cross-stack resolution

- **Rule:** a bare middleware name is the tile's own stack's; `stack/name`
  reaches another stack **in the same org**; an unresolvable reference becomes
  a deliberately dead router name rather than silently dropping a forwardAuth
  guard off a public route.
- **Category:** naming/identity + tenant boundary.
- **Owner it should have:** `StackService` (it owns the middleware map) or
  `ProxyService`.
- **Also spelled at:** the name function `StackMiddlewareName:532` is exported
  and keyed on stack id; the resolution rule is not.
- **Severity:** Medium.

### infra/proxy/proxy.go:839 — `routablePanel`

- **Rule:** the panel gets a generated route only when `BASE_URL` has a
  hostname that is neither empty, `localhost`, nor a bare IP.
- **Category:** eligibility.
- **Owner it should have:** `ProxyService`.
- **Also spelled at:** nowhere.
- **Severity:** Low.

### infra/proxy/proxy.go:389 — `WriteApp` render-time policy

- **Rule:** a `TraefikOverride` replaces the generated file verbatim; no
  domains means remove the file; redirect domains bypass auth and header
  middlewares; `HTTPS && !ForceHTTPS` still serves the tile on the web
  entrypoint; wildcard hosts are pinned to the `ledns` resolver.
- **Category:** branching on domain state.
- **Owner it should have:** mostly render, so mostly mechanism — except the
  redirect-bypasses-auth and wildcard-forces-DNS decisions, which are
  `DomainService`'s answers about a domain row.
- **Also spelled at:** nowhere.
- **Severity:** Low. Record only; splitting the renderer would cost more than
  it buys.

### infra/proxy/proxy.go:621 — `Resync` fan-out

- **Rule:** at boot, regenerate every tile's route, every stack's middlewares,
  the operator's custom dynamic entries and the managed registry's route; prune
  orphans; one tile whose settings cannot be read must not stop the rest.
- **Category:** orchestration + side effects fired by infra.
- **Owner it should have:** `ProxyService` should own the sequence; each write
  stays here.
- **Also spelled at:** nowhere.
- **Severity:** Low — one caller (boot), and the ordering is documented.

### infra/registry/token.go:201 — `Namespace`

- **Rule:** an org owns the repository prefix `<org-slug>_` and nothing
  outside it; the trailing underscore is the boundary that stops `acme_`
  matching `acme-evil_`.
- **Category:** naming/identity — this *is* the registry tenant boundary.
- **Owner it should have:** `RegistryService`.
- **Also spelled at:** `service/registry.go:250`,
  `handlers/api/v1/registry.go:105`, `handlers/web/handler/org/registry.go:85`,
  and `handlers/web/handler/org/registry_templ.go:315` (generated — evidence of
  spread only, nothing to change there). Four surfaces build the same prefix.
- **Severity:** High.

### infra/registry/token.go:232 — `GrantFor`

- **Rule:** a requested scope is narrowed to the org's namespace; anything
  outside is dropped rather than refused, so the registry issues its own 401.
- **Category:** permission.
- **Owner it should have:** `RegistryService` (which calls it at
  `service/registry.go:155` but does not own the rule).
- **Also spelled at:** the prefix test is `Namespace` again; the scope-string
  parse is duplicated verbatim in `GrantAll:208`.
- **Severity:** Medium — `OrgHasImages` moved out, this did not.

### infra/registry/token.go:208 — `GrantAll` / :339 `GrantAgentPull`

- **Rule:** the root credential gets every scope it asks for; the agent
  identity gets pull on `stkr-agent` and nothing else, including no push of its
  own image.
- **Category:** permission.
- **Owner it should have:** `RegistryService` (`service/registry.go:132,136`
  already decides *which* of the three to call — the tiers themselves belong
  next to that decision).
- **Also spelled at:** nowhere else.
- **Severity:** Medium.

### infra/registry/token.go:269 — `systemSecret` / :331 `AgentSecret` derivation

- **Rule:** an org's push credential and the agent's pull credential are HMACs
  of the registry password, so rotating the password rotates every credential;
  the table stores only hashes.
- **Category:** identity derivation.
- **Owner it should have:** `RegistryService`. Borderline: the derivation is
  cryptographic mechanism, the "one system credential per org, not deletable,
  repaired on mismatch" part (`EnsureSystemCredential:285`) is the rule.
- **Also spelled at:** `EnsureSystemCredential` is reached through
  `OrgCredential:357`, called from `infra/deploy/engine.go:922` and
  `infra/jobs/jobs.go:582` (both shard H).
- **Severity:** Medium.

### infra/registry/gc.go:68 — `repoName` / `tagRef` grammar

- **Rule:** this install's repositories are flat and underscore-joined, so a
  legitimate name has no slash; refusing the slash is what makes the registry's
  redirect-to-cleaned-path unreachable.
- **Category:** validation of domain shape (traversal guard).
- **Owner it should have:** `RegistryService` — `ValidRepoName` / `ValidTag`
  exist precisely so callers can refuse with their own status code, and they
  are already used at `service/registry.go:254,271` and
  `handlers/api/v1/registry.go:177`.
- **Also spelled at:** the API handler validates the tag itself rather than
  going through the service.
- **Severity:** Medium.

### infra/registry/registry.go:51 — `EnsureManaged` identity and defaults

- **Rule:** the managed registry row is named "stackr (managed)", user
  `stackr`, URL `localhost:<port>`, with a random 16-byte password, created on
  demand.
- **Category:** defaulting / naming.
- **Owner it should have:** `RegistryService` (it already owns the row through
  `Registries.EnsureRow`; the shape of a first row is still infra's).
- **Also spelled at:** nowhere.
- **Severity:** Low.

### infra/envnet/envnet.go:120 — `UpperEnv`

- **Rule:** a tile lives "above the default env" when its environment is not
  the stack's first static environment; upper rungs get images by Promote, so a
  build from branch head is refused there.
- **Category:** branching on domain state — a deploy-eligibility rule inside
  the naming package.
- **Owner it should have:** `EnvironmentService` (or `DeployService`, its
  consumer).
- **Also spelled at:** `service/deploy.go:76` and
  `handlers/web/components/panel.go:74`. The panel re-derives the same verdict
  to decide what to render.
- **Severity:** High.

### infra/envnet/envnet.go:67 — docker object naming scheme

- **Rule:** `stkr_<org>_<stack>_<env>`, with the stack home collapsing to
  `stkr_<org>_<stack>`; `_` as separator because slugs cannot contain one;
  63-char truncation gives way on the tile slug, never on the run id.
- **Category:** naming/identity.
- **Owner it should have:** stays here — this is the package whose stated job
  is "every docker object name is built here and nowhere else", and nothing
  parses the names back.
- **Also spelled at:** `TileAlias` is duplicated deliberately in
  `proxy.TileAlias` (`proxy.go:137`) with a comment saying the two must match.
- **Severity:** Low. Record; the duplication is a known, commented one.

### infra/envnet/envnet.go:209 — `TearDown`

- **Rule:** removing a tile's docker state means its service *and* any
  container left under the tile's labels, best-effort, because a tile whose
  objects are already gone must not strand its rows.
- **Category:** orchestration.
- **Owner it should have:** `TileService` should own "what deleting a tile
  means"; the two removals stay here.
- **Also spelled at:** nowhere.
- **Severity:** Low.

### infra/metrics/reconcile.go:24 — `reconcileStatus`

- **Rule:** which tile statuses a container check may arbitrate
  (running/unhealthy/stopped/error only), which kinds are exempt
  (`runpolicy.KeepAlive == false`, volume tiles), and that a running container
  clears a stuck `error`. Writes `tiles.status` and fires
  `notifier.Project(stackID)` per changed stack.
- **Category:** branching on domain state (rubric 1) + side effects fired by a
  caller rather than the owner (rubric 5).
- **Owner it should have:** `TileService`. The `Rows` interface
  (`metrics.go:28`) moved who *writes* the column; the decision and the
  notifier fan-out are still infra's.
- **Also spelled at:** the ownership rule is stated to match
  `handlers/web/handler/app/handler.go`'s `containerID`.
- **Severity:** High — infra decides a tile's displayed state and pushes it to
  every open canvas.

### infra/metrics/metrics.go:211 — retention read

- **Rule:** metric retention comes from `settings.ForServer(...).MetricRetentionHours`,
  read by the sampler itself.
- **Category:** defaulting read by a non-owner.
- **Owner it should have:** `SettingsService` should hand the sampler a value;
  the prune call stays here.
- **Also spelled at:** the same pattern in `infra/backup/work.go:80,96`, which
  reads `settings.Resolved` for queue concurrency.
- **Severity:** Low.

### infra/githubapp/githubapp.go:355 — `connectorForTile` org check

- **Rule:** a tile's `connector_id` is refused unless the connector belongs to
  the tile's stack's org (a stack file naming another org's connector would
  clone that org's private repos); with no explicit id, the org's *single*
  connected github connector is used, and ambiguity resolves to nothing.
- **Category:** permission + defaulting.
- **Owner it should have:** `ConnectorService` (the `Connectors` interface
  already owns the rows).
- **Also spelled at:** exported twice — `ConnectorForTile` (`feedback.go:49`)
  is a public wrapper over the same private function.
- **Severity:** Medium — the cross-org guard is the whole reason this exists,
  and it sits below the layer that owns org membership.

### infra/githubapp/feedback.go:73 — `DeployFinished` eligibility

- **Rule:** PR feedback only for a github git tile in an `ephemeral`
  environment whose slug starts `pr-`; a `cancelled` deploy is not a CI
  failure; `prCfg.NoComment`/`NoStatus` suppress each half.
- **Category:** branching on domain state.
- **Owner it should have:** `DeployService` should decide that a deploy
  warrants PR feedback; posting it stays here.
- **Also spelled at:** the `pr-` slug convention is re-parsed at `:84` to get
  the PR number.
- **Severity:** Medium.

### infra/githubapp/ci.go:95 — `mergeVerdict`

- **Rule:** CI passes only when every check run completed success/neutral/
  skipped **and** the combined status is green; failure anywhere wins, then
  pending, then "nothing reported".
- **Category:** answers a domain question (is this commit deployable).
- **Owner it should have:** `CIGateService` / whatever owns `wait_for_ci`
  (`infra/cigate`, shard H) — flagged here because the verdict is defined in
  this package.
- **Also spelled at:** nowhere.
- **Severity:** Low — one definition, but cross-shard; H should confirm.

### infra/cluster/cluster.go:436 — `NodeOfStorage` defaulting

- **Rule:** a storage row with no server node id resolves to *this* node when
  its server is `"local"` or when it is an org share (network-mounted, any node
  will do); otherwise it is an error.
- **Category:** defaulting.
- **Owner it should have:** `StorageService`.
- **Also spelled at:** nowhere; `storagetiles.ensureForConsumer:150` is the
  caller.
- **Severity:** Medium.

### infra/cluster/cluster.go:78 — `NodeOf` pinning rules

- **Rule:** a tile's node comes from `placement`, never `t.HomeNode`; a pinned
  tile without a home node is an error naming the tile; an unpinned one is an
  error telling the caller to use its running task's node.
- **Category:** answers a domain question about a tile.
- **Owner it should have:** `infra/placement` already owns the derivation
  (shard H); the two error dispositions are `TileService`'s.
- **Also spelled at:** nowhere.
- **Severity:** Low.

### infra/netpool/netpool.go:116 — `firstFree` allocation policy

- **Rule:** an environment takes the lowest-numbered unheld pooled name, past
  the pre-created size if needed; growth attaches traefik to the new overlay.
- **Category:** mechanism with a policy edge (who may grow the pool, and that
  growth costs a traefik roll).
- **Owner it should have:** stays here — the package's own doc explains why the
  name means nothing and the row is the only mapping. The `EnvOwner`/
  `TileOwner` interfaces already moved the writes out.
- **Severity:** Low. Clean by design.

### infra/runtime/service.go:127 — `NormalizeRestart`

- **Rule:** the accepted restart vocabulary and its aliases; every surface
  (form, API, config) must run through here so the stored column is canonical.
- **Category:** validation of domain shape.
- **Owner it should have:** `TileService`. **Parked:** the doc comment says it
  lives here because the config layer and the tile service both need it and the
  service cannot import the config engine without a cycle.
- **Also spelled at:** by design, everywhere.
- **Severity:** Low — record for shard J, do not move blind.

### infra/runtime/deps.go:15 — `ParseDep`

- **Rule:** `depends_on` grammar; bare slug means `started`; conditions are
  started/healthy/completed.
- **Category:** validation of domain shape.
- **Owner it should have:** `TileService`. **Parked** for the same cycle reason
  (stated in the doc comment).
- **Severity:** Low.

### infra/runtime/runtime.go:1386 — `ParseFileMount`

- **Rule:** `files:` grammar — source must be a clean repo-relative path with
  no upward escape, container path must be absolute, optional `:template`.
- **Category:** validation of domain shape (and a traversal guard).
- **Owner it should have:** `TileService`. **Parked** — same stated reason.
- **Severity:** Low.

### infra/runtime/runtime.go:655 — `ParseDevice`

- **Rule:** device-line grammar; container path defaults to host path,
  permissions default `rwm`; both paths must be absolute.
- **Category:** validation of domain shape + defaulting.
- **Owner it should have:** `TileService`. **Parked** — same stated reason.
- **Severity:** Low.

### infra/runtime/swarm.go:53 — `Node.Status` / :66 `IsManager`

- **Rule:** docker's state and availability fold into the one word the servers
  list shows; the manager has no Drain and no Remove.
- **Category:** answers a domain question, and `IsManager` is the basis of a
  permission the service re-states at `service/node.go:121`.
- **Owner it should have:** `NodeService` owns the refusal (it does); the word
  is presentation and may stay.
- **Severity:** Low.

### infra/runtime/service.go:781 — `RolledSince` / :790 `RolledBack` / :797 `Converged`

- **Rule:** what "this deploy has settled" and "swarm gave up and rolled back"
  mean; a rolled-back service is a *failed* deploy even though it is healthy.
- **Category:** answers a domain question the deploy engine acts on.
- **Owner it should have:** `DeployService` owns "a rollback is a failure"; the
  swarm-state folding stays here.
- **Also spelled at:** consumed by `infra/deploy` (shard H).
- **Severity:** Low.

### infra/runtime/runtime.go:1432 — `systemRoleLabel` protected set

- **Rule:** which containers may never be stopped from the UI — agent, proxy,
  registry, and this process itself.
- **Category:** eligibility / permission.
- **Owner it should have:** arguably `TileService`/container admin, but the
  labels are docker's and the check must work inside the agent with no
  database. Stays.
- **Severity:** Low. Clean by design; `cluster.ContainerIsSystem:196`
  fail-closes on an error, which is the right disposition.

## Mechanism vs rule calls

Exported surface of the four packages the brief asked to judge, plus rule rows
elsewhere. Wholly-mechanical packages get one blanket line with their
exceptions named.

### infra/storagetiles

| Symbol | Mechanism or rule | Owner if rule |
|---|---|---|
| `Backends` | rule | `StorageService` |
| `ValidBackend` | rule | `StorageService` |
| `VolumeOpts` | mechanism (driver-opt construction) — except the "local export must be absolute" check, which is a rule | `StorageService` |
| `EnsureVolume` | mechanism | — |
| `DropOrgShareVolumes` | mechanism + an unwritten caller contract (rule) | `StorageService` / orgconf applier |
| `Probe` | mechanism + rule (when, and what a failure means) | `StorageService` |
| `ValidateAttach` | rule | `StorageService` |
| `ParseAttachment` | alias of `placement.ParseAttachment` | **shard H** |
| `Resolve` | orchestration (rule) | `StorageService` |

### infra/proxy

| Symbol | Mechanism or rule | Owner if rule |
|---|---|---|
| `New`, `UseEnvs`, `UseSettings`, `UseRegistries` | mechanism (DI) | — |
| `Settings` (interface) | mechanism — ownership already moved | — |
| `TileAlias` | naming rule, deliberately duplicated from `envnet` | `envnet` |
| `StackMiddlewareName` | naming rule | `StackService` |
| `EnsureTraefik` | mechanism | — |
| `WriteApp` | mechanism + render-time branches (low-grade rule) | `DomainService` |
| `WriteStackMiddlewares` | mechanism | — |
| `WriteRegistry` | mechanism | — |
| `RemoveApp` | mechanism | — |
| `Resync` | orchestration (rule) | `ProxyService` |
| `SyncCustomDynamic` | mechanism | — |
| `CurrentStatic` | mechanism | — |
| `RefreshCloudflare` | mechanism | — |
| `AccessLog` | mechanism | — |
| *(unexported)* `basicAuthEntry` | rule | `ProxyService` |
| *(unexported)* `resolverFor`, `acmeAccounts` | rule | `DomainResourceService` |
| *(unexported)* `stackMiddleware` | rule | `StackService` |
| *(unexported)* `routablePanel` | rule | `ProxyService` |

### infra/registry

| Symbol | Mechanism or rule | Owner if rule |
|---|---|---|
| `Registries` (interface) | mechanism — ownership already moved | — |
| `EnsureManaged` | mechanism + first-row defaults (rule) | `RegistryService` |
| `ServiceName`, `Issuer`, `Service`, `TokenTTL`, `TokenPath` | mechanism (protocol constants) | — |
| `PanelAddr`, `PullAddr` | mechanism (address selection) | — |
| `EncodeAuth`, `Auth` | mechanism | — |
| `LoadSigner`, `Signer.Sign` | mechanism | — |
| `Access` | mechanism (wire shape) | — |
| `Namespace` | **rule** — the tenant boundary | `RegistryService` |
| `GrantAll` | rule | `RegistryService` |
| `GrantFor` | rule | `RegistryService` |
| `GrantAgentPull` | rule | `RegistryService` |
| `HashSecret` | mechanism | — |
| `SystemCredentialName`, `AgentUser`, `AgentRepo` | naming rule | `RegistryService` |
| `AgentSecret` | mechanism (derivation) | — |
| `EnsureSystemCredential` | rule (one per org, repaired on rotation) | `RegistryService` |
| `OrgCredential` | rule (resolves org, mints, refuses root pair) | `RegistryService` |
| `OrgPullAuth` | mechanism over `OrgCredential` | — |
| `GarbageCollect` | mechanism | — |
| `NewClient`, `Client.Images/Tags/Tag/DeleteTag` | mechanism (`Images` filters by `Namespace`) | — |
| `ValidRepoName`, `ValidTag` | rule | `RegistryService` |

### infra/nodes

| Symbol | Mechanism or rule | Owner if rule |
|---|---|---|
| `LocalID`, `KeyTTL`, `PingUnknown`, `PingRef` | mechanism (`KeyTTL` is a policy constant) | `NodeService` |
| `Rows` (interface) | mechanism — ownership already moved | — |
| `Row` | mechanism (view model) | — |
| `Service.Sync` | rule (adoption, pending gate, one-node-one-row) | `NodeService` |
| `Service.Ping` | mechanism | — |
| `Service.StoreSample` | mechanism | — |
| `ValidateAddress` | rule | `NodeService` |
| `Service.AddNode` | rule | `NodeService` |
| `Service.IssueKey` | rule (rotation) | `NodeService` |
| `Service.Claim` | rule (permission) | `NodeService` |
| `Service.Redeem` | rule (permission) | `NodeService` |

### Blanket lines

| Package | Verdict | Exceptions (rules) |
|---|---|---|
| `infra/runtime` | mechanism — the single docker boundary | `NormalizeRestart`, `ParseDep`, `ParseFileMount`, `ParseDevice`, `Node.Status`/`IsManager`, `ServiceState.Converged`/`RolledSince`/`RolledBack`, `systemRoleLabel` |
| `infra/agent` | mechanism — narrow RPC over one node's socket | none |
| `infra/cluster` | mechanism — routes a call to a socket or an agent | `NodeOf`, `NodeOfStorage` |
| `infra/netpool` | mechanism — allocator | none (writes already moved to `EnvOwner`/`TileOwner`) |
| `infra/envnet` | mechanism — naming | `UpperEnv`, `TearDown` (sequence only) |
| `infra/metrics` | mechanism — sampler | `reconcileStatus` + the notifier fan-out, retention read |
| `infra/backup` | mechanism (S3, tar, exec) | all of `authz.go`, `VolumeFor`, `prefixFor`, `restorable`, the pre-restore prune skip |
| `infra/githubapp` | mechanism — GitHub API client | `connectorForTile`, `DeployFinished` eligibility, `mergeVerdict` |

## Clean files

No policy found; mechanism only.

- `infra/agent/contract.go` — wire types and path constants.
- `infra/agent/client.go` — the panel's half of the RPC. Stream-trailer
  handling is correctness, not domain policy.
- `infra/agent/server.go` — the node's half. `auth` is a shared-key check with
  its ceiling written down; `overlayAddr` fails closed.
- `infra/agent/node.go` — socket-or-agent dispatch, address cache.
- `infra/agent/service.go` — builds the agent service; `publish` picks a
  pullable image reference and a pull-only credential. Infrastructure
  bootstrap, not domain.
- `infra/cluster/swarm.go` — thin forwards to the manager's socket.
- `infra/runtime/move.go` — rsync two-pass volume move.
- `infra/runtime/swarm.go` — node, task and secret reads. (`Node.Status` and
  `IsManager` are noted above but stay.)
- `infra/forward/registry.go` — an in-memory session set.
- `infra/hostmetrics/host.go` — `/proc` and `statfs` reads.
- `infra/mail/mail.go` — SMTP2GO adapter; "nil mailer means no provider" is a
  degradation contract, not a domain rule.
- `infra/gitlog/gitlog.go` — `git log` and `merge-base` shelling.
- `infra/proxy/probe.go` — asks traefik whether a route loaded and restarts it
  once. A transport error counts as loaded, which is a correctness choice.
- `infra/proxy/trusted.go` — Cloudflare range fetch and CIDR merge.
- `infra/proxy/accesslog.go` — tail and filter of traefik's JSON log.
- `infra/backup/space.go` — `statfs`.

## Node lifecycle: the full split

Asked for explicitly. `infra/nodes` and `service/node.go` each hold half of one
workflow, and the boundary does not follow any rule.

| Stage | Lives in | Notes |
|---|---|---|
| Validate the address | `infra/nodes:326` `ValidateAddress` | rule in infra |
| Refuse a duplicate address | `infra/nodes:364` (inside `AddNode`) | rule in infra |
| Refuse a name with a line break | `infra/nodes:357` | rule in infra |
| Default an empty name to the address | `infra/nodes:371` | rule in infra |
| Create the pending row | `infra/nodes:380` → `Rows.Adopt` → `service/node.go:Adopt` | decision infra, write service |
| Mint the join key, burning the old | `infra/nodes:399` → `Rows.IssueKey`/`BurnKeysFor` | decision infra, write service |
| Start the agent service (+ retry loop) | `service/node.go:EnsureAgent` | service |
| Render the join script | `handlers/web/handler/server/joinscript.go` | surface |
| Redeem the key on join | `infra/nodes:512` `Redeem` | rule in infra |
| Claim the swarm node id | `infra/nodes:439` `Claim` | rule in infra |
| Reconcile swarm into the table | `infra/nodes:92` `Sync` / `:172` `match` / `:218` `mirror` | rule in infra |
| Ping and store samples | `infra/nodes:249` / `:305` | mechanism |
| List a node's tasks, split pinned vs stateless | `service/node.go:Tasks` / `PinnedTiles` | service |
| Drain (refuse if pinned tiles) | `service/node.go:Drain` | service |
| Activate | `service/node.go:Activate` | service |
| Remove (manager refused, typed confirmation, burn keys, delete row) | `service/node.go:Remove` | service |
| Set the node group label | `service/node.go:SetGroup` | service |
| Reads (`Get`, `ByNodeID`, `ListAll`) | `service/node.go` | service |

The shape of it: **everything before a node has joined is infra's; everything
after it has joined is the service's.** `NodeService` owns removal and refuses
to drain a node holding volumes, while `infra/nodes` owns who may join, under
what name, with what key, for how long. `handlers/web/handler/server/nodes.go:67`
sits astride the seam — it calls `h.nodes.AddNode` and then
`service.NodeService.EnsureAgent` as two steps of one operator action, so the
handler is currently the only place the whole "add a node" workflow exists.
`service/node.go`'s own comment on the `Adopt`/`Save`/`IssueKey`/`BurnKey`
block says "there is no rule on the far side of these" — which is true of the
writes and not of the decisions that produce them.

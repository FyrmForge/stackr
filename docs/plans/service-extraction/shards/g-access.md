# Shard: access (lens 1, authorization on purpose)

Read-only sweep, 2026-09-18. Paths are under `internal/stackrd/` unless they start with `cmd/` or `internal/cli/`. Extends `orgs-members-auth.md`; the three holes it already found (api `cancelDeployment` read-gate, org plan owner vs member, `PATCH /orgs/:id/settings` no managed 409) are listed in the matrix for completeness but not re-argued.

## 0. The gate vocabulary (what each surface can say)

| level | web (session) | api (x-api-key) | notes |
|---|---|---|---|
| authenticated | `auth.RequireAuth()` per route, `handlers/web/server.go:206-699` | `KeyAuth` `api/v1/auth.go:149` on the `/api/v1` group `api/server.go:82` | neither checks `User.Active`; only login does (`service/auth.go:117`) |
| in org (tenancy) | `InOrg` / `RequireOrgAccess` `middleware/orgctx.go:158,179` (404, plus draft gate `RequireOrgSetup` `setupgate.go:412`) | `orgMember` / `orgAllowed` / `requireOrg` `auth.go:217,230`, `variables.go:319` (404, draft 409 `orgReady` :257) | api caches all orgs' setup state per request `auth.go:238` |
| content write (owner or member) | `RequireOrgWrite` `orgctx.go:220` (403 only when `mutating`), `RequireStackAccess` :258 | `requireOrgWrite` `auth.go:300`, `requireTile(write=true)` :313, `requireEnvWrite` `envs.go:26`, `requireConfigStack(write)` `config.go:39` | api `requireStackAccess` :276 is tenancy only; every write must add `requireOrgWrite` by hand |
| owner | `ownedOrg` `web/org/members.go:45`, `ownedSettingsOrg` `web/org/settings.go:217` (404 not 403) | `requireOrgOwner` `api/v1/members.go:26` (403) | six copies, see orgs shard |
| server admin | `adminOnly` closure `server.go:194` (404), `IsAdmin` `orgctx.go:44` | `adminOnly` `auth.go:207` (403), `isAdmin` :200 | admins are owner everywhere: `orgctx.go:107`, `auth.go:169,218,301` |
| scope | none | `requireScope` `auth.go:185` via `op[...]` `v1.go:82-83` | scopes are a second layer on top of role, never a substitute |
| structural lock | `RequireUnmanaged` / `ManagedErr` `middleware/managed.go:431,442`, inline `ConfigManaged()` in ~20 handlers | `rejectManaged` `api/v1/errors.go:23`, `rejectManagedOwner` `domainresources.go:166` | not access, but the same handlers decide it |
| active-org shortcuts | `ReadOnlyGuard` `orgctx.go:268` and `CanWrite` :148 read the cookie org, not the resource org | none | every id-addressed handler must call the resource-org check itself; the guard is a backstop only |

## 1. Gate matrix

Columns: operation, web gate, api gate, other surface gate, same level, class. `--` means no implementation on that surface. Role words: any = any org member, write = owner or member, owner, admin.

| operation | web gate | api gate | stream / webhook / cli / public | same? | class |
|---|---|---|---|---|---|
| List orgs | `OrgContext` `orgctx.go:58` (memberships, admins all, drafts hidden as active) | `listOrgs` `slugpath.go:109-131` any; drafts included | cli `org ls` via api | no (drafts) | known |
| Create org | admin: route `server.go:310` + `IsAdmin` `web/org/handler.go:68` | -- | -- | n/a | |
| Rename / delete org | owner `ownedOrg` `web/org/members.go:68,166` | -- | orgconf apply (plan approval) `config/orgconf/orgconf.go:660-673` | n/a | |
| Invite, role, remove, invites | owner `ownedOrg` :235,320,349,392,410 | owner `requireOrgOwner` `api/v1/members.go:62,95,122,160,176,193` | wizard team routes `server.go:244-249` same funcs | yes | |
| List members | any `settingsOrg` `web/org/settings.go:161` | any `requireOrg` `members.go:42` | | yes | |
| Org defaults (settings) | owner + managed 409 `web/org/registry.go:258-264` | write, no managed check `api/v1/settings.go:47-57` | | no | known A |
| Org env colours | owner + managed 409 `web/org/settings.go:125-129` | -- | | n/a | |
| Org variables | write `web/org/settings.go:421,458` (+ secret reveal `CanWriteOrg` :345) | write `variables.go:355`; reveal needs `secrets:read` `variables.go:28` | | yes | |
| Org domain resource | write + managed 409 `web/org/settings.go:529-533,573-576` | write + `rejectManagedOwner` `domainresources.go:75-79,127,153` | wizard `/setup/domain` `server.go:243` same func | yes | |
| Server domain resource | admin route `server.go:353-354` | admin `resolveResourceTenancy` `domainresources.go:61` | | yes | |
| Org registry credential create / delete, tag delete | owner `ownedSettingsOrg` `web/org/registry.go:166,193,218` | write `requireOrgRegistry(true)` `api/v1/registry.go:26-34`, `deleteRegistryCredential` :101 | registry token realm: credential secret + org slug `web/registrytoken.go:72-84` | no | A [SEC] |
| Org registry images / tags list | any `settingsOrg` `web/org/registry.go:33,71` | any `requireOrgRegistry(false)` :124,169 | | yes | |
| Org config binding (repo, branch) | owner `ownedSettingsOrg` `web/org/config.go:51` | -- | | n/a | |
| Org plan: plan / preview / list / approve / reject | owner `ownedSettingsOrg` via `loadOrgPlan` `web/org/config.go:113`, `Plans` `web/org/plans.go:38` | write `api/v1/orgconfig.go:28,55,111`; list any :86 | prhook `planConfigs` :250 unauthenticated beyond HMAC | no | known A |
| Org export config | any `settingsOrg` `web/org/registry.go:293` | any `requireOrg` `config.go:294` | | yes | |
| Org GitHub connector connect / delete | write `web/settings/handler.go:441,513` | -- | callback `GitHubCallback` :464 trusts `gh.Complete` state, no org re-check (not checked further) | n/a | |
| Org backup destination create / delete | write `destOrg` `web/settings/handler.go:160` | write `api/v1/backups.go:70,98`; global ones admin :65,95,425 | | yes | |
| Move stack between orgs | write in both orgs `web/org/handler.go:160,170`; the control is shown on `IsOwner` of the *active* org `web/project/handler.go:1782` | -- | | n/a | |
| Create stack | write on chosen org `web/project/handler.go:157` | write via `orgForCreate` `api/v1/helpers.go:27-90` (drops drafts) | | yes | |
| Delete / rename stack, PR env settings, stack vars, stack domain resource, staging | write `loadStack` `web/project/handler.go:89-97` (all POSTs in the project map route through it) | write `requireStackAccess` + `requireOrgWrite` `stacks.go:57`, `lifecycle.go:343,400`, `variables.go:250`, `domainresources.go:91` | | yes | |
| Stack plan: plan / preview | write `loadStack` `web/project/handler.go:748` | write + bound `requireConfigStack(true)` `config.go:100,130`; preview registered under `config:read` `v1.go:281` | | role yes, scope no | note |
| Stack plan: approve / reject | write `loadPlan` :827-838 (plan must belong to stack) | write + bound `requirePlan(true)` `config.go:169,189` | prhook `maybeAutoApply` :334 applies on policy auto, HMAC only | yes | |
| Env create / delete / reset / copy / settings / vars | write `loadStack` :305,363,409, `compareEnv` `envcompare.go:64`, :2542,2578 | write `requireEnvWrite` `envs.go:59,92,119,163`, `putEnvVars` `variables.go:301` | | yes | |
| Tile create / patch / delete / deploy / run / stop / restart / rollback / cron | write `load` `web/app/handler.go:430-438`, `loadTile` `web/deployment/handler.go:22-30` | write `requireTile(true)` `apps.go:178,464,481,498,521`, `lifecycle.go:31,49,77,103` | | yes | |
| Cancel deployment | write `loadDeployment` `web/deployment/handler.go:50` | read `loadDeployment` `apps.go:575` (`requireTile(false)`) | | no | known A [SEC] |
| Deployment detail / status / stream (SSE) | read via `loadDeployment` :109-160; skipped entirely when the tile row is gone (`if app != nil` :49) | read `apps.go:575` | stream topic `deploy-status:<id>` `infra/deploy/engine.go:336-528`, served only through the gated handler | yes | note |
| Tile logs / metrics | read `load` `web/app/handler.go:438` | read `logs.go:17`, `lifecycle.go:132` | | yes | |
| Tile variables | write `load`; reveal `CanWriteOrg` `web/app/handler.go:1076` | write `variables.go:85`; reveal `secrets:read` :28 | | yes | |
| Tile domains, volumes, provisions, attach, detach, storage | write `load` + `RequireUnmanaged` `web/app/storage.go:78,121` | write `requireTile(true)` `domains.go:36,96`, `volumes.go:40,90`, `databases.go:277,323`, `lifecycle.go:255,280` | | yes | |
| Volume / bucket files, table rows | write `load` via `loadVolumeTile` `web/app/files.go:16`, `loadData` `web/db/data.go:59`, `loadBucket` `web/db/files.go:111` | -- | | n/a | |
| Slices: create / fork / delete / public | write `load` `web/db/handler.go:48` | write on instance tile `resolve.go:86,138,162`, `lifecycle.go:280`; target env read only `resolve.go:102` | | yes | |
| Backups: create / save / delete / run / restore | write `loadTile` `web/backups/handler.go:85`, `loadBackup` :97 | write `requireTile(true)`, `requireBackup(true)` `backups.go:119-135,155,231,289,330,349` | | yes | |
| Server storage, proxy config, external registries, users, nodes, containers | admin route `server.go:330-354,473-502,549-558` | admin `adminOnly` `v1.go:184-187,248-258`; server settings `settings.go:39` | agent api: shared bearer key `infra/agent/server.go:100-108`; node join one-time key `web/server/nodes.go:108-154` | yes | |
| Port-forward tile | -- | write + `tiles:forward` `forward.go:37`, `v1.go:346` | cli `forward` | n/a | |
| Mint / revoke share link | write `web/project/handler.go:2347,2421` | -- | public `/s/:token` `server.go:674-675`, `sharelink.Open` :77 (state + expiry), passphrase `Unlock` :90, burn or window `Reveal` :164 | n/a | note |
| Mint API key | self, write scopes need `CanGrantWrite` `web/account/handler.go:201-205` (active org role) | -- | cli browser flow `web/cli/handler.go:96-100` same rule copied; `/cli/exchange` single-use code :139-150 | yes (copies) | |
| Live change pings (websocket) | `/ws` registered on `e`, outside `site` `server.go:128`; hub built with no session or subject func `cmd/stackrd/main.go:297`; join any room by name :305-307 | -- | rooms `project:<id>`, `org:<id>`, `containers`, `server:local`, `notifications`, `flows` `handlers/notify/notify.go:22-56`; payload is the event kind only :141 | n/a | [SEC] low |
| Webhooks | -- | -- | HMAC per stack secret `web/prhook/handler.go:99`, per connector :137; empty secret refused :130, :730 | n/a | |


## 2. Findings

**[SEC] A: deactivating a user does not close their access.** `ToggleUserActive` `web/settings/handler.go:563-580` flips `User.Active` and nothing else. `Active` is checked only in `AuthService.Authenticate` `service/auth.go:117`, so a fresh login fails. The browser session loader `server.go:162-164` returns the user row regardless, and hamr's `Load` only clears the cookie on a nil subject (`hamr/pkg/middleware/auth.go:95-101`), so an existing session runs until it expires. `KeyAuth` `api/v1/auth.go:163-166` checks only that the user row exists. Scenario: an admin disables a departed contractor; the contractor's CLI key keeps deploying and reading secrets indefinitely, and their open browser tab keeps working. No session or key revocation is called from the toggle. Not runtime-verified.

**[SEC] A: org registry credentials, owner on web, member on API.** Web `CreateRegistryCredential`, `DeleteRegistryCredential`, `DeleteRegistryTag` are owner-only (`web/org/registry.go:166,193,218`). API `createRegistryCredential` goes through `requireOrgRegistry(true)` which is `requireOrgWrite` (`api/v1/registry.go:26-34`), `deleteRegistryCredential` likewise (:101), `deleteRegistryTag` :200. A member with a `stacks:write` key can mint a long-lived pull/push credential for the org's registry, which the panel says only an owner may do, and can delete tags (including the system push credential guard is API-only at :104-107, web not checked).

**[SEC] A: cancel deployment, read gate on the API.** Already in the orgs shard (`apps.go:570-579` vs `web/deployment/handler.go:50`). Restated here because it is the one live case of the pattern below: a viewer whose key carries `apps:deploy` (minted while their cookie pointed at an org where they can write, `web/account/handler.go:201`) can cancel production deployments in the org where they are a viewer.

**[SEC] low: websocket hub is unauthenticated and rooms are joinable by name.** `/ws` is registered on the bare echo instance `server.go:128`, before `auth.Load()` :172, and the hub has no `WithSubjectIDFunc` (`cmd/stackrd/main.go:297-311`); the on-message handler joins whatever room the client names (:307). Payloads are event kinds with nil data (`notify.go:141`), so what leaks is activity timing per `project:<id>` / `org:<id>` and the global `containers` / `server:local` / `flows` rooms, not content. Anyone who can reach the panel port learns when any stack deploys or changes. Ids are uuids, so per-project rooms need a leaked id; the global rooms do not.

**A: API key write scopes are bounded by the wrong org.** `CanGrantWrite` `web/account/handler.go:260` and the CLI copy `web/cli/handler.go:96` read `CanWrite`, which is the *active cookie org* (`orgctx.go:148-154`). Keys are user-scoped, not org-scoped (`repo.APIKey` has no org column, `web/cli/handler.go:153-160`). A user who is member of A and viewer of B mints write scopes with the cookie on A and uses them on B. The design compensates with a live role check per write (`requireOrgWrite`), which holds everywhere except `cancelDeployment`. Also: demoting a member to viewer leaves their write scopes in place; only the live check stops them. Every future API write that forgets `requireOrgWrite` is a viewer-write hole, because `requireStackAccess` :276 and `requireEnvAccess` `resolve.go:197` are tenancy-only and their names do not say so.

**A: preview registered under a read scope, gated as a write.** `POST /stacks/:id/config/plan-preview` and `/orgs/:id/config/plan-preview` carry `config:read` (`v1.go:244,281`) but the handlers demand a write role (`config.go:130` via `requireConfigStack(true)` :39-52; `orgconfig.go:55`). A viewer with a `config:read` key gets 403 where the catalogue text promises "Read config-as-code plans". Harmless direction (stricter than declared) but the scope catalogue lies.

**A: wizard steps mix owner and member gates.** `GET /orgs/:slug/setup/:step` and `POST /setup/done`, `/setup/mode`, `/setup/config`, `/setup/name`, `/setup/team/*` are owner (`web/org/setup.go:93,171,416`, `config.go:51`, `members.go:68,235`), but `/setup/domain` is member (`web/org/settings.go:529`) and `/setup/connector` is member (`web/settings/handler.go:441`). A member of a draft org cannot see the wizard yet can post its domain and connector steps, because `setupOpen` (`setupgate.go:403-405`) waives the draft gate for the whole `/orgs/:slug/setup` prefix and the handlers below it decide the role individually.

**A: orphan deployment rows lose their gate.** `web/deployment/handler.go:44-52`: when the tile row is gone (`app == nil`) the tenancy check is skipped and Detail, Status, Stream and Cancel proceed on the deployment id alone. API `loadDeployment` `apps.go:575` 404s instead (`requireTile` fails on a missing tile). Deleting a tile does not delete its deployments (not checked); if that holds, any logged-in user with an old deployment id can read its logs and status across orgs. Not runtime-verified.

**Note: share links outlive their minter and their org.** `sharelink.Reveal` :164-201 reads live variables by `OwnerKind` + `OwnerID`. Nothing re-checks that `CreatedBy` is still a member, or that the owning stack is still in the org it was in at mint (`MoveStack` `web/org/handler.go:148` does not touch `secret_links`). Expiry (`ExpiresAt`, `Dead()`), burn, window and lockout are enforced (:77-115). Revoke requires write in the stack's *current* org (`web/project/handler.go:2421`). Recorded, not flagged: the token is a bearer credential by design.

**Note: draft-org gate lives in five places.** Web: `RequireOrgSetup` from `RequireOrgAccess` `orgctx.go:183-186`, the settings group `RequireSetupDone` `setupgate.go:328`, the `setupOpenRoutes` allow-list :395. API: `orgReady` from `requireOrg` `variables.go:328`, from `requireStackAccess` `auth.go:292`, `orgAllowed` :230, and `orgForCreate` `helpers.go:52-66`. The API `listOrgs` :109-131 is the one lister not going through `orgAllowed`.

**Note: same rule, different HTTP answer.** Web owner and admin gates answer 404 (`members.go:56`, `settings.go:225`, `server.go:197`); API answers 403 (`members.go:37`, `auth.go:210`). Web tenancy answers 404 on both surfaces. The CLI cannot tell "not yours" from "not allowed" on the web-derived paths (invite join, share) but can on the API.

**Duplication (record only).** Owner check: `ownerOf` `web/org/members.go:31`, `ownedSettingsOrg` `settings.go:217`, `requireOrgOwner` `api/v1/members.go:26`. Write check: `CanWriteOrg` `orgctx.go:206`, `requireOrgWrite` `auth.go:300`, `orgForCreate` `helpers.go:35`. Tenancy: `InOrg` :158, `orgMember` :217. Admin: `adminOnly` twice (`server.go:194`, `auth.go:207`). Scope grant rule: `web/account/handler.go:201-205` and `web/cli/handler.go:96-100`. Managed lock: `RequireUnmanaged` `managed.go:442`, `rejectManaged` `errors.go:23`, `rejectManagedOwner` `domainresources.go:166`, plus inline `ConfigManaged()` at ~20 web sites (see the sweep map: `web/project/handler.go:311,376,422,586,603,853,1708,1716,2125,2469,2503`, `web/org/settings.go:129,533,576`, `web/org/registry.go:262`, `web/app/handler.go:1239,1258`).

## 3. Service implications

An `AccessService` (name open for 03) has to own the answer to "may this principal do this verb on this resource" so that no handler on any surface computes a role again. What it must own, from the rows above:

1. **One principal type for both surfaces.** Session user and API key resolve to the same struct: user, admin flag, org roles (loaded once, as `OrgContext` :58 and `KeyAuth` :169-179 already do separately), and for keys the scope list. `Active` is checked at resolution, not only at login (finding 1). The web subject loader and `KeyAuth` become two adapters over one resolver.

2. **Resource-rooted checks, never active-org checks.** Every method takes the resource (org, stack, env, tile, deployment, backup, provision, domain, plan, link) and walks up to its org itself, the way `requireTile` :313 and `RequireStackAccess` :258 do today. `ReadOnlyGuard` :268 and `CanWrite` :148 go away or shrink to a UI hint; `IsOwner` at `web/project/handler.go:1782` is the kind of active-org read that must not survive.

3. **Levels as an enum, one ladder.** `Read` (member), `Write` (owner or member), `Owner`, `Admin`, plus `Draft` handling as a flag on the org, so the five draft-gate copies become one branch. The wizard allow-list (`setupOpenRoutes`) becomes a per-operation attribute, not a path prefix.

4. **Per-operation table, not per-handler calls.** The verbs in section 1 map to a level once: `registry.credential.create = Owner`, `deployment.cancel = Write`, `orgplan.approve = Owner`, `orgdefaults.set = Owner`. The API and web handlers both call `Access.Require(ctx, principal, verb, resource)`; the drift rows in this shard and the orgs shard are exactly the places where the two surfaces picked different levels for one verb.

5. **Scopes are enforced inside the same call.** The verb table carries the scope too, so `preview` cannot be registered under `config:read` while gating as a write (finding 6), `secrets:read` is applied by the variable service rather than by `variables.go:28` inline, and a key's scopes are validated against the principal's *current* roles in the *target* org at use time, which is what finding 5 relies on.

6. **Revocation is a service concern.** Deactivate user, remove member, demote member, move stack: each is an event the access layer reacts to (sessions, keys, share links, websocket rooms). Today nothing reacts (findings 1, 8).

7. **Websocket room membership goes through it.** `JoinRoom` (`main.go:307`) asks `Access.CanRead(principal, room resource)`; the hub gets a subject func wired to the session cookie so it has a principal to ask about.

8. **One error policy.** 404 for "not yours", 403 for "yours but not at this level", 409 for draft and managed, decided in the service; the surfaces only translate (HTML redirect vs JSON), as `SetupPending` `setupgate.go:380` already sketches.

Call sites that stop deciding: every gate helper listed in section 0, `orgForCreate`, `requireConfigStack`, `requireBackup`, `resolveResourceTenancy`, `loadStack` / `load` / `loadTile` / `loadDeployment` / `loadBackup` / `settingsOrg` / `ownedOrg` / `ownedSettingsOrg` / `loadOrg` on the web side, and the two scope-grant copies.

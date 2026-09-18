# Shard: orgs + members + auth (lens 1)

Read-only sweep, 2026-09-18. Paths are under `internal/stackrd/` unless they start with `internal/cli/`. `w:` web, `a:` api, `c:` cli, `k:` config. `--` means the surface has no implementation. Rows the calibration set already covers are not repeated.

## 1. Operation table

| operation | web | api | cli | config | shared | gate | logic in handler | side effects | return shape | class |
|---|---|---|---|---|---|---|---|---|---|---|
| Create org (draft) | `handlers/web/handler/org/handler.go:55` `Create` | -- | -- | -- | web only | w: route `adminOnly` `handlers/web/server.go:310` + inline `IsAdmin` re-check `handler.go:68` | w: mode ∈ {config,ui}; reuse newest unfinished owner draft (`unfinishedDraft` :108) instead of a second row; placeholder name `setupDraftName` `setup.go:398`, random slug `draftSlug` :135; creator becomes owner | w: store CreateOrg + UpsertOrgMember(owner) :90-97; org cookie `setActive` :190 | n/a | |
| Pick setup branch | `org/setup.go:415` `SetupMode` | -- | -- | -- | web only | w: `ownedSettingsOrg` (owner) | w: mode ∈ {ui,config}; switching to ui rejects the pending org plan and clears the binding :428-435 | w: store SetOrgConfigPlanStatus, UpdateOrg | n/a | |
| Finish setup (activate) | `org/setup.go:170` `SetupDone` | -- | -- | -- | web only (only writer of `setup_done_at`) | w: owner | w: refuse while name is placeholder :179; `ensureDefaultDomain` :208 creates `<slug>.<root>` org domain resource when none visible (`envops.VisibleDomainResources`) | w: store UpdateOrg, CreateDomainResource | n/a | |
| Discard / delete org | `org/members.go:164` `Delete` | -- | -- | -- | web only | w: owner (`ownedOrg` :45); typed-slug confirm only for finished orgs :179-183 | w: refuse if stacks remain :184-197; refuse last org unless draft :205; remove buildkit builder `deploy.BuilderFor` :214 | w: store DeleteOrg; runtime `RemoveBuilder` :214 | n/a | |
| Rename org | `org/members.go:66` `Rename` (also wizard name step, `server.go:235`) | -- | -- | `config/orgconf/orgconf.go:424-443` (plan), `:660-673` (apply) | separate copies | w: owner; k: plan approval (`Runner.Apply`) | w: non-empty, `Slugify`≠"" :90-95, slug collision :96, **domain anti-squat over other orgs' resources :101-116**, registry-images guard :122-129, logo replace/remove :136-147. k: slug collision :430,440, registry-images guard :429,664; **no anti-squat check**; name applied verbatim from `org:` | w: store UpdateOrg + file storage. k: store UpdateOrg | n/a | A |
| Switch active org | `org/handler.go:138` `Switch` | -- | -- | -- | web only | w: `InOrg` | cookie only | cookie | |
| Move stack between orgs | `org/handler.go:148` `MoveStack` | -- | -- | -- | web only (overlaps stacks shard) | w: `RequireOrgWrite` on both orgs :160,170 | slug collision in target :179 | store SetStackOrg | n/a | |
| List orgs | `middleware/orgctx.go:58` `OrgContext` (per request) | `api/v1/slugpath.go:109` `listOrgs` | `internal/cli/cmd/lifecycle.go:542` `org ls` | -- | separate copies | none | w: prefer finished org as active, honour cookie unless it names a draft :81-104. a: admin→all, else memberships; per-row `GetOrgMember` for role :124-127; **drafts not filtered** while `orgAllowed` :230 drops them from every other listing | none | w ctx `[]repo.Org`+role; a `orgOut{ID,Slug,Name,Role}` | |
| Invite member (by email) | `org/members.go:234` `AddMember` (+ wizard `server.go:244`) | `api/v1/members.go:61` `addMember` | `internal/cli/cmd/members.go:57` `members add` | -- (by design) | separate copies of `newInvite`: w `members.go:259`, a `members.go:210` | w: owner (`ownedOrg`); a: owner (`requireOrgOwner` :26, also `orgReady` 409 on drafts via `requireOrg`) | w: bad role→member :241, email required :245, expiry 1..365 else 14 (`settings.go:598`). a: email required :70, `validRole` :232, **409 if already a member :77-86** (web has no such check), expiry ≤0→7 days :211, **no upper bound** | w: store CreateInvite **+ mail send** `mailInvite` :285 (+`MailFailed` flag). a: store CreateInvite only | w flash; a `inviteOut{ID,Email,Role,ExpiresAt,URL,Used}` | B |
| Mint invite link (open or restricted) | `org/members.go:391` `CreateInvite` | `api/v1/members.go:175` `createInvite` | `internal/cli/cmd/members.go:215` `invites add` | -- | separate copies | owner both | same as row above minus the member-exists check on either side; default expiry w 14 vs a 7 | w: store + mail; a: store only | as above | B |
| Change member role | `org/members.go:318` `SetMemberRole` | `api/v1/members.go:94` `setMemberRole` | `internal/cli/cmd/members.go:97` `members set` | -- | separate copies incl. last-owner guard (w `lastOwner` :377, a `lastOwnerGuard` :145) | owner both | w: **bad role → 400 :327**; a: **bad role → silently `member` :108**. Both: 404 unknown member, refuse demoting last owner (w fails safe on list error :380, a returns error :150) | store UpsertOrgMember | w flash; a `memberOut` | A |
| Remove member | `org/members.go:347` `RemoveMember` | `api/v1/members.go:121` `removeMember` | `internal/cli/cmd/members.go:132` `members rm` (client confirm :148) | -- | separate copies | owner both | w: **no 404 when member missing**, deletes anyway :355-360; a: 404 :128. Last-owner guard both | store DeleteOrgMember | w flash; a 204 | A |
| List members | `org/settings.go:161` `SettingsMembers` | `api/v1/members.go:41` `listMembers` | `internal/cli/cmd/members.go:22` | -- | separate | w: any member (`settingsOrg`, GET); a: any member | w merges members+invites into `peopleRows` `members.go:492`; invites only shown to owners :173; admin-only `addCandidates` roster `settings.go:195` | none | w `personRow`; a `memberOut` | |
| List invites | `org/settings.go:161` (same page) | `api/v1/members.go:159` `listInvites` | `internal/cli/cmd/members.go:175` | -- | separate | owner both | w computes pending/expired state :502-505; a exposes `Used` only, no expired flag | none | differ | |
| Revoke invite | `org/members.go:422` `DeleteInvite` | `api/v1/members.go:192` `deleteInvite` | `internal/cli/cmd/members.go:256` | -- | separate | owner both; both check `inv.OrgID == o.ID` | none beyond gate | store DeleteInvite | flash / 204 | |
| Resend invite mail | `org/members.go:436` `ResendInvite` | -- | -- | -- | web only | owner | refuse when no mailer :441, expired :444 | mail | flash | |
| Re-invite (fresh token) | `org/members.go:460` `ReinviteMember` | -- | -- | -- | web only | owner | new row before old delete :465-471 | store CreateInvite+DeleteInvite, mail | flash | |
| Accept invite / join org | `handlers/web/handler/auth/invite/handler.go:47` `Page`, `:61` `Submit`, `:118` `join` | -- | -- | -- | web only | none (rate limit `server.go:662`) | live check unused+unexpired :36; restricted-address match :79,120; inline password rules **len≥8 + confirm :91-96**; register via `AuthService.Register` :100; upsert membership only if absent :124 (existing member keeps old role); mark used; set org cookie | store UpsertOrgMember, MarkInviteUsed; session create | redirect | A |
| Register (first boot) | `handlers/web/handler/auth/register/handler.go:62` `Submit` | -- | -- | -- | `service/auth.go:34` `AuthService.Register` (also used by invite) | `firstBootOnly` `server.go:654-656` (`CountUsers`) | w: `validate.PasswordStrength` :44 + confirm :45-51. svc: email-taken, hash, **first user becomes admin :47-50** | store CreateUser; session | redirect `/setup` | A |
| Login | `auth/login/handler.go:53` `Submit` | `api/v1/auth.go:149` `KeyAuth` (per request) | `internal/cli/login.go:26` `LoginWithKey`, `:37` `LoginBrowser` | -- | w via `AuthService.Authenticate` `service/auth.go:104`; a own copy (key hash lookup) | rate limit `server.go:635` | w: lowercase email :58; svc: active check. a: hash lookup, load user, load org ids for non-admin :169-179 | w: session row + cookie; a: none | n/a | |
| Logout | `auth/login/handler.go:92` `Logout` | -- | `internal/cli/cmd/auth.go:45` (local file only) | -- | separate (different things) | -- | w: delete session row before clearing cookie :98-104 | session delete | redirect | |
| Change password | `handlers/web/handler/account/handler.go:114` `ChangePassword` | -- | -- | -- | `AuthService.ChangePassword` `service/auth.go:76` (verify current, hash) | self | w handler: **len≥8 + confirm :121-128**; service has no strength rule | store UpdateUser | flash | A |
| Edit profile (name, email, avatar) | `account/handler.go:72` `SaveProfile` | -- | -- | -- | web only | self | email required, **trimmed but not lowercased :78** while login/register/invite lowercase (`login/handler.go:58`, `register/handler.go:67`, `invite/handler.go:74`) and `GetUserByEmail` is exact match (`store/repo/sqlite/users.go:29`); collision check :83-87 | store UpdateUser, file storage | flash | A |
| Create API key | `account/handler.go:194` `CreateAPIKey` | -- | `handlers/web/handler/cli/handler.go:86` `Approve` + `:139` `Exchange` (called by `internal/cli/login.go:107` `exchange`) | -- | **two copies**: scope filter `account:201-209` vs `cli/handler.go:96-104`; grant rule `account.CanGrantWrite` :260 vs inline `cli/handler.go:55,96`; token mint `"sk_"+uuid` `account:214` vs `cli:153` | w: `ReadOnlyGuard` exempts `/account/` `orgctx.go:281`; write scopes need `CanWrite` on the **active** org (cookie) not per-org | drop invalid/unauthorised scopes silently; ≥1 scope; cli: one-time code TTL 2m in memory :28, key minted only at exchange | store CreateAPIKey; w one-shot cookie :156 | w page; cli JSON `{key}` | |
| Revoke API key | `account/handler.go:232` `DeleteAPIKey` | -- | -- | -- | web only | owner of key or admin :234 (`ownsAPIKey` lists all keys :244) | none | store DeleteAPIKey | flash | |
| List API keys | `account/handler.go:141` `APIKeys` (`myKeys` :178 filters `ListAPIKeys` client-side) | -- | -- | -- | web only | self | none | none | page | |
| Grant/revoke server admin | `handlers/web/handler/settings/handler.go:528` `ToggleUserAdmin` | -- | -- | -- | web only | route adminOnly (not checked) | last-admin guard :534-546 | store UpdateUser | flash | |
| Enable/disable user | `settings/handler.go:563` `ToggleUserActive` | -- | -- | -- | web only | adminOnly | admin cannot be disabled :569 | store UpdateUser | flash | |
| Notification prefs / theme / graph prefs | `account/handler.go:280`, `:320`, `:353` | -- | -- | -- | web only | self | theme whitelist :326; prefs JSON ≤4KB :358-364 | store UpdateUser | flash / 204 | |
| Org access check (tenancy + setup gate) | `middleware/orgctx.go:179` `RequireOrgAccess`, `:220` `RequireOrgWrite`, `setupgate.go:113` `RequireOrgSetup` | `api/v1/variables.go:319` `requireOrg`, `auth.go:217` `orgMember`, `:257` `orgReady`, `:300` `requireOrgWrite` | via api | -- | separate copies | -- | w: 404 not member; draft → `SetupPending` unless wizard route (`setupOpenRoutes` :96); write = owner/member; admin bypass. a: 404 not member; draft → 409; write = owner/member; admin bypass. Same rule, two implementations | none | -- | |
| Org write role check | `orgctx.go:206` `CanWriteOrg` | `auth.go:300` `requireOrgWrite` | -- | -- | separate copies (identical rule) | -- | owner/member or admin | none | -- | |
| Org owner check | `org/members.go:31` `ownerOf` | `api/v1/members.go:26` `requireOrgOwner` | -- | -- | separate copies | -- | owner or admin | none | -- | |
| Bind org config repo | `org/config.go:50` `SaveOrgConfig` (+ wizard `server.go:238`) | -- (panel-owned by design) | -- | -- | web only | owner | connector must belong to org :61-64; repo url normalised :65; then plans immediately via `orgcfg.Plan` :80 | store UpdateOrg; plan row via runner | flash/redirect | |
| Plan org config | `org/config.go:50` (same handler, `plan_only`) | `api/v1/orgconfig.go:22` `planOrg` | `internal/cli/cmd/org.go:56` `org plan` | -- | `orgconf.Runner.Plan` `orgconf.go:314` shared | w: **owner** (`ownedSettingsOrg`); a: **`requireOrgWrite` = member** :28 | error mapping only | plan row | w redirect; a `planOut` | A |
| Preview org plan | -- | `api/v1/orgconfig.go:50` `previewOrgPlan` | `internal/cli/cmd/org.go:22` `org preview` | -- | `Runner.PreviewBundle` :345 | a: member write :55 + bound :58 | 2MB body cap; `main` required | none | `planDetailOut` | |
| List org plans | `org/plans.go:36` `Plans` | `api/v1/orgconfig.go:85` `listOrgPlans` | `internal/cli/cmd/org.go:79` | -- | separate | w: **owner** :38; a: **any member** (`requireOrg` only) :86 | w also folds in every stack's plans :54-71 | none | w `planGroup`; a `planOut` | A |
| View one org plan | `org/plans.go:77` `OrgPlanView` | -- (no read-one route, see surface-parity) | `org approve` confirms blind `cmd/org.go:110-116` | -- | -- | owner | -- | none | page | |
| Approve org plan (apply) | `org/config.go:125` `ApproveOrgPlan`, `:203` `approvePlan` (+ wizard `setup.go:253`) | `api/v1/orgconfig.go:117` `approveOrgPlan` | `internal/cli/cmd/org.go:106` (client-side confirm) | `Runner.Apply` `orgconf.go:635` | apply shared | w: **owner**; a: **member write** (`requireOrgPlan` :111) | both: status must be pending. w: apply error → flash + 200 redirect :217; a: 422 :124 | runner: UpdateOrg (rename, defaults, env colours, settings), proxy `Resync` :700-704, domain rows :799, stacks, storage, vars | w redirect; a `planOut` | A |
| Reject org plan | `org/config.go:140`, `:222` `rejectPlan` | `api/v1/orgconfig.go:132` | `internal/cli/cmd/org.go:135` | -- | separate (one store call) | w owner; a member write | pending check both | store SetOrgConfigPlanStatus | -- | A |
| Set plan input (missing org var) | `org/config.go:156` `SetPlanInput` | -- | -- | -- | web only (overlaps vars shard) | owner | name regex `settings.go:411`, value required; re-plans | store UpsertVariable, audit, `deploy.ClearWaitingOrg`, runner Plan | redirect | |
| Set org defaults (settings cascade) | `org/registry.go:257` `SaveDefaults` | `api/v1/settings.go:114` `patchSettingsFor("org")` | `internal/cli/cmd/defaults.go:83` `org defaults` | `orgconf.go:687-694` (apply), validated `:184` | `settings.Merge`+`Check` shared; the write and the proxy resync are copied (w :279-286, a :146,160, k :694,700) | w: **owner** + **409 when `ConfigManaged` :262**; a: **`requireOrgWrite` = member** `settings.go:53`, **no managed check** | same validation via `settings.Check` | store UpdateOrg + proxy `Resync` on all three | w flash; a `settingsOut` | A |
| Set org env colour defaults | `org/settings.go:123` `SaveEnvColor` | -- | -- | `orgconf.go:173-177` validate, `:680-683` apply | separate; both use `envcolor.Valid` | w: owner + 409 when managed :129 | slug via `Slugify`; delete on empty | store UpdateOrg | flash | |
| Export org config | `org/registry.go:293` `ExportConfig` | `api/v1/config.go:293` `exportOrgConfig` | `internal/cli/cmd/defaults.go:206` `org export` | `orgconf.ExportYAML` `export.go:129` | shared | any member both | none | none | yaml both | |
| Panel upgrade (existing service) | `service/admin.go:120` `Upgrade` | -- | -- | -- | `AdminService` | -- | in service already | cluster pull, runtime tag, backups archive, `UpdateServiceImage` with `swarm.UpdateConfig` :156 | -- | D |

## 2. Candidate services

Ranked by bugs and call sites.

**MemberService** (owns members + invites; 2 B, 3 A)
- `Invite(ctx, org, actor, email, role string, days int) (*repo.Invite, error)` — one role whitelist, one expiry rule (default + cap), one "already a member" check, mails when a mailer is configured (the API today never mails).
- `Resend(ctx, org, inv)`, `Reinvite(ctx, org, inv, days)`, `Revoke(ctx, org, id)`.
- `SetRole(ctx, org, userID, role) error` — strict role validation, last-owner guard.
- `Remove(ctx, org, userID) error` — 404 on unknown, last-owner guard.
- `Accept(ctx, inv, user) error` — membership upsert, mark used (from `invite/handler.go:118`).
- Calls: MailService (infra/mail), store. Callers: web `org/members.go`, `auth/invite`, api `members.go`.

**OrgService** (create/draft/activate/rename/delete/defaults; 3 A)
- `CreateDraft(ctx, creator, mode) (*repo.Org, error)` — draft reuse, placeholder, owner membership.
- `Rename(ctx, org, name string) error` — slug rule, collision, **anti-squat**, registry-images guard; used by web Rename and `orgconf.Apply` (which today skips anti-squat).
- `Delete(ctx, org) error` — stacks-empty, last-org rule, builder removal (runtime).
- `FinishSetup(ctx, org) error` — placeholder refusal, default domain resource (calls DomainService).
- `SetDefaults(ctx, org, vals url.Values) error` — `settings.Merge/Check`, managed-org refusal, proxy resync (calls ProxyService). Same body for stack/env levels belongs to the settings shard.
- `SetEnvColor`, `Bind(ctx, org, connectorID, repo, branch, path)`.
- Calls: ProxyService, DomainService, RegistryService (`OrgHasImages`), runtime (builder). Callers: web `org/*`, api `settings.go`, `orgconf.Apply`.

**OrgPlanService** (thin: gate + status transitions around `orgconf.Runner`; 2 A)
- `Plan(ctx, org, actor)`, `Approve(ctx, org, plan, actor)`, `Reject(...)`, `List`, `Get` — one authorisation rule (owner today on web, member on API).
- Calls: `orgconf.Runner`, OrgService (rename/defaults path inside Apply should route through it).

**AccessService** (tenancy + roles; today four copies)
- `IsMember(ctx, user, orgID)`, `CanWrite(ctx, user, orgID)`, `IsOwner(ctx, user, orgID)`, `Ready(org) error` (draft gate) — replaces `orgctx.go:206,220`, `auth.go:217,257,300`, `members.go:26` (api), `org/members.go:31` (web).
- Callers: every handler; middleware wrappers stay thin.

**AuthService** (exists, `service/auth.go`; extend; 3 A)
- Add `NormalizeEmail(string) string` and `ValidatePassword(string) error` so register (`validate.PasswordStrength`), invite (`len≥8`), change-password (`len≥8`), and profile email (`no lowercase`) stop disagreeing.
- Add `UpdateProfile(ctx, user, name, email)` with the collision check (`account:83-87`).
- Add `SetAdmin(ctx, user, bool)` / `SetActive` with the last-admin and admin-not-disabled guards from `settings/handler.go:534-546,569`.
- Sessions stay in hamr's `SessionManager`; `KeyAuth` stays in the API but should call the same active-user rule as `Authenticate` (`!user.Active` is checked at `service/auth.go:117`, not in `KeyAuth` `auth.go:163-166` — not checked whether inactive keys are rejected elsewhere).

**APIKeyService**
- `Mint(ctx, user, name string, scopes []string, canGrantWrite bool) (raw string, *repo.APIKey, error)` — one scope filter, one token format; callers `account/handler.go:194` and `cli/handler.go:139`.
- `Revoke(ctx, user, id)`, `ListForUser(ctx, userID)` (today `ListAPIKeys` + client filter, `account:178`).
- The grant rule (`CanGrantWrite`) reads the **active org** role; a service would take an explicit org or be admin-only for write scopes — open question, not a finding.

**AdminService** (exists) — class D on `swarm` import, see findings.

## 3. Drift findings

**B — API invite never sends mail.** Web `newInvite` `org/members.go:259` calls `mailInvite` :285 and records `MailFailed`; API `newInvite` `api/v1/members.go:210-219` writes the row and returns the link. `stackr org members add` therefore silently produces an unsent invite on a mailer-configured server; the CLI text says "The printed link is a credential" (`cmd/members.go:68`) but the web flash promises email (`members.go:311`). Same for `invites add` vs `CreateInvite`.

**A — invite defaults and bounds.** Web expiry `inviteExpiryDays` `org/settings.go:598`: 1..365, else 14. API `newInvite` :211-213: ≤0 → 7, no upper bound (`expires_days: 100000` accepted). CLI help says "default 7" (`cmd/members.go:93,252`), web UI defaults to 14.

**A — "already a member" check API-only.** `addMember` `api/v1/members.go:77-86` returns 409; web `AddMember` `org/members.go:234-255` mints a second invite for an existing member.

**A — bad role handling.** Web `SetMemberRole` `org/members.go:327` → 400; API `setMemberRole` `api/v1/members.go:108` `validRole` → silently `member`. Invite creation coerces on both sides (`members.go:241` web, `:72,181` api), so only role-change diverges.

**A — remove unknown member.** API `removeMember` `api/v1/members.go:128` 404s; web `RemoveMember` `org/members.go:355-360` runs `DeleteOrgMember` regardless and flashes "Member removed."

**A — org plan authorisation.** Web plan/approve/reject/list are owner-only (`ownedSettingsOrg` `org/settings.go:217`, used at `org/config.go:52,112` and `org/plans.go:38`). API `planOrg` `api/v1/orgconfig.go:28`, `requireOrgPlan` :111 and `previewOrgPlan` :55 use `requireOrgWrite` (owner **or member**), `listOrgPlans` :86 any member. A member can apply an org plan (creates/deletes stacks, renames the org) from the CLI but not from the panel. The web `Plans` comment (`plans.go:33-34`) states owner-only is deliberate.

**A — org defaults on a managed org.** Web `SaveDefaults` `org/registry.go:262-264` 409s when `ConfigManaged()`; API `patchSettingsFor("org")` `api/v1/settings.go:47-53` has no managed check and only requires member write, not owner. The next `orgconf.Apply` (`orgconf.go:687-694`) overwrites the API's write when the file declares `defaults:`; when it declares none the API write sticks and the file is no longer the source of truth.

**A — org rename anti-squat only on web.** Web `Rename` `org/members.go:101-116` refuses a slug that another org's domain resource leads with. `orgconf` plan/apply (`orgconf.go:424-443`, `:660-673`) checks slug collision and registry images only. A config file `org:` rename can take a slug the panel would refuse.

**A — password rule in three places, none in the service.** Register `auth/register/handler.go:44` `validate.PasswordStrength`; invite join `auth/invite/handler.go:91` `len ≥ 8`; change password `account/handler.go:121` `len ≥ 8`. `AuthService.Register/ChangePassword` (`service/auth.go:34,76`) hash whatever they are given. A weaker password is accepted via invite than via first-boot register.

**A — email case.** Login, register and invite lowercase the email (`login/handler.go:58`, `register/handler.go:67`, `invite/handler.go:74`); `SaveProfile` `account/handler.go:78` only trims. `GetUserByEmail` is `WHERE email = ?` (`store/repo/sqlite/users.go:29`). A profile save of `Bob@Example.com` produces an account that `Authenticate` (`service/auth.go:105`, fed the lowercased form) cannot find. Not verified at runtime.

**Duplication, no divergence yet (record only).**
- API key minting: `account/handler.go:201-222` vs `cli/handler.go:96-108,153-162` — scope filter, `CanGrantWrite` rule and `"sk_"+uuid` format copied; `account.CanGrantWrite` :260 is exported "so the CLI authorize page applies the identical rule" but `cli/handler.go` does not call it.
- Owner / write / membership / draft gates: web `orgctx.go:206,220`, `setupgate.go:113`, `org/members.go:31`; api `auth.go:217,257,300`, `members.go:26`. Same rules, six functions.
- Last-owner guard: `org/members.go:377` vs `api/v1/members.go:145` (web fails closed on a list error, API returns the error).
- `newInvite`: `org/members.go:259` vs `api/v1/members.go:210` (see B above).

**API listing shows drafts.** `listOrgs` `api/v1/slugpath.go:109-131` returns unfinished orgs while `orgAllowed` `auth.go:230` filters them from every other listing and `requireOrg` 409s on them (`orgReady` :257). Not a class A/B/C; recorded because the org table has no other place for it.

**D — driver leak in the service layer.** `service/admin.go:16` imports `github.com/docker/docker/api/types/swarm` and builds `swarm.UpdateConfig` at `:156` for `runtime.UpdateServiceImage`. `service/` sits above `infra/`. Only import of docker types outside `infra/` found under `handlers/`, `service/`, `config/`, `internal/cli` (`grep -rln '"github.com/docker' …`, 2026-09-18).

**Existing services and who bypasses them.**
- `AuthService` (`service/auth.go`): used by `auth/login`, `auth/register`, `auth/invite`, `account.ChangePassword`. Bypassed by `account.SaveProfile` (`UpdateUser` :106, email rule), `settings.ToggleUserAdmin/ToggleUserActive` (`UpdateUser` :555,575), and `config/sharelink/sharelink.go:53,94` (hashes its own passwords with `hamr/pkg/auth`). API `KeyAuth` `api/v1/auth.go:149` is its own authenticator.
- `AdminService` (`service/admin.go`): upgrade only; consumed via `deps.Admin` in `settingspage.NewHandler` `server.go:203` (not further checked).

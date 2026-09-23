# Sweep B — panel handlers

Files read: every non-generated `.go` file under
`internal/stackrd/handlers/web/handler/{org,server,settings,app,backups,account,auth,auth/invite,auth/login,auth/register,cli,notification,search,sharepub,about}`.

| File | Lines |
|------|-------|
| `about/handler.go` | 21 |
| `account/handler.go` | 393 |
| `app/deployments_test.go` | 34 |
| `app/files.go` | 69 |
| `app/gate_test.go` | 63 |
| `app/handler.go` | 1633 |
| `app/helpers.go` | 37 |
| `app/placement_test.go` | 64 |
| `app/provisions_test.go` | 42 |
| `app/runlogs_test.go` | 30 |
| `app/storage.go` | 125 |
| `auth/cookies.go` | 41 |
| `auth/invite/handler.go` | 143 |
| `auth/login/handler.go` | 110 |
| `auth/login/logout_test.go` | 99 |
| `auth/register/handler.go` | 100 |
| `backups/handler.go` | 304 |
| `cli/handler.go` | 206 |
| `cli/handler_test.go` | 87 |
| `notification/handler.go` | 68 |
| `org/config.go` | 290 |
| `org/create_admin_test.go` | 60 |
| `org/graph.go` | 575 |
| `org/handler.go` | 219 |
| `org/members.go` | 442 |
| `org/people_render_test.go` | 180 |
| `org/plans.go` | 146 |
| `org/registry.go` | 277 |
| `org/settings.go` | 547 |
| `org/setup.go` | 546 |
| `org/setup_test.go` | 281 |
| `org/tenancy_test.go` | 153 |
| `search/handler.go` | 384 |
| `search/handler_test.go` | 30 |
| `server/confirm_test.go` | 43 |
| `server/handler.go` | 391 |
| `server/joinscript.go` | 149 |
| `server/nodes.go` | 426 |
| `server/storage.go` | 148 |
| `server/volumenode_test.go` | 66 |
| `settings/handler.go` | 741 |
| `settings/proxy.go` | 78 |
| `settings/upgrade.go` | 61 |
| `sharepub/handler.go` | 118 |
| `sharepub/text.go` | 43 |

**Already-established line numbers re-confirmed, all still accurate:**
`server/storage.go:26` (probe) and `:140` (sub-path volume cleanup);
`server/handler.go:271-320` (volume create/delete); `server/nodes.go:65`
(AddNode + EnsureAgent); `org/members.go:171-203` (org delete: stacks, last
org, draft, RemoveBuilder at `:201`); `settings/handler.go:241` (global-dest
rule), `:260` (`backup.Validate`), `:266-269` (raw `Save` + `ReloadBackups`).
Those are not repeated below.

## Findings

**Security-relevant (auth, sessions, invites, keys, admin) — read these first**

### account/handler.go:120-127 — password strength, dead pre-check
- **Rule:** a new password must be at least 8 characters and match the confirm box.
- **Category:** 6 (and 2).
- **Owner it should have:** `service.ValidatePassword` (`internal/stackrd/service/auth.go:43`), whose own doc comment names this handler as one of the three copies it was created to replace.
- **Duplicated at:** `auth/invite/handler.go:93` (`len(f.Password) < 8`), `auth/register/handler.go:44` (`validate.PasswordStrength` via `FormRules`), `internal/stackrd/service/auth.go:43`.
- **Severity:** debt. Both service entry points do enforce the real rule — `Register` at `internal/stackrd/service/auth.go:62` and `ChangePassword` at `:120` — so no path accepts a weak password. These are two dead pre-checks the service already covers; the invite copy's only live effect is to mask the real refusal (see next finding).

### auth/invite/handler.go:102-110 — a strength refusal is reported as "Registration failed"
- **Rule:** map `AuthService.Register`'s error onto a field message.
- **Category:** 3.
- **Owner it should have:** `stackrmw.HTTP` / `svcerr.Invalid` field mapping, as the rest of the panel does.
- **Duplicated at:** `auth/register/handler.go:78-86` (same shape, but that path runs `FormRules` first so it never reaches the generic branch).
- **Severity:** bug. Only `ErrEmailTaken` is named; a password that passes the handler's `len>=8` and fails `ValidatePassword` comes back as "Registration failed. Please try again." with nothing to act on.

### auth/invite/handler.go:36-46 — invite liveness, first of four spellings
- **Rule:** an invite is usable only while `!UsedAt.Valid` and `time.Now()` is before `ExpiresAt`.
- **Category:** 1, 6.
- **Owner it should have:** `MemberService` — a `LiveInvite(ctx, token)` that is the only door.
- **Duplicated at:** `org/members.go:363` (resend refuses an expired invite), `org/members.go:422` (`peopleRows` marks the row expired), `org/members.go:379` (reinvite exists *because* of the same rule). `GetInvite` itself returns dead invites to every caller.
- **Severity:** bug. Four readers of the same two columns; any surface that forgets one half accepts a spent or expired token.

### auth/invite/handler.go:120-137 — join is an un-owned three-step write
- **Rule:** re-check the invite's bound address against the session user, add the membership, burn the invite, switch the active-org cookie.
- **Category:** 3, 5.
- **Owner it should have:** `MemberService.AcceptInvite(ctx, inv, user)` returning the org; the cookie stays here.
- **Duplicated at:** the address re-check at `:122` is a third copy of the `:81` and `:38` rules.
- **Severity:** bug. `_ = h.members.UseInvite(ctx, inv.ID)` at `:129` discards the error: a `Join` that lands with a failed burn leaves a single-use invite reusable, and nothing logs it.

### account/handler.go:196-258 — API-key scope escalation rule lives in a page package
- **Rule:** a key may never carry more than the person minting it already holds (`CanGrantWrite`), and it binds to the active org unless the holder is a server admin (`MintOrg`).
- **Category:** 1, 6.
- **Owner it should have:** `service.APIKeyService.Mint` — it already owns the token format and the row; the grant ceiling is the same decision.
- **Duplicated at:** `cli/handler.go:63` and `cli/handler.go:118` both re-spell `stackrmw.CanWrite(c) || stackrmw.IsAdmin(c)` inline rather than calling the exported `account.CanGrantWrite` two files away.
- **Severity:** bug. Three live spellings of a privilege ceiling, and the `cli` package imports the `account` page package to get the fourth (`MintOrg`) — a security rule whose home is a view.

### account/handler.go:211-233 — key-revoke authorization in the handler
- **Rule:** only the key's owner, or a server admin, may revoke it.
- **Category:** 1.
- **Owner it should have:** `APIKeyService.Delete(ctx, id, actor)`.
- **Duplicated at:** `-` (but `myKeys` at `:177` repeats the same owner filter over `ListAll`).
- **Severity:** bug. `ownsAPIKey` lists every key on the server and scans in Go; the service has no owner-scoped read at all, so every caller must remember to filter.

### settings/handler.go:521-553 — "never remove the last admin", counted in the handler
- **Rule:** an admin may be demoted only while another admin exists.
- **Category:** 1.
- **Owner it should have:** `AuthService` / `AdminService` — `SaveUser`'s own comment (`internal/stackrd/service/auth.go:180`) explicitly pushes role rules back to "the admin page's own checks", which is what makes this the only copy.
- **Duplicated at:** `-`.
- **Severity:** bug. Read-then-write with no transaction, and the only enforcement point found in this shard: nothing below `SaveUser` re-checks it. Whether an API surface reaches `SaveUser` without this guard is unverified — cross-check in shard D. *Answered 2026-09-21 (`12-business-logic-plan.md` §1, O3): no API route reaches it; all six callers are in `handlers/web`. Debt, not a hole.*

### settings/handler.go:562-565 — the two admin guards disagree
- **Rule:** an account with `Role == "admin"` may never be disabled — *any* admin, not just the last one.
- **Category:** 1, 6.
- **Owner it should have:** same `AuthService` rule as above; the two should be one decision ("can this account lose its powers").
- **Duplicated at:** `settings/handler.go:539` (the last-admin count), which permits exactly what this refuses.
- **Severity:** bug. Adjacent functions hold contradictory policies: demote-one-of-two is allowed, disable-one-of-two is not.

### cli/handler.go:107-190 — the whole CLI grant protocol is handler state
- **Rule:** an approved grant is held in memory under a one-time code for 2 minutes, burned on exchange, and the key is minted only at exchange time by an unauthenticated route.
- **Category:** 3, 5.
- **Owner it should have:** needs a new `CLIGrantService` (or `APIKeyService.Grant`/`Redeem`), so the burn, the TTL and the scope set that survived the session live with the key rules.
- **Duplicated at:** `-`.
- **Severity:** debt. Correct and tested today, but `h.codes` is process-local: two panel replicas, or a restart mid-login, turn a valid exchange into a 404 with no diagnostic.

### server/nodes.go:184-192 — join-key client address read straight off X-Forwarded-For
- **Rule:** the address a one-time join key is bound to is the leftmost `X-Forwarded-For` entry, unconditionally.
- **Category:** 2.
- **Owner it should have:** `nodes.Service` / `NodeService.Redeem` should take the address the framework resolved, with the trust decision configured once.
- **Duplicated at:** `server/nodes.go:153` (ClaimNode uses the same helper — same trust).
- **Severity:** debt. Deliberate and documented (the key is the authentication, the address is a rail), but the rail is client-controlled the moment the panel is reachable without traefik in front, and the code says so nowhere a reviewer of `Redeem` would see it.

### org/settings.go:203-205 — `ownedSettingsOrg` performs no owner check
- **Rule:** none. It is a bare alias for `settingsOrg`, while its name and doc comment ("settingsOrg plus the owner check") say otherwise.
- **Category:** 1.
- **Owner it should have:** the route verbs, which is where the check really moved (`org/members.go:37-43` explains the move); the alias should be deleted, not left lying.
- **Duplicated at:** `org/members.go:41` `ownedOrg` — the same no-op alias with the same misleading name.
- **Severity:** debt. ~15 call sites — `SaveOrgConfig`, `approvePlan`, `Plans`, `SetupDone`, `SetupMode`, `DeleteRegistryCredential`, `SaveDefaults`, `DeleteOrgDomain` — read as owner-gated and are not. Any new route that forgets its verb inherits the appearance of a guard.

### org/members.go:88-117 — a second, different anti-squat rule for rename
- **Rule:** an org may not rename to a slug that leads some *other* owner's domain resource, and may not rename at all once it has pushed images.
- **Category:** 2, 6.
- **Owner it should have:** `OrgService.Rename` — the whole body from `:77` to `:135` is a service method that does not exist.
- **Duplicated at:** `service.CheckOrgSquat` (exercised by `org/setup_test.go:103`), which `org/settings.go:492` says now owns anti-squat "in the service now … squat used to be checked *here only*, which made it decorative".
- **Severity:** bug. Two implementations of one guard, asking the question from opposite ends: `CheckOrgSquat` checks a host against org slugs, this checks an org slug against hosts. Only one of them protects rename, and it is the handler's.

### org/members.go:227-230 — role whitelist coerces instead of refusing
- **Rule:** an unrecognised role silently becomes `"member"`.
- **Category:** 6.
- **Owner it should have:** `MemberService.Invite` — `org/members.go:248` already states "the role whitelist … are the service's".
- **Duplicated at:** `org/members.go:315-318` (identical four lines in `CreateInvite`).
- **Severity:** bug. The handler's coercion runs first, so the service's whitelist can never fire: a typo'd or hand-posted `role=admin` grants membership rather than a 400.

### org/settings.go:264-272 — read-only redaction of secrets in the handler
- **Rule:** a member without write rights sees a secret's name but a blanked value.
- **Category:** 1.
- **Owner it should have:** `VariableService` — a viewer-scoped `List` that cannot return a value the caller may not see.
- **Duplicated at:** `org/settings.go:316-326` `OrgVarValue` serves plaintext with no `canWrite` check of its own (it leans entirely on the route verb), i.e. the same rule, spelled once as redaction and once as absence.
- **Severity:** bug. Redaction after the fact: the values were loaded and are one forgotten branch away from a template.

**Config-managed gate and staging**

### app/handler.go:1098 — the redeploy gate re-spelled next to the service that owns it
- **Rule:** redeploy after an env save only for a running, unmanaged, non-volume `service` tile.
- **Category:** 6.
- **Owner it should have:** `DeployService.RedeployIfRunning`, which the comment two lines above says holds it "in one place, where it used to be spelled out here and in three other handlers".
- **Duplicated at:** `app/handler.go:736-740` (the attach path calls `RedeployIfRunning` with no pre-gate at all — the two callers disagree), `internal/stackrd/service/deploy.go`.
- **Severity:** bug. The claim in the comment is false in the same function: a pre-gate that differs from the service's gate is the drift the extraction was for.

### app/handler.go:1109-1158 — four doors onto one config-managed question
- **Rule:** what happens to an edit to a field the config file owns.
- **Category:** 6.
- **Owner it should have:** `GateService` — `editGate` at `:1133` already delegates correctly; the other three do not.
- **Duplicated at:** `app/handler.go:1109` `uiManaged`, `:1116` `configMode`, `:1149` `rejectManaged`, plus `app/storage.go:78` and `:110` (`stackrmw.RequireUnmanaged`) and `org/settings.go:115` (a fifth, with its own 409 wording).
- **Severity:** bug. Five refusal spellings with three different messages; `wireProvision` (`:811`) branches on `uiManaged` to decide whether a database URL is injected *at all*, which is the most consequential of the five and the one furthest from the gate.

### app/handler.go:1474-1486 — nil-vs-false HTTPS semantics written out in a handler
- **Rule:** a staged domain must carry both `HTTPS` and `ForceHTTPS` explicitly when HTTPS is off, because `DomainConf.ForceHTTPSOn` reads nil as on.
- **Category:** 1, 2.
- **Owner it should have:** `stackconf` (a `DomainConf` constructor) or `DomainService`, so one place knows what nil means.
- **Duplicated at:** `app/handler.go:1539-1550` (`ToggleDomainHTTPS` re-derives the same nil/false pair), `app/handler.go:1462` (the checkbox-to-pointer conversion).
- **Severity:** bug. The comment records that the previous version of these lines shipped a domain that redirected plain HTTP onto TLS nobody served; three surviving hand-written copies of the same nil convention.

### app/handler.go:682-707 — volume attach: rules with no owner
- **Rule:** which attach fields are config-modeled, whether the gate's refusal propagates, that a target must be a same-environment unmanaged `service`, that a mount path must be absolute, and that a bad size is warn-only.
- **Category:** 1, 2, 3.
- **Owner it should have:** needs a new `VolumeService` (the index already records that none exists for `server/handler.go:271-320`; this is the tile half of the same gap).
- **Duplicated at:** `app/handler.go:627` (`envServices` applies the same `Kind == "service" && !IsManaged()` eligibility for the dropdown).
- **Severity:** bug. The absolute-path rule at `:694` only runs when `targetID != ""`, so a detach keeps whatever `mount_path` was posted on the row; the next attach then re-validates a field that was already written.

### app/handler.go:1006-1053 — variable deletion semantics in the handler
- **Rule:** a name present in the tile's env blob is deleted by staging a blob edit, unless a secret row of the same spelling exists, in which case it deletes directly.
- **Category:** 1, 3.
- **Owner it should have:** `VariableService.Unset` (or `TileService`), which already owns the name rule (`:996`).
- **Duplicated at:** `app/handler.go:1191-1210` `currentDesiredEnv` un-marshals the staged payload by hand to answer the same question.
- **Severity:** debt.

### app/handler.go:1191-1236 — staged-payload JSON parsed in the handler
- **Rule:** the desired env/domain set is the pending staged patch when one exists, else the committed row.
- **Category:** 3.
- **Owner it should have:** `staging` / `TileService` — a `Desired(ctx, tile, group)`.
- **Duplicated at:** two near-identical bodies in the same file (`:1191` env, `:1216` domains), each with its own anonymous unmarshal struct.
- **Severity:** debt.

### app/handler.go:1365-1370 — source-type field clearing duplicated with the validator
- **Rule:** `update_policy` only applies to image sources, `wait_for_ci` only to git sources.
- **Category:** 6.
- **Owner it should have:** `TileService.Update`'s validator, which the comment says "still refuses the combination if a caller sends it outright".
- **Duplicated at:** `service` validator.
- **Severity:** debt (the comment is honest about it being form semantics; listed because it is a second enforcement point).

### org/setup.go:208-227 — `ensureDefaultDomain` writes through the raw `Save`
- **Rule:** an org that finishes setup with no visible domain resource gets one at `<slug>.<root>`, org level, undeclared.
- **Category:** 1, 3.
- **Owner it should have:** `DomainResourceService.Create` — the escape hatch skips the host shape, taken and anti-squat rules `org/settings.go:492` says that service owns.
- **Duplicated at:** `org/settings.go:492` and `server/handler.go:218` both create resources the proper way.
- **Severity:** bug. The one creation path that bypasses the validator is the one whose host is derived from `BASE_URL` rather than typed by a person. *Downgraded 2026-09-21 (`12-business-logic-plan.md` §4, B8): low. `host` is `UNIQUE` in the schema (`001_initial.up.sql:390`), so a collision fails the insert rather than double-claiming, and a host under the org's own slug cannot squat anyone.*

### org/config.go:162-167 / org/settings.go:384 — a handler-local variable-name regexp
- **Rule:** a variable name is `^[A-Za-z0-9_][A-Za-z0-9_.-]*$`.
- **Category:** 6, 2.
- **Owner it should have:** `VariableService.Set`, which `app/handler.go:996` says owns the name rule now ("This form used to accept any non-empty string").
- **Duplicated at:** `org/settings.go:384` (`varNameRe`, whose own comment admits it "mirrors the stack-variable rule"), the service's validator.
- **Severity:** bug. `SetPlanInput` checks the handler's copy and then calls `vars.Set`, so two regexes must be kept in step or a plan input is accepted that no other surface would take.

**Duplicated derivations and un-owned reads**

### org/registry.go:123-152 — `liveTags` is the buggy matcher the delete path was fixed to stop using
- **Rule:** which registry tag a live deployment still runs, keyed by the repository path.
- **Category:** 6.
- **Owner it should have:** `RegistryService.TagInUse`, which `DeleteRegistryTag` at `:210` already calls.
- **Duplicated at:** `org/registry.go:206-215` (the service version).
- **Severity:** bug. `:145` indexes the stored tag by `strings.Cut(d.ImageTag, "/")` and drops any tag stored without a pull host — the exact defect `:201-205` documents as fixed on the delete path. The Images table's "runs on" column still carries it, so a tag reads as unused while a deployment points at it.

### backups/handler.go:115-118 — backup kind derived twice
- **Rule:** a managed tile produces a dump, anything else a volume archive.
- **Category:** 6, 1.
- **Owner it should have:** `BackupScheduleService`, which the same file says owns "the kind derivation" for `Create` (`:200-203`).
- **Duplicated at:** `internal/stackrd/service/backupschedule.go` (the create path).
- **Severity:** debt. The view derives the kind independently of the service that derives it on write, so the two can disagree about a tile whose managed flag changed.

### backups/handler.go:119-122 — infra reached from the tab
- **Rule:** whether this tile has a volume worth backing up, and which org owns it.
- **Category:** 4.
- **Owner it should have:** `BackupScheduleService` / `TileService`; `backup.VolumeFor` and `backup.OrgOf(ctx, h.store, t)` are infra calls, the second taking the raw store.
- **Duplicated at:** `-`.
- **Severity:** debt.

### server/handler.go:354-376 — a second metric downsampler
- **Rule:** bucket-average raw samples into ~240 points for a chart.
- **Category:** 6.
- **Owner it should have:** `TileTelemetryService.Points`, which `app/handler.go:193` and `:509` already use for exactly this.
- **Duplicated at:** `service.TileTelemetryService.Points`.
- **Severity:** debt. The server page calls `SamplesSince` and rolls its own loop; the two will drift the moment the bucket count or the unit conversion changes.

### server/handler.go:162 — "has this server joined" asked three ways
- **Rule:** an empty `NodeID` means the row has not joined, and must not collapse onto the manager.
- **Category:** 6, 1.
- **Owner it should have:** `repo.Server.Joined()` or `NodeService`.
- **Duplicated at:** `server/handler.go:181` (Volumes skips the listing), `server/handler.go:263` (`volumeNode` refuses with 409), `server/handler.go:113` (Detail's task read). `server/volumenode_test.go:30` calls it "one condition, two callers" — there are four.
- **Severity:** debt. Three of the four refuse or skip; the fourth (`:113`) silently returns nothing.

### server/nodes.go:349-371 — placement rules recomputed in the handler
- **Rule:** a tile may move to a ready node in its placement group that is not its current home.
- **Category:** 1, 3, 4.
- **Owner it should have:** `NodeService` / `volmove.Service` — a `MoveTargets(ctx, tile)`.
- **Duplicated at:** `server/nodes.go:386-391` (`Move` re-derives the list to re-validate the posted target — correct, but the second run of the same rule).
- **Severity:** bug. `:357` swallows `placement.For`'s error and falls back to `plan.Group = t.NodeGroup`, quietly replacing the resolved placement with the tile's raw column — so a tile whose group is resolved by cascade offers the wrong targets exactly when placement is unavailable.

### server/nodes.go:109-139 — join-script assembly is orchestration
- **Rule:** redeem the key, take the swarm join token, resolve the registry pull address and whether it is TLS, and stamp the panel callback.
- **Category:** 3, 4.
- **Owner it should have:** `NodeService.JoinScript(ctx, key, addr)`.
- **Duplicated at:** `server/joinscript.go:50-74` holds the companion rule (bare `host:port` → `insecure-registries`, real domain → nothing).
- **Severity:** debt.

### org/settings.go:465-477 — "resources owned by X" hand-filtered in five places
- **Rule:** select domain resources by level and owner id.
- **Category:** 6.
- **Owner it should have:** `DomainResourceService.ForOwner(level, ownerID)`; `service.VisibleDomainResources` already exists for the visibility half (`org/setup.go:220`).
- **Duplicated at:** `server/handler.go:119-124` (`instance`/server id), `server/handler.go:238` (delete scope guard), `org/settings.go:518` (delete scope guard), `org/setup.go:141`, `org/setup.go:491` (prefill scans for `instance`).
- **Severity:** debt.

### org/settings.go:455-459 — org storage filtered in the handler
- **Rule:** the shares belonging to one org.
- **Category:** 6.
- **Owner it should have:** `StorageService.ForOrg`.
- **Duplicated at:** `server/handler.go:129-138` (the same shape over `ServerID`), `app/storage.go:33-57` (`ListAll` then filter for the picker).
- **Severity:** debt.

### org/settings.go:530-536 — invite expiry bounds clamped twice
- **Rule:** an invite lasts 1-365 days, default 14.
- **Category:** 6.
- **Owner it should have:** `MemberService.Invite` — `org/members.go:248` already says "the expiry bounds … are the service's".
- **Duplicated at:** the service.
- **Severity:** debt. Clamping before the call means the service's bound can never refuse; an out-of-range value becomes 14 days rather than an error.

### org/config.go:24-36 — "a usable GitHub connector" derived three times
- **Rule:** a connector counts only when `Provider == "github"` and `githubapp.ParseConfig(cn.Config).Connected()`.
- **Category:** 6.
- **Owner it should have:** `ConnectorService.GitHubForOrg`.
- **Duplicated at:** `app/handler.go:111`, `settings/handler.go:404-406` (`LoadOrgConnectors`).
- **Severity:** debt.

### org/config.go:206-236 — plan approval decisions in the handler
- **Rule:** only a `pending` plan may be approved or rejected; approval enqueues an apply on the work queue.
- **Category:** 1, 3, 4.
- **Owner it should have:** `PlanService.Approve/Reject`; `orgconf.EnqueueApply` is called straight from the handler with `h.work`.
- **Duplicated at:** `org/config.go:228` (reject repeats the status check), `org/plans.go:130` (`waiting` says a plan is undecided if `pending` *or* `error` — a third, wider definition of the same state).
- **Severity:** debt.

### org/config.go:57-72 — config binding validation and repo-URL normalisation
- **Rule:** the connector must belong to this org; a repo is the form value with `https://github.com/` and `.git` stripped; a bound connector requires a repo.
- **Category:** 2, 3.
- **Owner it should have:** `OrgService` / `orgconf` — a `Bind(ctx, org, spec)`.
- **Duplicated at:** the stack-level config page (shard A) almost certainly carries the twin; worth cross-checking when A lands.
- **Severity:** debt.

### org/graph.go:118, 130, 163, 175, 310, 324 — notifier fan-out fired by the caller
- **Rule:** an org-canvas write pushes a refresh to open canvases.
- **Category:** 5.
- **Owner it should have:** `GraphService` / `AnnotationService`, beside the write.
- **Duplicated at:** the index already records 11 copies in `project/stackgraph.go` and `project/handler.go`; these are six more, and `org/graph.go:97, 105, 142, 150` (the user-scoped home versions) deliberately omit it — the asymmetry is invisible at the call site.
- **Severity:** debt.

### org/graph.go:396-414 — live forward counts assembled from two infra sources
- **Rule:** a tile's forward count is the distinct `(tile, port)` union of running relay containers and open CLI sessions.
- **Category:** 4, 6.
- **Owner it should have:** `forward` / a `ForwardService.CountsByTile`.
- **Duplicated at:** the env canvas builds the same union (shard A/C).
- **Severity:** debt.

### org/graph.go:518-566 — org variable classification and usage
- **Rule:** split org variables into plain and secret and work out which stacks reference each class, reading `varref.ScopeVarsUsed` with the raw store.
- **Category:** 3, 4.
- **Owner it should have:** `VariableService` — a usage summary per scope.
- **Duplicated at:** the stack/env equivalents (shard A).
- **Severity:** debt.

### org/graph.go:29-33 — "one unfinished org" redirect
- **Rule:** a user whose single org is a draft they own is sent to the wizard instead of the root canvas.
- **Category:** 1.
- **Owner it should have:** `OrgService` (a "where does this user belong" answer), since `setupFlow` and `SetupDoneAt` are already org-service state.
- **Duplicated at:** `org/setup.go:98-106` (the inverse: a finished org on a wizard URL is bounced to settings).
- **Severity:** debt.

### search/handler.go:314-357 — canvas-shape and tile-kind rules, fifth spelling
- **Rule:** an attached volume rides under its service's card, a slice-hosting instance is stood in for by its first slice, everything else has its own; and a tile's display kind is derived from `IsManaged`/`Kind`.
- **Category:** 6, 1.
- **Owner it should have:** the `graph` package (a shared `NodeFor(tile)`), or `repo.Tile` for the kind.
- **Duplicated at:** `org/graph.go:449` and `:482` (`MapTile` / `DBNodeID`), `app/handler.go:448` `defaultTab`, `app/handler.go:457` `runsTile`, the env canvas builder (shard A).
- **Severity:** debt. Search silently drops any tile whose shape it classifies differently from the canvas it links to (`base == ""` → `continue`).

### search/handler.go:59-282 — org scoping done by post-filtering global lists
- **Rule:** a result is visible only if its org is in `middleware.Orgs(c)`.
- **Category:** 1, 3.
- **Owner it should have:** needs a new `SearchService` taking a viewer, so the scoping is one decision rather than eleven `if _, ok := orgByID[...]; !ok { continue }`.
- **Duplicated at:** eleven in-loop guards in this one function (`:100`, `:150`, `:168`, `:181`, `:200-215`, `:225`, …).
- **Severity:** debt. `tiles.ListAll`, `domains.ListAll`, `connectors.ListAll`, `schedules.ListAll` and `vars.Names` are all server-wide reads; one missed guard is a cross-org name leak, and nothing outside this function enforces the invariant.

### app/handler.go:806-839 — `wireProvision` decides whether a DB URL is injected at all
- **Rule:** on a config-managed stack the file owns `tile.Env`, so the panel prints a `uses:` snippet instead of injecting; otherwise it wires, and an s3 engine that auto-injects everything reports no variable name.
- **Category:** 1, 3.
- **Owner it should have:** `SliceService.Wire` — it already returns the used name; the config-managed branch and the engine branch belong with it.
- **Duplicated at:** `app/handler.go:850` and `:880` spell "only services and crons can consume provisions" twice in the same file; `app/handler.go:790-803` (`infraAddress`) rebuilds a managed-tile address from `envnet.Resolve` + stacks + orgs.
- **Severity:** bug. The most consequential branch in the file (does the app get its connection string or not) is a handler `if`.

### app/handler.go:427-443, 652, 164-166, 263-267 — infra called straight from the panel
- **Rule:** which node a tile's data sits on (`placement.NodeOf` with the raw store), a volume's size (`clus.InspectVolume`), where log lines come from (`clus.StreamServiceLogsMarked` / `clus.Self`), which service a run streams from (`jobs.RunService`).
- **Category:** 4.
- **Owner it should have:** `NodeService` for placement, a `VolumeService` for size, `TileTelemetryService` for the stream (it already resolves the *source* at `:153` and then hands the handler the docker call).
- **Duplicated at:** `app/files.go:26` (`clus.NodeOf`), `server/storage.go:31-34` and `:140` (already audited), `org/registry.go:78` (`registry.NewClient` built in the handler).
- **Severity:** debt.

### app/handler.go:773-777 — managed-tile eligibility read with the raw store
- **Rule:** which instances a tile may provision from, and which existing databases it may attach.
- **Category:** 4.
- **Owner it should have:** `SliceService` / `ManagedInstanceService`.
- **Duplicated at:** `-`.
- **Severity:** debt.

### app/storage.go:33-57, 87-92, 113-120 — attachment rules mirrored for the picker
- **Rule:** an already-attached sub-path is not offered; a database needs local-backed storage (`storagetiles.ValidateAttach`); an exact duplicate line is a 409; a detach matches a line by exact string.
- **Category:** 4, 6.
- **Owner it should have:** `TileService`'s storage validator, which `:93-96` says owns "the grammar, the lookup, the local-backed rule and the kind rule".
- **Duplicated at:** the service validator; `app/handler.go:1310` binds the same column from the settings form with no rules at all.
- **Severity:** debt. The picker's hint and the writer's refusal are two implementations of one rule, and the hint is the weaker (no kind check).

### account/handler.go:77-88 — email uniqueness rule outside AuthService
- **Rule:** a profile save may not move the account onto an address another account holds; the new address is written trimmed but not normalised.
- **Category:** 2, 6.
- **Owner it should have:** `AuthService` — `Register` (`internal/stackrd/service/auth.go:65-71`) already owns exactly this check *and* normalises with `NormalizeEmail`; there is no `ChangeEmail`.
- **Duplicated at:** `internal/stackrd/service/auth.go:66` (the register copy).
- **Severity:** debt. The store folds case on lookup (`internal/stackrd/store/repo/sqlite/users.go:28-36`, written for this very bug), so the collision check holds — but the write still stores un-normalised, and the check-then-save has no transaction behind it.

### account/handler.go:177-190 — key list filtered in the handler
- **Rule:** a person sees only their own API keys, admin included.
- **Category:** 1.
- **Owner it should have:** `APIKeyService.ForUser`.
- **Duplicated at:** `account/handler.go:222-232` (`ownsAPIKey` scans the same global list).
- **Severity:** debt.

### auth/login/handler.go:92-109 — logout ordering rule
- **Rule:** the session row must be deleted before the cookie is cleared; a failed delete keeps the user signed in and says so.
- **Category:** 3.
- **Owner it should have:** a session service (or `AuthService.Logout`), so an API or CLI logout cannot invert the order.
- **Duplicated at:** `-` (worth checking the API side in shard D).
- **Severity:** debt. Correct and covered by `logout_test.go`; listed because it is a security ordering rule with exactly one implementation, in a page package.

### settings/handler.go:485-518 — connector delete: guard plus a loose id fallback
- **Rule:** a connector still referenced by a stack or tile may not be deleted.
- **Category:** 1, 3.
- **Owner it should have:** `ConnectorService.Delete` — it already answers `Users(ctx, cn)`; the refusal should be its own.
- **Duplicated at:** `-`.
- **Severity:** debt. Also `:488-490` falls back to `c.Param("id")` for the connector id, which on the `/orgs/:id/...` route is the org — dead in practice, live as a footgun.

### settings/handler.go:693-717 — the audit filter list is a second full query
- **Rule:** the actor dropdown is built from an unfiltered page of the same table.
- **Category:** 3.
- **Owner it should have:** `AuditService.Actors(ctx)`.
- **Duplicated at:** `-`.
- **Severity:** trivial.

### sharepub/handler.go:96-103 — the post-submit release is nil-guarded
- **Rule:** after a drop-box submit, variables that just arrived are marked applied so parked deploys are released and config plans refresh.
- **Category:** 5.
- **Owner it should have:** `sharelink.Submit` / `RevokeService`, inside the same transaction as the burn.
- **Duplicated at:** `-`.
- **Severity:** debt. `if h.vars != nil` re-creates the exact failure the comment above it describes as fixed: a handler wired without `WithVariables` silently skips the release with no error.

### org/members.go:436-442 vs server/nodes.go:44-53 — two answers to "what is my own URL"
- **Rule:** the absolute base URL of this panel.
- **Category:** 6.
- **Owner it should have:** one helper (`components.BaseURL` already exists and is used by `org/setup.go:498`).
- **Duplicated at:** `inviteURL` reads `TLS` plus `X-Forwarded-Proto`; `panelHost` reads the configured base URL then `TLS` only; `components.BaseURL` is a third. An invite link and a join script generated by the same request can disagree on scheme.
- **Severity:** debt.

### server/handler.go:229-246 / org/settings.go:507-526 — level scope guards, same shape twice
- **Rule:** this page may only delete resources at its own level (`instance` / `org`).
- **Category:** 6, 1.
- **Owner it should have:** `DomainResourceService.DeleteAtScope(level, ownerID, id)`.
- **Duplicated at:** each other.
- **Severity:** debt.

### org/setup.go:170-194 — `SetupDone` owns the finish rules
- **Rule:** an org still carrying the draft name may not finish; finishing stamps `setup_done_at` once and then mints a default domain.
- **Category:** 1, 3.
- **Owner it should have:** `OrgService.FinishSetup`; the draft-name constant is already `service.DraftOrgName` (`:442`), so only the comparison stayed behind.
- **Duplicated at:** `org/setup.go:446` (`setupNamePrefill` compares against the same constant), `org/setup.go:349` (the summary branches on `SetupConfigBranch`).
- **Severity:** debt.

### org/setup.go:459-485 — branch switch side effects
- **Rule:** switching to the UI branch rejects any pending plan and clears the config binding.
- **Category:** 3, 5.
- **Owner it should have:** `OrgService` / `PlanService`.
- **Duplicated at:** `org/config.go:57-59` (clearing the binding when the connector is emptied — the same four-field reset).
- **Severity:** debt.

### org/registry.go:45, 73, 238 / org/members.go:25-35 — `ownerOf` as a view input
- **Rule:** owner rights on one named org, admins always qualifying.
- **Category:** 1.
- **Owner it should have:** `AccessService` (`stackrmw.IsOwnerOf` already exists and is what `org/settings.go:440` uses).
- **Duplicated at:** `stackrmw.IsOwnerOf`, `org/graph.go:31` and `:266` (`h.ownerOf` again).
- **Severity:** debt. Two helpers for one question; the page picks whichever the neighbouring line used.

### notification/handler.go:30-38 — a GET with a write
- **Rule:** opening the centre marks everything read.
- **Category:** 5.
- **Owner it should have:** fine where it is; listed only because a prefetch or a link previewer marks the list read.
- **Duplicated at:** `notification/handler.go:43` (the explicit button).
- **Severity:** trivial.

### org/plans.go:130-132 — "awaiting review" defined in the banner helper
- **Rule:** a plan is waiting if its status is `pending` or `error`.
- **Category:** 1.
- **Owner it should have:** `PlanService` / `repo.ConfigPlan.Waiting()`.
- **Duplicated at:** `org/config.go:211` and `:228` (approve/reject accept `pending` only — a narrower answer to the same question), `org/plans.go:111-126` (the count), `org/setup.go:473`.
- **Severity:** debt. The banner counts an errored plan as actionable; the approve route 409s on it.

**Outside the `.go` sweep — flagged for completeness**

`app/deployments_test.go` and `app/placement_test.go` exercise three rules that
live in `.templ` sources, not in any `.go` file in this shard:
`liveDeployment` (`app/panel.templ:389` — "the image in use is the newest
*done* build with a tag"), `placementReason` (`app/panel.templ:700` — what a
non-ready node means for a tile) and `homeNodeName` (`app/app.templ:718`).
These are category-1 domain rules sitting in view templates; the wave plan
should decide whether templates count as handlers for extraction purposes,
because a grep over `.go` files will never find them.

## Clean files

- `about/handler.go` — renders one page.
- `app/helpers.go` — pure serialisation for the editor's autocomplete.
- `app/files.go` — thin wrappers over `filebrowse`; the one infra call
  (`clus.NodeOf`, `:26`) is rolled into the app-handler infra finding above.
- `auth/cookies.go` — cookie mechanics only.
- `auth/register/handler.go` — bind, validate through `FormRules`, delegate;
  its rules are counted in the password/duplication findings above rather
  than as faults of this file.
- `settings/proxy.go` — binds and renders; the rules are in `service/proxy`.
- `settings/upgrade.go` — binds and renders; the rules are in `AdminService`.
- `sharepub/text.go` — presentation strings.
- `org/handler.go` — the two checks it holds (`:101` admin-only create,
  `:139` write rights in the *target* org) are deliberate, documented and
  tested, and the second cannot move to the route because the org arrives in
  the body.
- All test files: `app/{deployments,gate,placement,provisions,runlogs}_test.go`,
  `auth/login/logout_test.go`, `cli/handler_test.go`,
  `org/{create_admin,people_render,setup,tenancy}_test.go`,
  `search/handler_test.go`, `server/{confirm,volumenode}_test.go`.

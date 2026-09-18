# Shard x: plan + staging (lens 2)

Packages: `internal/stackrd/config/stackconf`, `config/staging`, `config/envops`, `config/orgconf`. Paths below are relative to `internal/stackrd/` unless they start with `internal/cli`. `internal/stackrd/service/` has no call into any of these packages (only `admin.go`, `auth.go`).

## 1. Call sites

| caller | surface | operation | called | gate | error handling |
|---|---|---|---|---|---|
| `handlers/api/v1/config.go:105` planStack | api | plan stack now | `applier.Planner.RunAll` | `requireConfigStack(write)` = requireOrgWrite + ConfigManaged else 409 (`config.go:46-50`) | ErrNoFile 422, ErrNotBound 409, else 422 |
| `handlers/api/v1/config.go:150` previewPlan | api | preview posted bundle | `Planner.PreviewBundle` | `requireConfigStack(write)` | ErrNotBound 409, else 422 |
| `handlers/api/v1/config.go:182` approvePlan | api | approve + apply plan | `stackconf.EnqueueApply(force=true)` | `requirePlan(write)`; status != pending → 409 (`:175`) | enqueue err 422; 202 |
| `handlers/api/v1/config.go:198` rejectPlan | api | reject plan | `store.SetConfigPlanStatus("rejected")` direct, no package call | `requirePlan(write)`; pending 409 | passthrough |
| `handlers/api/v1/config.go:267,271,284` exportStackConfig | api | export stack YAML | `stackconf.Planner{Store}.Snapshot`, `StateToResolved`, `ExportYAML`; ad-hoc Planner, not `a.applier.Planner` | `requireStackAccess` (read) | passthrough |
| `handlers/api/v1/config.go:298` exportOrgConfig | api | export org YAML | `orgconf.ExportYAML` | `requireOrg` | passthrough |
| `handlers/api/v1/orgconfig.go:19` orgRunner | api | build org runner | `orgconf.Runner{...}` constructed per call (twin of `prhook/handler.go:275`) | -- | -- |
| `handlers/api/v1/orgconfig.go:31` planOrg | api | plan org now | `orgRunner().Plan` | requireOrg + requireOrgWrite | ErrNotBound/ErrNoFile 409, else passthrough |
| `handlers/api/v1/orgconfig.go:69` previewOrgPlan | api | preview org bundle | `orgRunner().PreviewBundle` | requireOrgWrite + `org.ConfigManaged()` else 409 | ErrNotBound 409, else 422 |
| `handlers/api/v1/orgconfig.go:125` approveOrgPlan | api | approve + apply org plan | `orgRunner().Apply` **inline on request ctx** | `requireOrgPlan`; pending 409 | err 422 |
| `handlers/api/v1/orgconfig.go:140` rejectOrgPlan | api | reject org plan | `store.SetOrgConfigPlanStatus` direct | `requireOrgPlan`; pending 409 | passthrough |
| `handlers/api/v1/releases.go:146` promote (no plan) | api | promote commit | `applier.Promote` | requireStackAccess + requireOrgWrite + `promoteEnv` (`:170`) | err 400; 202 |
| `handlers/api/v1/releases.go:158` promote (with plan) | api | apply plan then promote | `a.work.Enqueue(stackconf.ApplyKind, s.ID, ApplyJob{Force: in.Force,...})`, bypasses `EnqueueApply` (dedupe key is `s.ID`, `job.go:298-306` says it must be `cp.ID`); no `cp.Status` check; Force defaults false | same | passthrough; 202 |
| `handlers/api/v1/lifecycle.go:175` createAutoDomain | api | add auto domain | `envops.Ops{...}.EnsureAutoDomain` (Ops built inline, no DBs) | `rejectManaged` (`:165`) | passthrough |
| `handlers/api/v1/lifecycle.go:365` patchStack | api | rename stack | `applier.RenameStack(s, name, &Plan{})`, then a second `store.UpdateStack` at `:375` | requireOrgWrite + `rejectManaged` (`:346`) + slug uniqueness (`:359`) | passthrough |
| `handlers/api/v1/lifecycle.go:389` getPREnv | api | read PR-env config | `envops.LoadPRConfig` | requireStackAccess | -- |
| `handlers/api/v1/lifecycle.go:407,418` putPREnv | api | set PR-env config | `envops.LoadPRConfig` / `SavePRConfig` | requireOrgWrite; no managed gate (file overrides it at `prhook/handler.go:488-499`) | passthrough |
| `handlers/api/v1/envs.go:80` deleteEnv | api | delete env | `envOps().Teardown` | `rejectManaged` + last-env 400 + force/running 409 | passthrough; `jobs.LoadSchedules` after |
| `handlers/api/v1/envs.go:107` resetEnv | api | reset managed env | `envOps().Teardown` | requires `ConfigManaged()` (inverse gate, `:96`) + force/running | passthrough; LoadSchedules |
| `handlers/api/v1/envs.go:147` copyEnv | api | copy env | `envOps().CloneTiles` after direct `CreateEnvironment` (`:144`) | `rejectManaged` + reserved slugs settings/list/HomeSlug (`:132`) | passthrough; LoadSchedules |
| `handlers/api/v1/envs.go:193` envOps | api | build Ops | `envops.Ops{..., DBs: managedtiles.NewService}` | -- | -- |
| `handlers/api/v1/stacks.go:64` deleteStack | api | delete stack | `envOps().TeardownStack` then `store.DeleteStack` | requireOrgWrite; no managed gate | passthrough |
| `handlers/api/v1/domainresources.go:107,124` createDomainResource | api | create domain resource | `envops.ValidateResourceHost`, `envops.HostTaken` | `rejectManagedOwner` (`:127`); no `CheckOrgSquat` | 400 / 409 |
| `handlers/api/v1/apps.go:324` patchApp | api | validate depends_on | `stackconf.ParseDep` | `rejectManaged` (`:183`) | 400 |
| `handlers/api/v1/types.go:81` appPatch | api | type reuse | embeds `stackconf.TileConf` | -- | -- |
| `handlers/api/v1/v1.go:47,57` | api | wiring | holds `stackconf.Applier` | -- | -- |
| `handlers/web/handler/project/handler.go:350` CreateEnvironment | web | create env (with base) | `ops().CloneTiles` after direct `CreateEnvironment` (`:346`) | ConfigManaged → managedErr (`:311`); reserved slugs settings/list only (`:323`) | passthrough; LoadSchedules |
| `handlers/web/handler/project/handler.go:389` DeleteEnvironment | web | delete env | `ops().Teardown` | ConfigManaged → managedErr (`:376`) + last-env + RequireConfirm | passthrough; LoadSchedules |
| `handlers/web/handler/project/handler.go:429,436` ResetEnvironment | web | reset managed env, replan | `ops().Teardown`, then `planner().RunAll` | requires ConfigManaged (`:422`) + RequireConfirm | teardown passthrough; replan err → flash |
| `handlers/web/handler/project/handler.go:460` Delete (stack) | web | delete stack | `ops().TeardownStack` then `store.DeleteStack` | RequireConfirm; no managed gate (deliberate, `:446-448`) | passthrough |
| `handlers/web/handler/project/handler.go:587,592` CreateTile | web | create tile (UI-managed → staged) | `stackconf.TileConfOf`, `staging.Stage(OpCreate)` via `stageChange` | `!ConfigManaged` stages; ConfigManaged → managedErr (`:603`); no editGate | passthrough |
| `handlers/web/handler/project/handler.go:698` stageChange | web | stage helper | `staging.Stage` (duplicate of `app/handler.go:1278`) | -- | -- |
| `handlers/web/handler/project/handler.go:764` runPlan (PlanNow `:748`, SaveConfigBinding `:702`) | web | plan stack now | `planner().RunAll` | loadStack only | ErrNoFile / err / plan error → flash |
| `handlers/web/handler/project/handler.go:793` ApprovePlan | web | approve + apply | `stackconf.EnqueueApply(force=true)` | loadPlan; pending else **400** (`:787`; api says 409) | err → flash |
| `handlers/web/handler/project/handler.go:817` RejectPlan | web | reject | `store.SetConfigPlanStatus` direct | pending else 400 | passthrough |
| `handlers/web/handler/project/handler.go:866` SaveEnvConfig | web | env branch/policy, replan | `planner().RunEnv` after `UpdateEnvironment` (`:862`) | requires ConfigManaged (`:853`) | err → flash |
| `handlers/web/handler/project/handler.go:933` SetPlanInput | web | set secret, replan | `planner().RunAll` after `UpsertVariable`+audit+`ClearWaiting` (`:923-930`) | loadStack; no managed gate | plan err → redirect settings |
| `handlers/web/handler/project/handler.go:955-963` PlanView | web | render plan | `stackconf.Plan` unmarshal, `ApplyKind`, `ApplyJob` | resolveStackSlugs | -- |
| `handlers/web/handler/project/handler.go:1997-2024` unsetSecrets | web | render inputs | `stackconf.Input` | -- | -- |
| `handlers/web/handler/project/handler.go:2278` | web | PR-env page | `envops.LoadPRConfig` | -- | -- |
| `handlers/web/handler/project/handler.go:2454` SettingsDomains | web | list resources | `envops.VisibleDomainResources` | -- | -- |
| `handlers/web/handler/project/handler.go:2473,2480` SaveStackDomain | web | create stack domain resource | `envops.ValidateResourceHost`, `HostTaken` | ConfigManaged → managedErr (`:2469`); no `CheckOrgSquat` | 400 / 409 |
| `handlers/web/handler/project/handler.go:2532` replanAsync | web | background replan | `planner().RunAll` in goroutine, `context.Background()` | ConfigManaged check only | errors dropped |
| `handlers/web/handler/project/handler.go:2641` ops | web | build Ops | `envops.Ops{..., DBs}` | -- | -- |
| `handlers/web/handler/project/handler.go:2652-2677` SavePREnv / RotatePRSecret | web | PR-env config | `envops.LoadPRConfig` / `SavePRConfig` | loadStack; no managed gate | passthrough |
| `handlers/web/handler/project/staging.go:42` StagingReview | web | review staged env | `applier.StagedPlan` | loadStagingEnv | passthrough |
| `handlers/web/handler/project/staging.go:62` StagingApply | web | apply staged env | `applier.ApplyStaged(force=true)` | ConfigManaged → managedErr (`:59`) | err → flash |
| `handlers/web/handler/project/staging.go:81,99` StagingDiscard / One | web | discard staged | `store.DeleteStagedByEnv` / `DeleteStagedChange` direct | none | passthrough |
| `handlers/web/handler/project/envcompare.go:32` compareStack | web | compare envs | `stackconf.Planner{Store}.Snapshot` (ad hoc) | -- | passthrough |
| `handlers/web/handler/project/envcompare.go:130-142` CopyEnv | web | copy tile from reference env | `Snapshot`, `StateToResolved`, `applier.CopyTile` | ConfigManaged → managedErr (`:124`) | passthrough; LoadSchedules |
| `handlers/web/handler/project/releases.go:322` PromoteCommit (with plan) | web | apply plan then promote | `h.work.Enqueue(ApplyKind, p.ID, ApplyJob{Force: true,...})`, bypasses `EnqueueApply`; no pending check | loadStack + promoteEnv (`:171`) | err → flash |
| `handlers/web/handler/project/releases.go:336` PromoteCommit | web | promote commit | `applier.Promote` | same | err → flash |
| `handlers/web/handler/project/releases.go:353-370` ExportConfig | web | export stack YAML | `Planner{Store}.Snapshot`, `StateToResolved`, `ExportYAML` (copy of `api/v1/config.go:267-284`) | resolveStackSlugs | passthrough |
| `handlers/web/handler/project/commitlog.go:252` | web | commit log | `applier.Planner.StackBranch` | -- | ignored |
| `handlers/web/handler/app/handler.go:783` (volume save) | web | stage volume attach/path | `staging.Stage(OpUpdate,"volume")` | `editGate` (`:744`), error propagated only on attach/path change | passthrough |
| `handlers/web/handler/app/handler.go:1142` DeleteVar | web | stage env var removal | `staging.Stage("env vars")` | `editGate` (`:1138`) | passthrough |
| `handlers/web/handler/app/handler.go:1186` SaveEnv | web | stage env + build args | `staging.Stage("env vars")` | `editGate` (`:1172`) | passthrough |
| `handlers/web/handler/app/handler.go:1278` stage | web | stage helper | `staging.Stage` | -- | -- |
| `handlers/web/handler/app/handler.go:1339` currentDesiredDomains | web | read desired domains | `stackconf.TileConfOf(...).Domains` | -- | passthrough |
| `handlers/web/handler/app/handler.go:1467,1531` SaveSettings | web | validate depends_on | `stackconf.ParseDep` | `editGate` (`:1439`) | 400 |
| `handlers/web/handler/app/handler.go:1582` SaveSettings | web | stage settings | `staging.Stage("settings", settingsPatch(a))` | `editGate` | passthrough |
| `handlers/web/handler/app/handler.go:1626` Delete (tile) | web | stage tile delete | `staging.Stage(OpDelete)` | `editGate` (`:1612`), a structural op; api twin uses `rejectManaged` (`apps.go:468`) | passthrough |
| `handlers/web/handler/app/handler.go:1710` CreateDomain | web | anti-squat | `envops.CheckOrgSquat` | `editGate` (`:1654`) | 409 |
| `handlers/web/handler/app/handler.go:1719-1726` CreateDomain | web | stage domain add | `stackconf.DomainConf{}`, `staging.Stage("domains")` | `editGate` | passthrough |
| `handlers/web/handler/app/handler.go:1778` CreateAutoDomain | web | add auto domain | `envops.Ops{...}.EnsureAutoDomain` (no DBs) | `rejectManaged` (`:1768`) | passthrough |
| `handlers/web/handler/app/handler.go:1830` ToggleDomainHTTPS | web | stage https flip | `staging.Stage("domains")` | `editGate` (`:1806`) | passthrough |
| `handlers/web/handler/app/handler.go:1903` DeleteDomain | web | stage domain removal | `staging.Stage("domains")` | `editGate` (`:1883`) | passthrough |
| `handlers/web/handler/app/handler.go:1946` checkMiddlewares | web | validate middleware refs | `stackconf.ParseMiddlewares` | -- | error string |
| `handlers/web/handler/org/config.go:79` SaveOrgConfig | web | bind org repo, plan | `orgcfg.Plan` | not checked | ErrNoFile / err → flash |
| `handlers/web/handler/org/config.go:184` SetPlanInput | web | set org secret, replan | `orgcfg.Plan` after `UpsertVariable`+audit+`ClearWaitingOrg` (`:173-180`) | not checked | err → flash |
| `handlers/web/handler/org/config.go:214` approvePlan (ApproveOrgPlan `:125`, SetupApprovePlan `setup.go:253`) | web | approve + apply org plan | `orgcfg.Apply` **inline on request ctx** | pending else 409; nil runner → 503 | err → flash, returns nil |
| `handlers/web/handler/org/config.go:230`, `setup.go:430` | web | reject org plan | `store.SetOrgConfigPlanStatus` direct | pending 409 | passthrough |
| `handlers/web/handler/org/settings.go:537,544,547` SaveOrgDomain | web | create org domain resource | `envops.ValidateResourceHost`, `HostTaken`, `CheckOrgSquat` | RequireOrgWrite + ConfigManaged → 409 | 400 / 409 |
| `handlers/web/handler/org/setup.go:220` | web | auto-create org domain resource | `envops.VisibleDomainResources`; then direct `CreateDomainResource` (`:223`, undeclared) | -- | passthrough |
| `handlers/web/handler/org/registry.go:298` ExportConfig | web | export org YAML | `orgconf.ExportYAML` | settingsOrg | passthrough |
| `handlers/web/handler/org/plans_templ.go:322`, `setup_templ.go:1148` | web | render org plan | `stackconf.ParsePlan` | -- | -- |
| `handlers/web/handler/server/handler.go:203,210` CreateDomainResource | web | create instance domain resource | `envops.ValidateResourceHost`, `HostTaken` | not checked | 400 / 409 |
| `handlers/web/handler/prhook/handler.go:91,160` | webhook | gate on PR-env config | `envops.LoadPRConfig` | HMAC signature (`:99`) | 404 |
| `handlers/web/handler/prhook/handler.go:275-276` planConfigs | webhook | plan org on push | `orgconf.Runner{...}.Plan` (constructed inline, twin of `api/v1/orgconfig.go:19`) | repo/branch match | slog only |
| `handlers/web/handler/prhook/handler.go:296,305,306` planConfigs | webhook | plan stack on push | `Planner.RunEnv`, `StackBranch`, `Run` | repo/branch match | slog, continue |
| `handlers/web/handler/prhook/handler.go:344` maybeAutoApply | webhook | unattended apply | `stackconf.EnqueueApply(force=false)` | `cp.Status == pending` | slog |
| `handlers/web/handler/prhook/handler.go:498` openPR | webhook | file overrides panel PR config | `envops.SavePRConfig` | `pre.Comment/Status != nil` | ignored |
| `handlers/web/handler/prhook/handler.go:510,525,526,532` openPR | webhook | build PR env | direct `CreateEnvironment`, `Planner.Opts`, `applier.ApplyResolved(force=true)`, `envops.CopyLayout` | pr_envs enabled/against | passthrough; LoadSchedules |
| `handlers/web/handler/prhook/handler.go:581` updatePlanComment | webhook | plan preview for PR comment | `Planner.Preview` | ConfigManaged + repo match (`:157`) | ErrNoFile return; err → markdown |
| `handlers/web/handler/prhook/handler.go:647,651` fileResolved | webhook | load file | `Planner.StackBranch`, `LoadResolved` | ConfigManaged | nil |
| `handlers/web/handler/prhook/handler.go:699` closePR | webhook | tear down PR env | `h.ops.Teardown` (Ops from `web/server.go:612`, with DBs) | -- | passthrough; LoadSchedules |
| `internal/cli/cmd/plan.go:192,227,260,290,310,319,338` | cli | preview / plan now / list / show / approve / reject | `client.PlanPreview`, `PlanNow`, `Plans`, `Plan`, `ApprovePlan`, `RejectPlan` → API rows above (`internal/cli/client.go:1049-1097`) | API's | API's |
| `internal/cli/cmd/org.go:38,66,89,122,145` | cli | org preview / plan / list / approve / reject | `client.OrgPlanPreview`, `OrgPlanNow`, `OrgPlans`, `ApproveOrgPlan`, `RejectOrgPlan` (`client.go:1021-1047`) | API's | API's |
| `internal/cli/cmd/promote.go:103` | cli | promote (optional plan) | `client.Promote` (`internal/cli/lifecycle.go:345`) → `releases.go:127` | API's | API's |
| cli | cli | staging review/apply/discard, config export, PR-env settings | no CLI verb found for staging; export and PR-env: not checked | -- | -- |

## 2. apply.go direct writes vs handler counterparts

`store` = `a.Planner.Store`. Sibling files called from `apply.go` are listed with their own refs.

| apply.go | row / effect | handler counterpart | divergence | class |
|---|---|---|---|---|
| `:328`, `:376` | `SetConfigPlanStatus("applied")` | rejected written directly at `api/v1/config.go:198`, `web/project/handler.go:817`, `api/v1/orgconfig.go:140`, `web/org/config.go:230`, `web/org/setup.go:430`; error at `job.go:86` | plan status written from 7 sites, no owner | -- |
| `:426` applyDomainRes | `CreateDomainResource` level=stack, `Declared=true` | `web/project/handler.go:2488`, `api/v1/domainresources.go:132`, `web/org/settings.go:555`, `web/server/handler.go:218` | apply skips `envops.ValidateResourceHost` and `HostTaken` (file has `stackconf.ValidateDomains` `stackconf.go:98`, body not checked); `CheckOrgSquat` only on the org page (`settings.go:547`), nowhere else incl. config | A |
| `:443` | `UpdateDomainResource` (IncludeEnvOnDefault, Declared) | `api/v1/domainresources.go` patch sets ACME only (`v1.go:323`); web has no update | different writable field sets per surface | A |
| `:452` | `DeleteDomainResource` only when `Declared` | `api/v1/domainresources.go:138`, web DeleteStackDomain `~2495` | whether handlers refuse a Declared row: not checked | not checked |
| `:474` applyUIEdits | `UpdateStack` UIEditsMode | none (grep: no handler writes UIEditsMode) | config-only | -- |
| `:560,570-575` RenameStack | `envnet.TearDown` each tile, `UpdateStack` name+slug, `restartTile` | `api/v1/lifecycle.go:365` calls this then `UpdateStack` again `:375`; web: no stack rename op | shared impl; API double-writes the row | -- |
| `:924` execute | `CreateEnvironment` (Name = capitalised slug, Color, ApplyPolicy from file) | `web/project/handler.go:346` (reserved settings/list `:323`), `api/v1/stacks.go:111` (no reserved check), `api/v1/envs.go:144` copyEnv (settings/list/HomeSlug `:132`) | three reserved-name rules; config has none | A |
| `:936` + `:971` | `UpdateStack` Settings, `PX.Resync` | `web/project/handler.go:1038,1053`, `api/v1/settings.go:149,160` | apply replaces whole blob (`:933`), handlers `settings.Merge` then `Check`; validation `DefaultsConf.Check` at parse (`stackconf.go:688`) | -- |
| `:964` + `:971` | `UpdateEnvironment` Color/Position/ApplyPolicy/Settings, `PX.Resync` when settings changed | `api/v1/envs.go:185` patchEnv (color+policy, no Resync), `web/project/handler.go:862` SaveEnvConfig (policy+branch, no Resync), `web/project/handler.go:2542` SaveEnvSettings (settings + Resync at `:2568` via `resyncProxy :1049`), `api/v1/settings.go:152,160` (settings + Resync) | ~~web env-settings save skips the proxy resync~~ **CORRECTED**: it resyncs at `:2568`; the original read was wrong. No drift on this row | |
| `:966` syncDeclared | `ClearDeclaredIntended`, `SetIntended{Key}` | `web/project/envcompare.go:108` `SetIntended{Key, Value}` | config writes key-only rows and wipes declared rows each apply; web marks with value | -- |
| `:1006` | `CreateEnvironment` HomeEnv fallback | store-owned (`repo.HomeEnv`) | -- | -- |
| `:293`, `:976` ensureSecrets | `UpsertVariable` generated secret (`:1814`) | `web/project/handler.go:923-930` (+`audit.Record`, `deploy.ClearWaiting`), `api/v1/variables.go:167-180` writeVars (+audit, ClearWaiting*) | apply mints with no audit row and no ClearWaiting | B |
| `:980` applyVars | `UpsertVariable` (`:1846`) + `deploy.ClearWaiting`/`ClearWaitingEnv` (`:1856,1870`) | `api/v1/variables.go:167-180`, `web/project/handler.go:2296-2302,2596-2602` | apply writes no `audit.Record` | B |
| `:983`, `:988` applyBackups | `backups.go:211` DeleteBackup, `:227` CreateBackup, `:233` UpdateBackup; `Backups.LoadSchedules` | `api/v1/backups.go:205` (`backup.Validate` `:202`, reload `:404`), `web/backups/handler.go:233`, `web/settings/handler.go:246-254` | config validates at parse (`stackconf.go:939-965`: dest, schedule, tz, keep, mode); whether `backup.Validate` is the same rule set: not checked | not checked |
| `:1022` deleteTile (`:1544-1566`) | `envnet.TearDown`, `PX.RemoveApp`, `DBs.Detach` provisions, `DeleteTile`; cron reload via `:1100` | `web/app/handler.go:1632-1640` same four + attached-volume guard `:1616`, **no `jobs.LoadSchedules`**; `api/v1/apps.go:471` teardownTile (body not checked) + LoadSchedules `:475` | config deletes an attached volume tile with no guard; web deletes a cron without reloading the scheduler | A (config) / B (web) |
| `:1038` createTile (`:1171-1240`) | `CreateTile`, `ReplaceTileVars`, `syncBindings`, `PublishConnection`, `syncDomains`, `DBs.Deploy` / `deploy` / `redeployAttached`; `jobs.ValidateCron` `:1202`; timeout default `:1206`; connector = `firstNonEmpty(tc.Connector, stack.ConfigConnectorID)` (`:1890,1897`) | `web/project/handler.go:607` (CreateTile, LoadSchedules `:611`, volume target Enqueue `:616`; connector org check `:552`), `api/v1/apps.go:157` (CreateTile, LoadSchedules `:162`; `checkConnector` `:145`, AllowsIngress `:122`, source guard `:126-133`) | config never checks the connector belongs to the org; config deploys on create, handlers deliberately do not (`:355` flash) | A |
| `:1049` updateTile (`:1404-1504`) | `UpdateTile`, `ReplaceTileVars`, `syncBindings`, `PublishConnection`, `redeployAttached`, `syncDomains`, `DBs.Deploy` (`:1473`), `PX.WriteApp` (`:1491`), `deploy` (`:1494,1500`) | `web/app/handler.go:1588-1600` SaveSettings (UpdateTile, cron LoadSchedules, `syncProxy`), `web/app/handler.go:1195-1202` SaveEnv (UpdateTile + Enqueue running service), `api/v1/apps.go:453-458` patchApp (UpdateTile, cron LoadSchedules) | config redeploys a service on any non-proxy field, rebuilds a cron on source change, redeploys a managed DB on limits/port; web SaveSettings and api patchApp never redeploy. `ReplaceTileVars` is called only from config (no handler call, grep) | B |
| `:1625,1628,1658,1667,1674` syncDomains | `DeleteDomain`+`CreateDomain` recreate, `CreateDomain`, `DeleteDomain`, `PX.WriteApp` | `web/app/handler.go:1700-1760` (GetDomainByHostPath, `CheckOrgSquat`, `checkMiddlewares`), `api/v1/domains.go:70-83` (GetDomainByHostPath, WriteApp; no squat, middlewares not checked), `web/app/handler.go:1836` / `api/v1/lifecycle.go:212` `SetDomainHTTPS` | config: conflicts via `plan.go:437` AllDomains at diff time, squat via `plan.go:348`; api createDomain has no squat rule; `apply.go:1624` says no partial-update op exists while `SetDomainHTTPS` is one | A |
| `:1093` teardownEnv (`:1573`) | `Ops.Teardown`, fallback `DeleteEnvironment` `:1575` | `web/project/handler.go:389`, `api/v1/envs.go:80` | shared | -- |
| `:347` applyMiddlewares | `middlewares.go:214` `UpdateStack` ProxyMiddlewares, `:218` `PX.WriteStackMiddlewares` | none | config-only | -- |
| `:901` applyMoves | `moved.go:280` `envnet.TearDown`, `:283` `RenameEnvironment`, `:306-311` TearDown/`PX.RemoveApp`/`RenameTile` | none (no handler renames a tile or env, grep) | config-only | -- |
| `:1173,:1406` createSlice / updateSlice / removeSlice | `slices.go:79` `Eligible`, `:85` `WaitReady` 90s, `:90/92` `ProvisionSlice`/`CloneSlice`, `:112` `SetBucketPublic`, `:130` `UpdateProvision`, `:147` `DropDB`, `:149` `Detach`, `:208` `CreateBinding` | `api/v1/resolve.go:116` ProvisionSlice (no `Eligible` call visible in `:81-155`), `web/app/handler.go:914` Eligible, `api/v1/databases.go:307` Eligible (attach), `api/v1/lifecycle.go:294` SetBucketPublic | api slice create may skip the Eligible scope rule: not confirmed; `WaitReady` only in config | not checked |
| `:862` | `Jobs.Hold`/`Release` | none | config-only | -- |
| `:77-105` deploy, `:59` isUpper | `Engine.Enqueue` / `EnqueueCurrent` / `EnqueuePromote`, upper-env rule from `EnvOrder` | `api/v1/apps.go:485` `envnet.UpperEnv` (`infra/envnet/envnet.go:110`) then Enqueue; `web/app/handler.go:1202` Enqueue with no upper guard | two upper-env rules (`apply.go:59` vs `envnet.UpperEnv`); web SaveEnv redeploys an upper-env tile from branch head | A |
| `:1197,1220,1435` | `managedtiles.NewDB`, `PublishConnection` | `api/v1/databases.go:241,247`, `web/project/handler.go:1703,1722` | shared helpers | -- |
| `:1938` pendingMoves | `placement.InGroup` (read) | `web/project/handler.go:967-975` re-checks the same on PlanView (body not read) | -- | -- |
| `job.go:86` RunApplyJob | `SetConfigPlanError` | -- | -- | -- |
| `staging.go:128` ApplyStaged | `DeleteStagedByEnv` | `web/project/staging.go:81` discard | -- | -- |
| `orgconf.go:635` Runner.Apply | `UpdateOrg :694`, `PX.Resync :700`, `UpsertVariable :707,723`, `TeardownStack :775`, `DeleteStack :779`, `Create/Update/DeleteDomainResource :815-837`, `CreateStack :864`, `CreateEnvironment :867` (hard-coded "production"), `Applier.ApplyResolved :886`, `UpdateStack :899`, `UpdateTile :938`, `DBs.Deploy :942,975`, `DBs.Remove :950,990`, `DeleteTile :952,992`, `CreateTile :970`; `storage.go:215-246`; `moved.go:78-103` | api `stacks.go:40` createStack, web `project/handler.go:165`; org var writes `api/v1/variables.go:350` putOrgVars, `web/org/config.go:173` | runs inline on the request context from both `api/v1/orgconfig.go:125` and `web/org/config.go:214`, the failure mode `job.go:241-248` fixed for stack applies; org var writes have no audit row (handlers do) | B |

Tile field-list copies (calibration said four): `repo.Tile` (`store/repo/models.go:342`), `TileConf` (`stackconf.go:363`), `tileToConf` (`serialize.go:76-162`), `applyTileConf` (`apply.go:1681-1770`), the diff side (`plan.go:415` Diff, `:732` diffSource), `settingsPatch` (`web/app/handler.go:1366-1425`), `patchApp` `has()` key switch (`api/v1/apps.go:177-451`), web SaveSettings form parse (`web/app/handler.go:1428-1577`), web CreateTile form (`web/project/handler.go:471-582`), api createApp (`api/v1/apps.go:60-157`), CLI `tile set` flags (`internal/cli/cmd/tile.go:221`, 17 flag registrations). Eleven hand-written mappings. Correction to the calibration: the `Tile → TileConf` serializer already exists, `stackconf.TileConfOf` (`serialize.go:72`), and is called by `web/project/handler.go:587` and `web/app/handler.go:1339`; `settingsPatch` at `web/app/handler.go:1366` does not use it.

## 3. Plan / staging as a service

| service | methods (sketched from the call sites above) | calls | called by |
|---|---|---|---|
| `PlanService` (stack) | `Plan(stack, sha)`, `PlanEnv(stack, env, sha)`, `PreviewBundle(stack, main, files, env)`, `PreviewBranches(stack, head, base, env, skip)`, `Approve(plan, force) → enqueue`, `Reject(plan)`, `Get`, `List`, `Export(stack)`, `RunApplyJob(job)`; owns every `SetConfigPlanStatus/Error` write | `ApplyEngine` | `api/v1/config.go`, `web/project/handler.go:702-1000`, `prhook/handler.go:250-347,581`, CLI `plan.go` |
| `StagingService` | `Stage(tile, op, group, patch, author)`, `DesiredEnv(tile)`, `DesiredDomains(tile)` (today `web/app/handler.go:1295,1320`), `Review(stack, env) → Plan`, `Apply(stack, env)`, `Discard(env)`, `DiscardOne(change)`; owns the editGate decision (`web/app/handler.go:1234`) | `ApplyEngine` | `web/app/handler.go` (8 sites), `web/project/handler.go:592`, `web/project/staging.go` |
| `OrgPlanService` | `Plan`, `PreviewBundle`, `Approve → enqueue` (today inline), `Reject`, `Export`; one `orgconf.Runner` instead of two inline constructors (`api/v1/orgconfig.go:19`, `prhook/handler.go:275`) | `StackService`, `DomainService`, `VariableService`, `StorageService`, `ApplyEngine` | `api/v1/orgconfig.go`, `web/org/config.go`, `web/org/setup.go`, `prhook/handler.go:275`, CLI `org.go` |
| `ReleaseService` | `Promote(stack, env, commit)`, `ApplyThenPromote(stack, plan, env, commit, force)` (today two hand-built `ApplyJob`s at `api/v1/releases.go:158`, `web/project/releases.go:322`) | `PlanService`, `DeployService` | api releases, web releases, CLI `promote.go` |
| `ApplyEngine` (today `Applier.execute` and helpers) | `Reconcile(stack, resolved, opts, force)`; each `createTile/updateTile/deleteTile/syncDomains/applyDomainRes/applyVars/ensureSecrets/applyBackups/env create+settings/RenameStack/applyMoves/applyMiddlewares/createSlice` becomes a call into the concept service that owns the rule | `TileService`, `DomainService`, `EnvService`, `StackService`, `VariableService`, `BackupService`, `SliceService`, `ProxyService`, `DeployService`, `SchedulerReload` | `PlanService`, `StagingService`, `OrgPlanService`, `prhook openPR` (`:526`), `CopyEnv` (`envcompare.go:142`) |
| `EnvService` (from `envops.Ops`) | `Create(stack, name, base?)`, `Clone`, `Teardown`, `Reset`, `TeardownStack`, `EnsureAutoDomain`, `PRConfig get/set`; one constructor instead of four (`api/v1/envs.go:193`, `api/v1/lifecycle.go:175`, `web/app/handler.go:1778`, `web/project/handler.go:2641`, `web/server.go:612`) | `ProxyService`, `SliceService`, `SchedulerReload` | api envs/stacks/lifecycle, web project/app/prhook, `ApplyEngine` |
| `DomainResourceService` (from `envops` host helpers) | `Create(level, owner, host, opts)` with `ValidateResourceHost` + `HostTaken` + `CheckOrgSquat` once; today four handlers and `applyDomainRes` each pick a subset | -- | `api/v1/domainresources.go`, `web/project:2460`, `web/org/settings.go:522`, `web/server/handler.go:196`, `web/org/setup.go:223`, `ApplyEngine` |

## 4. Operations a lens-1 shard likely missed

- stacks: stack rename exists on api (`lifecycle.go:365`) and config (`apply.go:352`) only; web has no rename handler (grep `Slugify`/`RenameStack` in `web/project/handler.go`).
- stacks: stack delete has no managed gate on web (`project/handler.go:446-460`) or api (`stacks.go:52-66`); deliberate per comment, record anyway.
- stacks+deploys: apply-then-promote bypasses `EnqueueApply` and its plan-keyed dedupe with no pending check, `api/v1/releases.go:158` and `web/project/releases.go:322`.
- stacks+deploys: upper-env rule exists twice, `apply.go:59` isUpper and `infra/envnet/envnet.go:110` UpperEnv; `web/app/handler.go:1202` SaveEnv redeploys with neither. A.
- envs: env create reserved-slug rule differs three ways (`web/project:323`, `api/stacks.go:105-107`, `api/envs.go:132`) and config has none (`apply.go:913`). A.
- envs: **withdrawn.** `SaveEnvSettings` (`web/project/handler.go:2542`) does resync, at `:2568` via `resyncProxy` (`:1049`), as api (`settings.go:160`) and config (`apply.go:971`) do. Not a B.
- envs: PR-env config has no managed gate on api (`lifecycle.go:394`) or web (`project/handler.go:2646`) while the file overwrites it on every PR open (`prhook/handler.go:488-499`).
- envs: `resetEnv` requires ConfigManaged, `deleteEnv` requires not ConfigManaged, both call the same `Teardown` (`api/envs.go:58-114`, `web/project:363-442`); one op with an inverted gate pair.
- tiles: web tile delete uses `editGate` (`web/app/handler.go:1612`), api uses `rejectManaged` (`apps.go:468`); a structural op gated two ways. A.
- tiles: web tile delete never reloads the cron scheduler (`web/app/handler.go:1632-1644`); api does (`apps.go:474`). B.
- tiles: tile settings/env save redeploys only from config (`apply.go:1494`); web SaveSettings (`:1588-1600`) and api patchApp (`:453-458`) leave the running container on the old image/port/limits. B.
- tiles: config picks the tile connector with no org ownership check (`apply.go:1890,1897`); web `project/handler.go:552` and api `apps.go:145` check. A.
- tiles: `ReplaceTileVars` (`store/repo/sqlite/tiles.go:97`) is called only from `apply.go:1212,1426`; no handler write path maintains that table.
- domains: api `createDomain` (`domains.go:35-90`) has no org-squat rule; web (`app/handler.go:1710`) and config (`plan.go:348`) do. A.
- domains: domain-resource create is four handlers plus `applyDomainRes`; `CheckOrgSquat` only at `web/org/settings.go:547`. A.
- domains: `apply.go:1624` recreates a domain row claiming no partial update exists; `SetDomainHTTPS` is used at `web/app/handler.go:1836` and `api/lifecycle.go:212`.
- variables: config writes vars and mints generated secrets with no `audit.Record` (`apply.go:1814,1846`; `orgconf.go:707,723`); every handler path audits (`api/variables.go:171,194`, `web/project:929,2301,2322,2601,2629`, `web/org/config.go:179`). B.
- variables: generated secrets (`apply.go:1777`) never call `deploy.ClearWaiting`; every other secret write does. B.
- orgs: org plan apply runs inline on the request context (`api/orgconfig.go:125`, `web/org/config.go:214`); stack apply moved to the work queue for exactly that failure (`job.go:241-248`). B.
- orgs: org plan reject is a direct store write at three sites (`api/orgconfig.go:140`, `web/org/config.go:230`, `web/org/setup.go:430`).
- databases: api slice create `resolve.go:116` shows no `Eligible` call in `:81-155`; web `app/handler.go:914` and config `slices.go:79` do. Not confirmed.
- plan: approve of a non-pending plan is 400 on web (`project/handler.go:787`) and 409 on api (`config.go:176`); api and web build different plan view shapes (`api/config.go:235` planDetailOut vs `web/project:955` raw `stackconf.Plan`).
- plan: `replanAsync` (`web/project/handler.go:2525-2538`) runs `RunAll` on `context.Background()` and drops the error.
- staging: `StagingDiscard` / `StagingDiscardOne` (`web/project/staging.go:75-107`) have no gate at all.
- staging: `StateToResolved` + `ExportYAML` export is copied verbatim between `api/v1/config.go:261-289` and `web/project/releases.go:347-375`, both with an ad-hoc `Planner{Store}`.

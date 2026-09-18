# Lens 2 shard: proxy (`infra/proxy`, `infra/forward`)

Rows are call sites. Paths are under `internal/stackrd/` unless shown otherwise.
`tx` column: no call site runs inside a store transaction; every proxy call is
issued after the store write has returned. Store methods themselves: not checked.

## 1. Call-site table

### 1a. `proxy.Proxy` writes

| # | caller file:line | surface | operation | call | gate / rule around it | tx | error handling |
|---|---|---|---|---|---|---|---|
| 1 | `handlers/web/handler/app/handler.go:1597` -> `:1923` (`syncProxy`) | web | update tile settings | `WriteApp(a, domains)` | `editGate` (`:1439`), skipped for kind cron/function (`:1596`) | after `UpdateTile` `:1588` | returned |
| 2 | `handlers/web/handler/app/handler.go:1752` -> `:1923` | web | add domain to tile | `WriteApp` | `editGate` `:1654`; `checkMiddlewares` `:1669`; port rule `:1673-1681`; wildcard needs dns_provider `:1687`; host+path uniqueness `:1698`; `envops.CheckOrgSquat` `:1710` | after `CreateDomain` `:1749` | returned |
| 3 | `handlers/web/handler/app/handler.go:1839` -> `:1923` | web | toggle domain HTTPS | `WriteApp` | `editGate` `:1806`; **no `d.TileID == a.ID` check** (`:1810-1813`) | after `SetDomainHTTPS` `:1836` | returned |
| 4 | `handlers/web/handler/app/handler.go:1865` -> `:1923` | web | set/clear domain cert | `WriteApp` | **none** (no `editGate`, no `rejectManaged`); ownership `d.TileID != a.ID` `:1852`; `tls.X509KeyPair` `:1858` | after `SetDomainCert` `:1862` | returned |
| 5 | `handlers/web/handler/app/handler.go:1912` -> `:1923` | web | delete domain | `WriteApp` | `editGate` `:1883`; ownership checked only on the stage branch `:1888`, **not** on the live branch `:1909` | after `DeleteDomain` `:1909` | returned |
| 6 | `handlers/web/handler/app/handler.go:1633` | web | delete tile (service or volume) | `RemoveApp(a.ID)` | `editGate` `:1612` (stages); attached-volume guard `:1616`; `RequireConfirm` `:1620` | before `DeleteTile` `:1640` | **ignored** (`_ =`) |
| 7 | `handlers/web/handler/app/handler.go:1778` -> `config/envops/envops.go:462` | web | generate auto domain | `EnsureAutoDomain` -> `WriteApp` | `rejectManaged` `:1768`; kind==service && port>0 `:1771` | after `CreateDomain` `envops.go:459` | returned |
| 8 | `handlers/web/handler/db/handler.go:609` -> `:646` (`syncProxy`) | web | add domain to managed db | `WriteApp(d, domains)` | `requireFileUnowned` `:579` (= `RequireUnmanaged`, org-scope exempt, fails closed); `managedtiles.SpeaksHTTP` `:582`; host+path uniqueness `:593`; port forced to engine port `:601` | after `CreateDomain` `:606` | returned |
| 9 | `handlers/web/handler/db/handler.go:633` -> `:646` | web | delete db domain | `WriteApp` | `requireFileUnowned` `:624`; ownership `dom.TileID != d.ID` `:628` | after `DeleteDomain` `:630` | returned |
| 10 | `handlers/web/handler/org/registry.go:284` | web | save org defaults | `Resync(ctx)` | `o.ConfigManaged()` 409 `:262`; `settings.Check` `:271` | after `UpdateOrg` `:278` | logged |
| 11 | `handlers/web/handler/server/handler.go:336` | web | save server settings | `Resync` | `settings.Check` `:324`; no managed gate (server level) | after `UpdateServer` `:329` | logged |
| 12 | `handlers/web/handler/project/handler.go:1041` -> `:1053` (`resyncProxy`) | web | save stack settings | `Resync` | `settings.Check` `:1033`; **no `ConfigManaged` check** | after `UpdateStack` `:1038` | logged |
| 13 | `handlers/web/handler/project/handler.go:2568` -> `:1053` | web | save env settings | `Resync` | `settings.Check` `:2561`; no managed check | after `UpdateEnvironment` `:2565` | logged |
| 14 | `handlers/web/handler/project/handler.go:389` -> `envops.go:519` | web | delete environment | `Teardown` -> `RemoveApp` per tile | `p.ConfigManaged()` 409 `:376`; last-env guard `:382`; `RequireConfirm` `:386` | before `DeleteEnvironment` `envops.go:584` | ignored inside `Teardown` |
| 15 | `handlers/web/handler/project/handler.go:429` -> `envops.go:519` | web | reset environment | `Teardown` -> `RemoveApp` | requires `ConfigManaged()` `:423`; `RequireConfirm` `:426` | same | ignored |
| 16 | `handlers/web/handler/project/handler.go:460` -> `envops.go:598` -> `:519` | web | delete stack | `TeardownStack` -> `RemoveApp` | `RequireConfirm` `:453`; managed stacks allowed on purpose `:447` | before `DeleteStack` `:463` | ignored |
| 17 | `handlers/web/handler/prhook/handler.go:699` -> `envops.go:519` | other (webhook) | close PR env | `Teardown` -> `RemoveApp` | none (webhook auth upstream, not checked) | same | ignored |
| 18 | `handlers/web/handler/settings/proxy.go:42` (`saveProxyEntries`) from `:148`, `:165` | web | write / delete custom dynamic entry | `SyncCustomDynamic(entries)` | adminOnly route `server.go:499-500`; `Slugify` name `:138`; `validYAML` `:143` | after `SetSetting` `:39` | returned |
| 19 | `handlers/web/handler/settings/proxy.go:84` | web | save trusted proxies | `RefreshCloudflare(ctx)` | adminOnly `server.go:497`; `netaddr.ParseTrusted` `:74` | before `SetSetting` `:92` | fallback to cache, else 502 `:85-90` |
| 20 | `handlers/web/handler/settings/proxy.go:99` | web | save trusted proxies | `EnsureTraefik` (goroutine) | as 19 | after `SetSetting` `:92,95` | logged |
| 21 | `handlers/web/handler/settings/proxy.go:123` | web | set static override | `EnsureTraefik` (goroutine) | adminOnly `server.go:498`; `validYAML` `:114` | after `SetSetting` `:119` | logged |
| 22 | `handlers/web/handler/settings/handler.go:380` | web | save DNS provider | `EnsureTraefik` (goroutine) | adminOnly; no validation of provider/env | after `SetSetting` `:373,376` | logged |
| 23 | `handlers/web/handler/settings/handler.go:624` | web | set managed registry domain | `WriteRegistry(reg.Domain)` | adminOnly `server.go:489`; managed registry must exist `:609`; `registry.EnsureManaged` first `:619` | after `UpdateRegistry` `:613` | returned |
| 24 | `handlers/api/v1/domains.go:83` | api | add domain to tile | `WriteApp(t, ds)` | `rejectManaged` `:40`; port rule `:51`; host+path uniqueness `:71`; **no kind / SpeaksHTTP check, no `CheckOrgSquat`, no middleware/rule/priority fields, no wildcard-dns check** | after `CreateDomain` `:76` | returned |
| 25 | `handlers/api/v1/domains.go:110` | api | delete domain | `WriteApp` | `rejectManaged` `:100`; tile resolved from `d.TileID` `:96` | after `DeleteDomain` `:103` | returned |
| 26 | `handlers/api/v1/lifecycle.go:238` | api | patch domain (https, force_https, cert) | `WriteApp` | `rejectManaged` `:205`; `X509KeyPair` `:228` | after `SetDomainHTTPS/ForceHTTPS/Cert` `:214-231` | returned |
| 27 | `handlers/api/v1/lifecycle.go:175` -> `envops.go:462` | api | generate auto domain | `EnsureAutoDomain` -> `WriteApp` | `rejectManaged` `:165`; kind/port `:168` | after `CreateDomain` | returned |
| 28 | `handlers/api/v1/helpers.go:132` (`teardownTile`) from `apps.go:471`, `databases.go:435`, `volumes.go:101` | api | delete app / db / volume tile | `RemoveApp(t.ID)` | `rejectManaged` (`apps.go:467`, `databases.go:418`, `volumes.go:99`); db: held-slices guard `databases.go:426`; **no `editGate`, no confirm** | before `DeleteTile` `helpers.go:134` | ignored |
| 29 | `handlers/api/v1/envs.go:80`, `:107` -> `envops.go:519` | api | delete / reset environment | `Teardown` -> `RemoveApp` | delete: `rejectManaged` `:64`, last-env `:71`, `force` if running `:76`; reset: requires `ConfigManaged` `:96`, `force` `:102` | same | ignored |
| 30 | `handlers/api/v1/stacks.go:64` -> `envops.go:598` | api | delete stack | `TeardownStack` -> `RemoveApp` | `requireOrgWrite` `:57`; no confirm | before `DeleteStack` `:67` | ignored |
| 31 | `handlers/api/v1/settings.go:160` (`patchSettingsFor`) | api | patch server/org/stack/env settings | `Resync` | `settings.Check` `:135`; **no `ConfigManaged` check at any level** (grep: none in `settings.go`) | after `UpdateServer/Org/Stack/Environment` `:141-152` | logged |
| 32 | `handlers/api/v1/proxycfg.go:56` (`saveProxyEntriesMap`) from `:111`, `:126` | api | write / delete custom dynamic entry | `SyncCustomDynamic` | adminOnly `v1.go:257-258`; `Slugify` `:103`; `yamlValid` `:107` | after `SetSetting` `:53` | returned |
| 33 | `handlers/api/v1/proxycfg.go:93` | api | set static override | `EnsureTraefik` (goroutine) | adminOnly `v1.go:256`; `yamlValid` `:82` | after `SetSetting` `:89` | **ignored** (`_ =`) |
| 34 | `handlers/api/v1/domainresources.go:224` | api | set domain-resource ACME email | `EnsureTraefik(ctx)` (synchronous) | `resolveResourceTenancy` `:207`; no email validation beyond lower/trim `:216` | after `SetDomainResourceACME` `:217` | **ignored** |
| 35 | `handlers/api/v1/registry.go:321` | api | patch registry | `WriteRegistry(r.Domain)` | adminOnly `v1.go:186`; `r.Managed` `:320`; **no `registry.EnsureManaged`** (web has it, row 23) | after `UpdateRegistry` `:316` | returned |
| 36 | `config/stackconf/apply.go:971` | config | apply env settings | `Resync` | `settingsChanged` only | after `UpdateEnvironment` `:964` | `warn` (logged) |
| 37 | `config/stackconf/apply.go:1491` | config | apply tile change (proxy-only field set) | `WriteApp` | `proxyOnly` = only `security_headers`, `basic_auth_*`, `domain *` fields changed `:1448-1453`; kind==service | after tile row update (not checked which) | `warn` |
| 38 | `config/stackconf/apply.go:1674` (`syncDomains`) | config | reconcile tile domains | `WriteApp` | `changed && t.Kind == "service"` | after domain rows create/delete `:1667` | `warn` |
| 39 | `config/stackconf/apply.go:1556` (`deleteTile`) from `:1022` | config | delete tile | `RemoveApp` | plan says `del`; `stopContainers` first `:1554` | before `DeleteTile` `:1566` | `warn` |
| 40 | `config/stackconf/apply.go:1573` (`teardownEnv`) -> `envops.go:519` | config | remove env | `Teardown` -> `RemoveApp` | only when `RT != nil && PX != nil`, else bare row delete `:1575` | same | ignored |
| 41 | `config/stackconf/middlewares.go:218` (`applyMiddlewares`) | config | apply stack middlewares | `WriteStackMiddlewares(stack)` | plan change set `:202`; **only writer of this method outside `Resync` (`proxy.go:614`)**; web/api have no middleware edit (grep `ProxyMiddlewares` in handlers: read-only `app/handler.go:1946`) | after `UpdateStack` `:214` | returned |
| 42 | `config/stackconf/moved.go:308` (`applyMoves`) | config | rename/move tile | `RemoveApp(t.ID)` | move declared in plan; `TearDown` first `:306` | before `RenameTile` `:311`; **no `WriteApp` after the rename** (route rebuilt only by later deploy/domain apply, not checked) | ignored |
| 43 | `config/orgconf/orgconf.go:700` | config | apply org defaults | `Resync` | `settingsChanged` | after `UpdateOrg` `:694` | logged |
| 44 | `cmd/stackrd/main.go:340`, `:357`, `:362`, `:367` | other (boot) | construct proxy, seed trusted proxies, start traefik, resync | `New`, `RefreshCloudflare` (via `seedInstall`), `EnsureTraefik`, `Resync` | none | n/a | logged |

### 1b. `proxy.Proxy` reads

| # | caller | surface | operation | call | gate | error handling |
|---|---|---|---|---|---|---|
| 45 | `handlers/web/handler/app/handler.go:555` (`loadTab` "http") | web | tile HTTP access log | `AccessLog(a.ID, 100)` | tile load `h.load` | n/a; API has no equivalent (grep `AccessLog` in `api/`: none) |
| 46 | `handlers/web/handler/settings/proxy.go:62` | web | proxy page | `CurrentStatic()` | adminOnly | n/a |
| 47 | `handlers/api/v1/proxycfg.go:70` | api | get proxy config | `CurrentStatic()` | adminOnly | n/a |
| 48 | templ signatures `app/panel_templ.go:26,440,1095`, `app/app_templ.go:64` | web | view type `proxy.AccessEntry` | type only | -- | -- |

### 1c. `forward.Registry`

| # | caller | surface | operation | call | gate | error handling |
|---|---|---|---|---|---|---|
| 49 | `handlers/api/v1/forward.go:132` (`forwardPresence`) from `:89` | api | open port-forward presence session | `fwd.Add(sess)` (+ deferred remove) | `requireTile(write)` `:37`; `forwardPort` `:41`; running-task check `:52`; env net `:62` | `a.fwd` nil-tolerant |
| 50 | `handlers/web/handler/org/graph.go:417` | web | org canvas forward chips | `Sessions()` | none (read) | -- |
| 51 | `handlers/web/handler/project/handler.go:1386` (`forwardCounts`) | web | env canvas forward counts | `Sessions()` | none | -- |
| 52 | `handlers/web/handler/project/handler.go:1431` (`addForwards`) | web | env canvas forward cards | `Sessions()` | none | -- |
| 53 | `cmd/stackrd/main.go:521` | other (boot) | construct | `forward.New()` | -- | -- |

Web has no forward writer; API has no forward reader. Same set (in-memory registry), so not class C.

### 1d. Class D: docker / swarm import above `infra/`

| file:line | import | note |
|---|---|---|
| `service/admin.go:16` | `github.com/docker/docker/api/types/swarm` | used at `:156-160` (`swarm.UpdateConfig`, `UpdateOrderStopFirst`, `UpdateFailureActionRollback`) passed into `rt.UpdateServiceImage` |

No docker/moby import in `handlers/` or `config/` (grep `"github.com/docker/` and `"github.com/moby/` outside `infra/`: only the row above; other hits are strings in a test, a shell script and a templ label).

Not a proxy write but worth one line: nothing in `infra/deploy` calls `WriteApp` (grep `WriteApp|RemoveApp` outside `handlers/`, `config/`, `infra/proxy/`: only a comment at `store/repo/models.go:431`). Routes are written at domain/settings time and by `Resync`, never by a deploy.

## 2. Rule sets around proxy writes

Grouped by what protects the write, not by what is written.

| set | rule | call sites |
|---|---|---|
| R1 | `editGate` (config-managed may stage) then `WriteApp` on the live branch | web rows 1, 2, 3, 5 (`app/handler.go:1439,1654,1806,1883`) |
| R2 | `rejectManaged` (fails closed, never stages) then `WriteApp`/`RemoveApp` | api rows 24-29 (`domains.go:40,100`, `lifecycle.go:165,205`, `apps.go:467`, `databases.go:418`, `volumes.go:99`, `envs.go:64`); web row 7 (`app/handler.go:1768`) |
| R3 | `RequireUnmanaged` with org-scope exemption (`db/handler.go:62`) | web rows 8, 9 |
| R4 | `p.ConfigManaged()` inline 409 (no shared helper) | web rows 10 (`org/registry.go:262`), 14 (`project/handler.go:376`) |
| R5 | **no managed gate at all** on a route-affecting write | web row 4 (`SetDomainCert` `:1846`), web rows 11-13 (`server/handler.go:312`, `project/handler.go:1022,2542`), web row 16, api row 30 (stack delete, deliberate), api row 31 (`patchSettingsFor`, all four levels), api rows 32-35, web rows 18-23 (admin pages: adminOnly only) |
| R6 | config apply: plan change set is the gate, errors go to `warn` | config rows 36-43 |
| R7 | boot / webhook: none | rows 17, 44 |

Seven rule sets for one write surface. The same operation crosses sets:

- "add domain to a tile": R1 (web app), R3 (web db), R2 (api), R6 (config) - four sets, and the API copy skips five checks the web copy has (row 24).
- "save settings then Resync": R4 (web org), R5 (web server/stack/env, api all levels), R6 (config stack+org) - the web org page refuses a config-managed org, the API patches it and resyncs anyway.
- "delete tile": R1 with stage + confirm (web), R2 no confirm (api), R6 (config); web db delete is outside all of them (see 4.1).
- "generate auto domain": R2 on both surfaces, the one operation where web and api agree, via the same `envops.Ops` literal duplicated at `app/handler.go:1778` and `lifecycle.go:175`.

Error handling around `RemoveApp` is uniformly ignored (rows 6, 14-17, 28-30, 39, 40, 42) and around `Resync` uniformly logged (rows 10-13, 31, 36, 43). `EnsureTraefik` is fire-and-forget in five of six call sites (rows 20-22, 33 background; 34 synchronous but discarded).

## 3. Sketched proxy service

One package owning `*proxy.Proxy`; nothing above it holds `px`. Method list covering every row above:

| method | covers rows | called by concept service |
|---|---|---|
| `SyncTile(ctx, tile) error` - `ListDomainsByTile` + `WriteApp`, skip cron/function/managed-non-HTTP | 1-5, 7-9, 24-27, 37, 38 | tiles (settings save), domains (create/delete/patch/auto) |
| `DropTile(ctx, tileID) error` - `RemoveApp`, logged not ignored | 6, 14-17, 28-30, 39, 40, 42 | tiles (delete), envs (teardown), stacks (delete), releases/prhooks (close PR) |
| `SyncStackMiddlewares(ctx, stack) error` | 41 (+ future web/api middleware edit) | stacks |
| `Resync(ctx) error` - after any settings-cascade write | 10-13, 31, 36, 43, 44 | settings (all four levels) |
| `SetCustomEntries(ctx, map) error` - store `proxy_custom_dynamic` + `SyncCustomDynamic` (moves the JSON marshal out of both handlers, `settings/proxy.go:34-43` and `proxycfg.go:48-57`) | 18, 32 | settings (admin proxy) |
| `SetStaticOverride(ctx, yaml) error` - validate, store, `EnsureTraefik` in background with logging | 21, 33 | settings (admin proxy) |
| `SetTrustedProxies(ctx, cidrs, trustCF) error` - parse, `RefreshCloudflare`, store, restart | 19, 20 | settings (admin proxy); API has no endpoint today |
| `SetDNS(ctx, provider, env) error` | 22 | settings (admin TLS); API has no endpoint today |
| `SetResourceACME(ctx, resID, email) error` - store + restart | 34 | domains (resources); web has no form today |
| `SetRegistryDomain(ctx, reg) error` - `UpdateRegistry`, `registry.EnsureManaged` when domain set, `WriteRegistry` | 23, 35 | registry |
| `AccessLog(tileID, n) []AccessEntry` | 45 | tiles (read); API gap |
| `CurrentStatic() string` | 46, 47 | settings (read) |
| `Forwards() []forward.Session` / `AddForward(sess) remove` | 49-52 | envs/graph (read), tiles (forward write) - or keep `forward.Registry` as-is, it has no rules |

`EnsureAutoDomain` stays in `envops` (it is domain logic) but takes the proxy service instead of `*proxy.Proxy`; the three `envops.Ops{...}` literals (`lifecycle.go:175`, `envs.go:194`, `app/handler.go:1778`, `project/handler.go:2641`, `web/server.go:612`) collapse into one constructor.

## 4. Operations a lens-1 shard likely missed

1. **Class B** - web managed-db delete (`db/handler.go:444`, `:464-467`) calls `dbs.Remove` + `DeleteTile` and never `RemoveApp`; `managedtiles.Remove` (`managedtiles.go:570-577`) has no proxy call either. A db with a domain (`db/handler.go:572`) keeps its traefik route until the next `Resync` prunes it (`proxy.go:717`). API `deleteDatabase` goes through `teardownTile` and does remove it (`databases.go:435`, `helpers.go:132`).
2. **Class A** - web `SetDomainCert` (`app/handler.go:1846`) has no `editGate`/`rejectManaged`; API `patchDomain` (`lifecycle.go:205`) has `rejectManaged`. A cert change on a config-managed stack succeeds in the browser and 409s over the API.
3. **Class A / authz** - web `ToggleDomainHTTPS` (`:1810-1813`) and the live branch of web `DeleteDomain` (`:1909`) never check the domain belongs to `:id`; the store write hits whatever `:domainID` names, then `syncProxy(a)` rewrites the wrong tile's route. API derives the tile from `d.TileID` (`domains.go:96`, `lifecycle.go:200`).
4. **Class A** - API `createDomain` (`domains.go:35`) accepts any tile kind (no `SpeaksHTTP`/`IsManaged`/kind check), skips `CheckOrgSquat`, the wildcard-needs-DNS check and `checkMiddlewares`, and cannot set `rule`/`priority`/`middlewares` (`domainIn` vs web `:1657-1665`). Web db page refuses non-HTTP engines (`db/handler.go:581`).
5. **Class A** - settings patch on a config-managed org/stack: web org page 409s (`org/registry.go:262`), web stack/env pages (`project/handler.go:1022,2542`) and API all levels (`settings.go:114`) write and `Resync`. Config apply then overwrites on the next plan (`orgconf.go:688`, `apply.go:963`).
6. **Class B** - API `patchRegistry` (`registry.go:298`) writes the traefik route without `registry.EnsureManaged`; web `SetRegistryDomain` (`settings/handler.go:619`) runs it first because the alias traefik dials lives in the service spec.
7. Surface gaps (no drift, just missing): trusted proxies (`proxy.go:67`) and DNS provider (`settings/handler.go:371`) are web-only; domain-resource ACME email (`domainresources.go:192`) is API-only; stack `proxy_middlewares` is config-only (`middlewares.go:191`); tile HTTP access log is web-only (`app/handler.go:555`).
8. `moved.go:308` removes the route on a tile rename and does not rewrite it; whether the following deploy/`syncDomains` always runs for a moved tile: not checked.
9. `EnsureTraefik` after ACME-email patch is synchronous and its error discarded (`domainresources.go:224`), unlike every other caller which backgrounds it; the request blocks on a container swap.

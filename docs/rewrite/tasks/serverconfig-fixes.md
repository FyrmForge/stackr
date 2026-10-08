# Server config blitz fix round: Opus review findings

Status: in progress 2026-10-07 under darthvader's go on `serverconfig-blitz.md`
(wave 2 includes the fix round). Five Sonnet fixers in parallel, then one
Fable verification pass.

Rules: as `serverconfig-blitz.md` (no git writes, no stash; no hand edits to
generated files; no em dashes; match style; `ponytail:` for ceilings).
**Test first**: each finding gets a test from its scenario that FAILS on
the current code, then the fix. Touch only your files; a need elsewhere
goes to `serverconfig-seams.md` "Fix round needs". Re-read a shared file
right before every edit. The full findings with scenarios and suggested
fixes are at the end of this file; the fix line there is a suggestion,
the scenario is the contract.

## F1 Connectors
Owns: `internal/web/handler/setup/handler.go`, `internal/service/wiring.go`
ONLY `clone`/`cloneEnv`, `leaf/connector/*`, `store/credentials.go`, tests.
- MEDIUM setup wizard 404 with only a shared server connector.
- LOW tile builds Ambiguous: a tile falls back to its stack's
  `ConfigConnectorID` when that connector serves the host; else the error
  says which connectors are ambiguous.
- LOW share/rename overwriting a completing App's config: update only the
  changed columns.

## F2 flow/serverconfig
Owns: `internal/service/internal/flow/serverconfig/*`, `internal/service/internal/slug/*`
(reserve `all`), tests.
- HIGH org slug `all` turns on share-all: reserve `all` as a slug AND make
  share rows and removal keys unambiguous (Field-based, not New == "all").
  The apply side (`serverconfig_apply.go` `applyShares`/`unshare`) is F3's;
  write the row contract into seams and F3 reads it.
- MEDIUM missing server param blocks the whole plan: only that item waits
  (a note on the plan, its rows dropped), everything else applies.
- LOW a ref to a param the same file declares: resolve against the file's
  non-secret params over live.
- MEDIUM create-only checks on existing rows block export-then-plan.
- MEDIUM `from:` rename of the root instance resource fights
  `settings.root_domain`: one of them must win consistently (a rename of the
  root resource also sets `root_domain`, and the file's `root_domain` and a
  `from:` rename of the same resource that disagree are a blocker).
- MEDIUM rename target skips the clash checks: run the add path's checks.
- LOW two renames from one host: blocker.
- LOW share-all removal row offered while a bound org would lose the
  connector: blocker on that row, as named-org shares have.
- LOW export writes acme_email/dns_provider values Parse refuses: export
  and Parse agree (Parse accepts what the panel accepts, or export skips).

## F3 Service apply and settings
Owns: `internal/service/serverconfig_apply.go`, `internal/service/serverconfig.go`,
`internal/service/admin.go`, tests.
- MEDIUM apply re-diff applies new risky changes without confirm: after
  the re-diff, a risky plan whose row was not confirmed fails "the server
  changed since the approve; plan again" (auto-applied plans included).
- MEDIUM panel_domain stored raw: clean and check it (`CheckRoot`, refuse
  wildcard, localhost and bare IPs per the leftover) before storing.
- MEDIUM root_domain plus panel_domain in one `SetSettings`: fixed order
  (root first), all validated before any write.
- LOW proxy_custom take-back race and skip on a later failure.
- Apply side of F2's share row contract (`applyShares`, `unshare`).

## F4 Domain rename
Owns: `internal/service/domainres.go`, `internal/service/domain.go` ONLY
the rename/redirect paths, `leaf/domainres/*`, tests.
- HIGH rename reclaims another org's generated redirect: reclaim only a
  redirect on the renamer's own moved tiles; any other is "already attached
  to a tile".
- MEDIUM `setRootDomain` stores the raw value: store the cleaned host, and
  the instance row and the setting stay in step.
- LOW generated redirect ignores the moved row's HTTPS flags.
- LOW a later stack, env or tile rename leaves the generated redirect
  pointing at a host nothing serves: the redirect follows the row (or is
  dropped with the rename), test it.

## F5 Web and proxy
Owns: `internal/web/handler/admin/config.go`, org drawer plan view
(`web/handler/canvas/orgconfig.go` plan/approve only), `ui/components/plan.templ`,
`internal/web/handler/canvas/org.go` ONLY invite link building,
`leaf/domain/caddy.go`, tests.
- LOW an approved plan still offers Approve, Reject and removal boxes
  until the apply finishes: show it as applying, no actions.
- LOW invite links and the setup prefill use BASE_URL's host after
  panel_domain moves: build them from the panel host setting.
- LOW custom route hosts get certs but no :80 route with TLS on: the :80
  server redirects them like every other host.

## Verification (one Fable worker, after all five)
Replay every finding with its own scratch test, close the seams, `make
test && make lint && make templint`, then record what was fixed, skipped
or left in `serverconfig-seams.md` "Fix round status".

## Findings (full)

### [MEDIUM] panel_domain is checked in its cleaned form but stored and used raw, so a pasted URL, host:port or trailing dot locks the panel out of the proxy
internal/service/admin.go:116 proven=True
Scenario: An admin types `https://new.example.com`, `new.example.com:443` or `new.example.com.` into the Settings form, or sends it with PUT /admin/settings/panel_domain. In internal/service/route.go, `checkPanelRoutes` and `checkPanelTaken` run `installspec.CleanHost` on the value and pass it. `settings.Set` only trims it and stores the raw string. Then `wiring.go` `proxyConfig` passes it raw as `PanelHost`, so `caddy.go` `Build` emits `match host ["https://new.example.com"]`. That matches no request, so the panel vhost is gone on every host. The GitHub manifest also uses it raw: `githubapp.Client.base()` sets `u.Host` to the raw value, so the URLs it registers are broken. I confirmed all three values are accepted and pushed with a scratch test in the service package (deleted). Leftovers section I only lists `localhost` and a bare IP, so the real gap is wider than stated. The server file path is safe because `Parse` and `checkSetting` normalise the value through `CheckRoot`.
Fix: In `SetSettings`, for panel_domain, run `installspec.CheckRoot` (refuse a wildcard) and store the cleaned host. This also closes the localhost/IP leftover.

### [LOW] proxy_custom take-back can lose a concurrent save, and is skipped when a later write in the same save fails
internal/service/admin.go:127 proven=False
Scenario: (a) `was` is read with no lock. Admin A saves a proxy_custom value Caddy cannot load while admin B saves a good one (form and server-file apply at the same time). A writes BAD, B writes GOOD, A's sync was built with BAD and fails, and A restores OLD over B's GOOD. B's sync then pushes OLD and returns nil, so B's save is silently lost. With two bad values in a row, B can restore BAD as its `was`. (b) The write loop walks a map in random order. If proxy_custom is written and a later write fails (for example `setRootDomain` in a server-file apply that also sets root_domain), the function returns before Sync and before the take-back. A value Caddy cannot load then stays stored and fails every later push, tile deploys included. That is the exact failure the take-back was added to prevent.
Fix: Hold a mutex across SetSettings (read `was`, write, sync, restore). Restore proxy_custom on any error after it was written, not only on a sync error, or write proxy_custom last.

### [LOW] Invite links and the setup prefill still use BASE_URL's host after panel_domain moves, and nothing tells the admin
internal/web/handler/canvas/org.go:224 proven=False
Scenario: An admin moves the panel from stkr.a.com to stkr.b.com (form or server file) and then creates an org invite. `comp.AbsoluteURL` builds it from `components.BaseURL`, which is set from the BASE_URL env at boot (cmd/stackrd/main.go), so the link is https://stkr.a.com/invite/<id>. The old vhost was dropped in the same push, so the invitee gets a dead link. The same holds for setup/handler.go lines 229 and 286 (prefill and invite). The S6 trace found this (item 3), but the panel_domain impact line in flow/serverconfig/diff.go `settingImpact` only names CLI logins, API keys and GitHub webhooks, and leftovers section I does not list it.
Fix: Build the absolute URL from the panel_domain setting, with the scheme and port from BASE_URL, as `githubapp.Client.base()` does. Or add invite links to the impact line and leftovers.

### [LOW] With TLS on, custom route hosts get certificates but no :80 route, so http:// requests to them get Caddy's empty response
internal/service/internal/leaf/domain/caddy.go:136 proven=False
Scenario: TLS is on and proxy_custom is `[{"match":[{"host":["x.example.com"]}],...}]`. `Build` sets `httpTail = nil`, so the :80 server has no route for x.example.com: no `forceRoute` redirect, and the custom route itself is not there. The https server has `automatic_https.disable_redirects: true`, so Caddy adds no redirect either. A browser that opens http://x.example.com gets no route, which in Caddy is an empty 200. The admin cannot fix it from proxy_custom, because the tail only goes on :443.
Fix: With TLS on, append a `forceRoute(h, "")` to `plain` for each host from `customHosts(custom)`.

### [MEDIUM] A missing server param blocks the whole server plan, not just the item that refs it (breaks serverconfig.md rule 5)
internal/service/internal/flow/serverconfig/diff.go:212 proven=True
Scenario: stackr-server.yml adds a route `new.io` and a backup dest `cold` whose secret_key refs `${{ server.params.cold.sk }}`, and that secret has no value yet. `differ.resolve` calls `d.block(...)`, so `Plan.Blocked()` is true. `checkApprove` then refuses the whole plan, `applyServerPlan` (internal/service/serverconfig_apply.go:218) refuses it again on the re-diff, and `AutoOK` is false. The unrelated route row, and every other row (orgs, settings, connector shares), cannot apply until the secret is set. Design rule 5 says "A missing value blocks only that item", and the S2 blitz says "nothing else". On the DR or fresh-box path (rule 2) this means one unset dest key stops the orgs from being created. A scratch test confirmed it: blocked=true, with the route row present but unapprovable.
Fix: For a missing ref, drop that item's change rows and add a note or a per-item flag ("backup_dests.cold waits for server.params.cold.sk") instead of `d.block`. The walk already re-diffs, so it skips that item.

### [LOW] A dest ref to a param the same file declares is blocked, so the file can never apply on its own
internal/service/internal/flow/serverconfig/diff.go:210 proven=True
Scenario: The file has `params: {cold: {ak: {type: param, value: AKIA}}}` and a dest `cold` with `access_key: ${{ server.params.cold.ak }}`, on a box where cold.ak is not stored yet (fresh box or DR). `resolve` reads only `d.live.Params`, so the plan blocks with "backup_dests.cold needs server.params.cold.ak (access_key), which is not set". The walk runs params before dests (serverWalk.params), so the apply would have stored the value first. But the plan is blocked, so the `param cold.ak` row never applies either. The file cannot converge until an admin sets the param by hand through the panel or CLI. A scratch test confirmed it: both the `param cold.ak` and `dest cold` rows were present, plus the blocker.
Fix: In `resolve`, look the key up in the file's non-secret `params:` values merged over `live.Params`, since params apply first.

### [LOW] Approved plan still offers Approve, Reject and unticked removal boxes until the apply finishes
/home/darthvader/FyrmForge/stackr/internal/web/handler/admin/config.go:110 proven=True
Scenario: An admin ticks route:X on a pending server plan, confirms and approves. The plan row keeps status "pending" with DecidedAt set until the apply job ends (leaf/orgplan Machine.Approve). The drawer redraw (configTab, `latest.Status == "pending"`) offers Approve and Reject again, and the checkboxes render unticked, so the approver cannot see what they ticked. Clicking Approve again returns 409 "already approved", and Reject returns a conflict. The org drawer (canvas/orgconfig.go configTab) does the same. That behavior existed before this feature, and the service refuses both clicks, so this is display only.
Fix: Offer the answers only when `latest.Status == "pending" && latest.DecidedAt == nil`, as setup/handler.go does with `applying`. While the apply runs, render the stored Ticked keys as checked chips.

### [MEDIUM] Setup wizard returns 404 when the org's only connector is a shared server connector
internal/web/handler/setup/handler.go:186 proven=True
Scenario: An admin creates a draft org with mode=config and shares a server connector with it. The org has no GitHub App of its own. In github(), the widened ConnectedConnectors returns only the server connector, so conns[0] is that server connector. github() then calls ConnectorInstallURL(og.ID, conns[0].ID). That goes through leaf connector Get, which refuses a nil-OrgID row as ErrNotFound, so GET /<org>/-/setup/connector?body=1 and /config?body=1 both return 404. The owner cannot get through the onboarding steps for exactly the case server connector sharing is meant for. Reproduced with a scratch webtest (copy at scratchpad/review/S1/zz_s1review_test.go): both pages return 404, and the log shows 'connector requested by another org owner=server'.
Fix: Take the install URL from the first conns entry with OrgID != nil, and leave it empty when there is none. Installing a server App is the admin's job, so InstallURL itself should keep refusing server connectors.

### [LOW] Tile builds fail with Ambiguous and nothing in a tile can name a connector
internal/service/wiring.go:297 proven=True
Scenario: An org has no GitHub connector of its own and is shared two server connectors on github.com. Two are normal: manifest-made Apps are private, so a server with repos under two GitHub accounts needs one App per account. buildTile calls clone(st.OrgID, "", t.GitURL, ...), which goes cloneEnv -> conns.For -> Resolve(conns, "", host). Resolve finds two shared connectors and returns Ambiguous ('name the connector to use'). Every tile build and push deploy from github.com in that org then fails, but a tile has no connector field to name one, and promote's plan never resolves connectors, so nothing blocks at plan time. The design rule makes this case a blocker the user can fix by naming a connector; for tiles that fix does not exist. Leftovers section I only lists the org file's version of this as closed.
Fix: Have a tile fall back to its stack's ConfigConnectorID when that connector serves the host, or show the Ambiguous error as a promote-plan blocker. Otherwise record the limit in leftovers section I.

### [LOW] A share or rename that overlaps Complete can overwrite the new App's config
internal/service/internal/leaf/connector/connector.go:202 proven=False
Scenario: SetShares and RenameServer read the whole row through Server(), then write it back whole with conns.Update, including the encrypted config column. Admin A starts a server connector and is finishing it on GitHub. At the same moment someone shares or renames that pending row from the admin Connectors tab or the API. Complete writes config={App, private key}, then the stale Update writes the pending config back over it. The App exists on GitHub, but stackr has lost its private key and webhook secret, and the row stays 'connecting' for good. The window is narrow.
Fix: Write only the column that changes: UPDATE connectors SET share_all = ? WHERE id = ? (and SET name = ? for rename) instead of rewriting the whole row.

### [MEDIUM] Server apply re-diffs and applies new risky (impact) changes without the approver's confirm
internal/service/serverconfig_apply.go:217 proven=True
Scenario: Plan P (local or repo) is made while live panel_domain/acme_email/root_domain match the file, so P has no impact line and is approved with no confirm (or auto-approved as AutoOK). Between plan and apply an admin changes panel_domain in the panel (or an earlier queued apply changes it). applyServerPlan re-diffs (line 217), gets a settings row with an Impact ('panel moves to X...'), checks only Blocked(), and walks it: the panel is moved back with nobody having confirmed it. The same path covers auto-apply (server_config_auto): a plan AutoOK at plan time can be risky at apply time (including org-create, share-all, root rename). pl.Confirmed is stored but never read at apply. Proven with a scratch test (acme_email, same code path): SetSetting acme_email=a, PlanServerFile(file with acme_email a + a new route) -> no impact, SetSetting acme_email=b, ApproveServerPlan(ApproveOpts{}) -> applied, acme_email back to a with no confirm. Org side is not affected today (flow/orgconfig emits no Impact).
Fix: In applyServerPlan after the re-diff: if plan.Risky() && !pl.Confirmed, fail with 'the server changed since the approve; plan again' (or compare plan.Impacts() with the stored plan's and refuse on any new one).

### [HIGH] Rename reclaims another org's generated redirect: cross-org hostname takeover
internal/service/domainres.go:286 proven=True
Scenario: Org B has org resource bcorp.io and tile api.shop.bcorp.io. B renames bcorp.io to bcorp.com, which leaves a generated redirect on api.shop.bcorp.io (B's tile) while B's DNS still points at the box. Org A, a different owner, creates acorp.io, attaches an auto host api.shop.acorp.io, then renames acorp.io to bcorp.io through POST /orgs/A/domain-resources/:id/rename. The held check passes (no resource is on bcorp.io any more) and the squat check passes (bcorp is not an org slug). planRename puts B's redirect into p.reclaim, and applyRename Detaches it. A's tile now serves api.shop.bcorp.io, so B's users following old links reach org A. A scratch test confirmed it: rename err=nil, B's redirect row is gone, and A's tile holds api.shop.bcorp.io. CreateDomainResource plus AttachDomain refuses the same host ('already attached to a tile'), and so does promote ('already routed to another tile').
Fix: In planRename, reclaim a generated redirect only when its TileID is one of the moved rows' tiles (it is the renamer's own earlier name). Any other redirect gets the existing 'already attached to a tile' Conflict.

### [MEDIUM] root_domain plus panel_domain in one SetSettings: panel shadowed or a partial write, depending on map order
internal/service/admin.go:121 proven=True
Scenario: The server file (or any caller) saves {root_domain: new.io, panel_domain: api.shop.acme.new.io} in one SetSettings call while tile api has auto host api.shop.acme.example.com. The check loop runs checkRootDomain/planRename against the OLD panel host, and checkPanelRoutes against the tile hosts BEFORE the rename, so both checks pass. Over 8 scratch runs: 6 landed both, leaving a tile domain on the panel host (exactly what checkPanelTaken exists to refuse; one side shadows the other, which risks admin lock-out). The other 2 wrote panel_domain first, then setRootDomain's re-plan refused 'is the panel's own host', so the panel moved but the root did not: a partial write in a save that checks everything before writing.
Fix: In SetSettings, check root_domain against the incoming panel_domain when vals carries one (pass it into planRename/checkRenamedHost). Have checkPanelRoutes also test the rename's new hosts. Write root_domain before panel_domain, or after all checks only.

### [MEDIUM] setRootDomain stores the raw value; setting and instance row desync, so later root changes silently move nothing
internal/service/domainres.go:481 proven=True
Scenario: An admin saves root_domain 'New.io' (or 'https://new.io/', or 'new.io.'). CheckRoot cleans it and the instance row is renamed to new.io, but the setting row stores 'New.io'. A later SetSetting('root_domain','other.io') then looks up instanceRow by exact match on 'New.io', finds none, and falls back to SeedInstance. SeedInstance is a no-op because an instance row exists, so the save 'succeeds' with root_domain=other.io while the instance row and every tile host stay on new.io. A scratch test confirmed it. The same path is hit when the root row was deleted while another instance row exists. followRoot also compares the stored string exactly, so an API rename does not follow either. Meanwhile the server-file plan prints a rename impact line that the apply never performs.
Fix: Store CheckRoot's cleaned value (keeping '*.') in setRootDomain and followRoot. When no instance row matches the old root but some instance row exists, refuse instead of calling SeedInstance.

### [LOW] Generated redirect ignores the moved row's HTTPS flags
internal/service/domainres.go:359 proven=True
Scenario: Precondition: TLS is on for the server and a tile's auto row has HTTPS=false (for example, TLS terminated upstream). After a rename, applyRename Attaches the redirect with HTTPS/ForceHTTPS nil, which means on (a scratch test confirmed https=true force=true). Caddy now asks for a certificate for the old host, which may not be issuable. The 308 Location is always https://<new host> (leaf/domain/caddy.go domainRoute), but the new row is HTTP-only, so the redirect lands on a host with no TLS route.
Fix: Copy HTTPS and ForceHTTPS from the moved row into the redirect's DomainSpec. Separately, the always-https Location in caddy.go should follow the target row's scheme.

### [LOW] A later stack, env or tile rename leaves the generated redirect pointing at a host nothing serves
internal/service/domain.go:180 proven=True
Scenario: Rename the instance resource from example.com to new.io: the redirect on api.shop.acme.example.com points to api.shop.acme.new.io. Then RenameStack shop to store: refreshAutoHosts moves the real row to api.store.acme.new.io but skips generated redirects entirely, so the old example.com host now 308s to api.shop.acme.new.io, which no route serves. TestRenameStackLeavesGeneratedRedirects asserts this stale target as intended.
Fix: In refreshAutoHosts, when an auto row moves from host X to Y, repoint every generated redirect whose RedirectTo is X to Y (the same repoint applyRename already does).

### [HIGH] Sharing with an org whose slug is "all" turns on share-all with no impact line, and the plan can auto-apply
internal/service/internal/flow/serverconfig/diff.go:319 proven=True
Scenario: An org owner renames their org to "All", which gives slug "all". slug.Valid accepts it and slug.Reserved only refuses "params". The admin then writes `connectors: [{name: gh-main, share: [acme, all]}]`. Diff emits {Kind: connector-share, Tile: gh-main, New: "all"} with no Field and no Impact. A scratch test shows plan.AutoOK()=true and Risky()=false. In serverconfig_apply.go:338, applyShares checks `c.New == "all"` and sets all=true, then calls ShareConnector(all=true). The connector is now shared with every org, so every org owner can clone every repo the App is installed on. There is no confirm, and with auto on it applies by itself, which breaks org isolation. The removal key also collides: `connector-share:gh-main/all` means both "drop share-all" and "drop org all". Dropping org "all" from a list goes through unshare's share-all branch and leaves that org shared.
Fix: Reserve "all" as an org slug (slug.Reserved), or give named-org share rows and keys a form that cannot equal the share-all row, e.g. key on the org id. Then make applyShares and unshare switch on Field=="share", not New.

### [MEDIUM] Diff runs create-only checks on rows that already exist, so export then plan is permanently blocked
internal/service/internal/flow/serverconfig/diff.go:673 proven=True
Scenario: (1) The root domain is example.com, with its instance resource seeded by the installer, and an org has slug "example". o.claims leaves out instance resources, so org create and rename allow that slug, and the file's own `orgs: [{slug: example}]` can create it. In domains(), domainres.Prepare runs for every entry, including existing ones, and Prepare runs CheckOrgSquat. Scratch test: Export(full()+org example) then Parse then Diff gives the blocker "domains: example.com: That hostname starts with another organization's slug." Every plan that has a domains: block is blocked forever, which breaks design rule 11 (export then plan reads clean). UpdateDomainResource never runs the squat check, so the existing row is fine. (2) Same class in routes(): the wildcard-needs-DNS-provider check also runs on existing file routes. SetSettings does not refuse clearing dns_provider while a wildcard http/https route exists, so in that state the exported routes: block blocks every plan.
Fix: Call Prepare, or only its squat check, just for the !have path (new resources); for existing ones check the email only. Apply the wildcard and TLS checks in routes() only to new or mode-changed routes.

### [MEDIUM] A from: rename of the root instance resource fights settings.root_domain forever
internal/service/internal/flow/serverconfig/diff.go:627 proven=True
Scenario: An exported file always carries settings.root_domain: example.com, since it is non-default, plus domains: [example.com]. The admin edits the domain to `{host: new.com, from: example.com}` and leaves root_domain alone. Plan 1 has a domain-rename row with no blocker. Apply runs RenameDomainResource, and followRoot (svc/domainres.go:402) rewrites root_domain to new.com. Plan 2 has a settings root_domain row new.com -> example.com, an impact line that renames everything back. Approving it brings the from: rename back in plan 3, and so on. The file never reads clean, and each confirmed flip moves every host and reissues certificates (scratch test: plan1/plan2 rows shown).
Fix: In domains(), when fd.From equals the live root host and settings.root_domain does not move to fd.Host, block with "rename the root through settings.root_domain", or treat it as the root rename and require root_domain to match.

### [MEDIUM] domain-rename target gets none of the clash checks the add path and the verb have, so the apply fails midway
internal/service/internal/flow/serverconfig/diff.go:655 proven=True
Scenario: `domains: [{host: old.io, from: example.com}]` where old.io is an external route, or `{host: panel.example.com, from: example.com}` where that is the panel host. Diff emits a domain-rename row with no blocker (scratch test). The add path (line 679/683) checks other resources and routes; the rename path checks only the instance map. At apply, RenameDomainResource -> planRename -> checkRenamedHost refuses (route or panel host), as do hosts held by non-instance resources and tile domains already attached. By then the walk has already applied params, shares, settings (proxy push), defaults (which redeploys N tiles) and dests. The plan ends in error with a partial apply. settings.root_domain has the same gap: checkRootDomain refuses only in SetSettings at apply. This breaks the blitz rule that blockers reuse each verb's refusal.
Fix: For a rename target, and for a root_domain move, run the same checks as the add path: any resource with that host, route.Holds(finalRoutes), Taken, and equality with the final panel domain. Ideally use a pure pre-check exported by S5.

### [LOW] Two renames from the same host pass Diff; the second fails at apply
internal/service/internal/flow/serverconfig/file.go:393 proven=True
Scenario: `domains: [{host: a.com, from: example.com}, {host: b.com, from: example.com}]` parses, and Diff emits two domain-rename rows with no blocker (scratch test). The same happens with `settings.root_domain: new.com` plus `{host: other.com, from: example.com}`. At apply, the first rename moves example.com and the second finds no instance row. The domains step errors after the earlier steps ran.
Fix: In checkDomains, refuse a from: named twice. In Diff, block a from: equal to rootFrom unless its host is rootTo.

### [LOW] A share-all removal row is offered even when a bound org would lose the connector
internal/service/internal/flow/serverconfig/diff.go:322 proven=True
Scenario: gh-all has ShareAll and org globex binds through it (Bound=[o2]). The file says `share: [acme]`. Diff emits the connector-share-delete key connector-share:gh-all/all as a tickable row with no note (scratch test). A named-org revoke in the same situation becomes a note. Ticking the row makes unshare call ShareConnector(all=false) without globex, which refuses ("binds to this connector"). The removals step fails after every other step applied.
Fix: Before emitting the all-removal, check each Bound org that is not in want.Orgs (and file org bindings). If any is found, emit a note instead of the row.

### [LOW] Export writes values for acme_email and dns_provider that the panel accepts but Parse refuses
internal/service/internal/flow/serverconfig/file.go:242 proven=False
Scenario: SetSettings (svc/admin.go) validates only trusted_proxies, panel_domain and root_domain beyond settings.Check, and boot env values are not checked either. A box with acme_email "Ops <ops@x.com>" or a dns_provider other than cloudflare exports a file that Parse rejects ("is not an email address" / "not a provider in this build"). Export then plan fails, and so does the DR bootstrap path (export, then apply on a fresh box).
Fix: Validate acme_email and dns_provider in SetSettings the same way checkSetting does, so live can never hold a value the file refuses.


# Rig fix round (from docs/qa/serverconfig-2026-10-07.md)

Status: go 2026-10-08 (darthvader). Two Sonnet fixers in parallel, then a
Fable verifier who also ships `v0.6.0-dev.18` and replays on the rig. Same
rules as above. Bug numbers are the QA report's; its repro is the contract.

## R1 A bad resource limit takes tiles down (QA bug 1)
Owns: `internal/service/internal/flow/deploy/*`, `internal/service/admin.go`
ONLY `SetSettingDefaults` and the redeploy scope, the org/stack/env/tile
settings setters where they validate `cpu_limit`/`mem_limit_mb` (grep
`leaf/settings` validation), the server/org apply jobs' handling of the
redeploys they queue (`internal/service/serverconfig_apply.go`,
`orgconfig.go` only that), `docker/*` and `dockerfake` if a host-info call
is needed, tests.
1. Refuse a `cpu_limit` above the host's cores (Docker info `NCPU`) and a
   `mem_limit_mb` above its memory, at every rung that sets them (server
   defaults, org, stack, env, tile, the server and org files' plans as a
   blocker). Message names the host's limit.
2. A deploy whose new container fails to create or start never leaves the
   tile with nothing running: create the new container before stopping or
   removing the old one (create needs no stop; only start does for
   stop-first tiles), and on a start failure bring the old one back.
   Test with dockerfake: create error and start error each keep the old
   replica running.
3. A tile left with no container must still count for "redeploys N tiles"
   and be redeployed by the next cascade change (the QA follow-up plan
   skipped the three dead tiles).
4. The server or org apply that queued redeploys reports their outcome: the
   apply job's log lists failed redeploys and the job ends error when any
   fail (or, if the apply cannot wait on them, the plan shows "N redeploys
   failed" once they finish; pick the smaller change that never reads
   "done" over failed deploys, and state it).

## R2 Small rig bugs (QA bugs 2-6)
Owns: `internal/web/render/plan.go`, `internal/ui/components/plan.templ`,
`internal/web/server.go` (CSP line only), `internal/ui/drawer/org/org.templ`
ONLY the domain rename form, `cmd/stackr/serverconfig.go` preview output,
tests.
2. Plan view keeps the item name on create rows; a change to an existing
   route is not relabelled "create".
3. CSP gets `img-src 'self' data:` so checkbox ticks render.
4. An applied plan shows ticked removals as removed, unticked as kept.
5. Org domain rename in the web confirms like the CLI does (same text,
   existing confirm component).
6. `stackr server preview` prints "no changes" for a clean file.

## Rig verify (Fable, after R1 and R2)
Replay each bug with scratch tests, `make test && make lint && make
templint`, ship `v0.6.0-dev.18` (`make release RELEASE=v0.6.0-dev.18`,
`scripts/rig.sh upgrade 0.6.0-dev.18`) and replay bug 1 on the rig
**only on a throwaway org** (org-level `cpu_limit` above `nproc` on a
throwaway org with one tile: refused; a forced failing deploy keeps the old
container if reachable without touching shop, web, infra, byhand), bugs
2-6 in the web and CLI. Never change server-level cascade defaults on the
rig. Append results to `docs/qa/serverconfig-2026-10-07.md`.

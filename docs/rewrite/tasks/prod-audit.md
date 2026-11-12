# Production-readiness audit, 2026-11-02

Read-only audit by three helpers (security; data safety and reliability;
operations, tests and debt). "checked" = verified in code by the main
session; the rest verified by the helper only. Plan not built yet: the
developer wants a plan, merged with review items from another agent.

## Blockers

1. Build context follows a symlink out of the clone (checked).
   internal/service/internal/git/git.go Repo.Under checks text only; a
   committed `ctx -> ../..` shipped the panel data dir (stackr.db, master key,
   CA key, other orgs clones) into an image. FIXED 2026-11-02 (merge after b59d2d56).
2. Deleting a backup destination makes its archives unreadable (checked).
   leaf/backup/backup.go:138 Delete refuses only if the panel backup or a
   schedule uses it; backup_runs.dest_id ON DELETE CASCADE
   (migrations/003_rest.up.sql:322) drops the runs, and the dest row holds
   the only ArchiveKey.
3. Default install: losing the box loses the key to off-box volume backups.
   Archive keys live only sealed in the panel DB; the panel backup goes to
   local disk unless panel_backup_dest is set (service/backup.go:531-558).
   Warning only (service/backupwarn.go), setup step not enforced.

## Should-fix: data

- Namecheap zone wipe on an empty/unparsed read (checked):
  namecheap.go edit() -> getHosts -> setHosts writes back only what was read;
  no guard against an OK answer with no hosts.
- Retention is newest-N by count, no sanity check (leaf/backup/backup.go
  688-710); no scheduler hold after a restore (recovery.md #6).
- Volumes get no backup schedule by default, no warning.
- Missed scheduled backups never catch up; no "no good backup in N days"
  alert (flow/schedule/schedule.go).
- Volume delete takes no final backup, plain confirm (service/volume.go:29).
- Jobs, job logs (<data>/jobs/*.log), security_events never pruned; pulled
  app images never cleaned (leaf/image/image.go:230).
- Repo clones (<data>/repos) and seed dumps never cleaned. FIX IN PROGRESS.
- Upgrade helper dying mid-swap can leave no panel running
  (leaf/panel/panel.go:203-226), manual docker start.
- Upgrade hold fails open on a Docker error (admin.go ~493); cert renewal,
  CA rollover, mesh and proxy pushes ignore the hold during a trial.
- Writes without a transaction: domain rename (domainres.go:331), PR
  secrets copy (params.go:173), connector shares (store/credentials.go:157),
  Change scope DB error mid-way. Plan approval read-then-write
  (leaf/orgplan/orgplan.go:100).
- Two waiting applies can block both job workers for 30 min
  (redeploys.go:44, jobs.go:156 two workers).

## Should-fix: security (helper)

- Host-access grants keyed by tile slug only (flow/deploy/mount_host.go
  22-47): change the image of a granted tile = host root, all envs.
- Build RUN steps bypass the tenant network filter (stackr-builder-net on
  Docker's default pool, docker/images.go:375; filter vipboot.go:320).
- SSRF via backup destinations, member-level (authz destination.write;
  leaf/backup/backup.go:260-290 probe + error text).
- Password change/reset revokes no sessions or API keys (checked:
  leaf/user/user.go ChangePassword/SetPassword only set the hash).
- Stolen node key: renewal has no due window and signs any CSR
  (certrenew.go:35-40); public /nodes/recover swaps the WG key
  (recover.go:67-72).
- TLS 1.3 resumption skips the revocation check (clusterca/clusterca.go
  292-328): set SessionTicketsDisabled or use VerifyConnection.
- Any ingress node can write _acme-challenge anywhere in a managed scope
  (acmedns.go:124, dnscap/acme.go:47-58).
- Forward to a host-network tile on a worker dials controller loopback
  (leaf/tile/forward.go:23, service/forward.go:59).
- Login limiter: no per-account cap, no IPv6 /64 bucketing
  (auth/login/handler.go:189); unknown email returns before argon2
  (leaf/user/user.go:166) = account enumeration by timing.

## Should-fix: operations and CI (helper)

- Every master push that passes CI releases and tags :latest; no install or
  upgrade test before publish.
- CI skips `make lint` (stale openapi.json check), -race, the 7
  integration-tagged packages, security scans (govulncheck, gitleaks).
- Go version: ci.yml pins 1.27.3, release.yml uses go.mod.
- internal/service tests ~6 min, no t.Parallel, 16 time.Sleep.
- Let's Encrypt certs never checked for expiry (alerts.go:540); old panel
  lacks the DNS challenge socket after a failed trial (acmedns.go:155).
- No troubleshooting / controller-down operator guide; alerts guide misses
  heartbeat, low-memory, crash-loop, OOM (alerts.go:668-837).
- amd64 only (workerupgrade.go:363, systemimages.go:110).

## Nice-to-have

- CSP allows 'unsafe-inline' with no inline scripts; no HSTS, base-uri,
  form-action; __Host- cookie prefix.
- Session tokens stored plaintext (store/sessions.go:19); API keys never
  expire; no 2FA. /api/health returns the raw DB error.
- AES-GCM without AAD (secrets/secrets.go:64).
- Join token in URL path; join installer and worker image not
  checksum/signature verified.
- Failed migration on a plain restart has no operator fix (no
  MigrateForce caller). Future table-rebuild migrations can cascade-delete
  (FK pragma ignored inside migrate's tx); add a test guard.
- Manual and pre-restore archives never pruned. Restore drops the -wal of
  the replaced DB copy (stackr-install/restore.go:120-126). Two panels with
  the same install id prune each other's backups.
- One DB connection (MaxOpenConns 1), 52 store.Tx callers.
- pg_dumpall not one snapshot; moving postgres:17 tag; restore doesn't stop
  bound tiles (leftovers G3).
- gosec G110 decompression bomb (flow/backup/backup.go:415).
- GitHub connector lists first 100 only (connectors/github/api.go:228).
- Leftover compat code: leaf/connector/kinds.go:152, flow/deploy/deploy.go
  311, service/internal/cluster (dup of internal/mesh).
- govulncheck: GO-2026-4887, GO-2026-4883 (docker daemon plugin code, not
  reachable, no fix released).

## Solid

Route authz and org isolation (authz.Can, path params checked), API keys
hashed, invites, webhooks HMAC, CSRF header token + WS origin + CORS, node
join (hashed single-use tokens, CA pin, mTLS), secrets AES-256-GCM at rest,
panel upgrade backup + trial + rollback, worker rollout per node with
self-rollback, backup consistency (VACUUM INTO, age encryption, verify
before restore), installer restore, log caps, disk checks, alerts webhook
(node, backup, certs, disk, clock, memory, crash loop, heartbeat). No
TODO/FIXME markers; 199 `ponytail:` comments (deliberate ceilings).

## Review items from stackr-13, 2026-11-02 (last 5 commits + templ pin)

Verified by stackr-13 in code. No fix approved yet.

Fix now (medium):
1. Rescope drops unwritten custom declarations outside the new scope
   (dnscap/records.go:683 DeleteFunc; HandBack built from dns_records only).
   Fix: drop the DeleteFunc, customWants (dnscustom.go:286) filters by InScope.
2. Renames commit, then a live MXRefused read per zone (dnscustom.go:217 via
   domain.go:325 refreshAutoHosts); provider outage leaves stale auto hosts.
   Fix: skip read when old and new host share a zone; live check only for
   UpdateDomain typed host (domain.go:247).
3. upgradeSwapping (admin.go:494, DB + docker.List, 5s) runs on every page
   render (render.go:181). Fix: memoise ~2s.
4. jobs.go:286 Hold() runs under r.mu once per startable job; Enqueue etc.
   block behind Docker. Fix: evaluate once per pass.
5. MX on a zone that flipped to forwarding: Namecheap drops it, row stays,
   reported written (records.go:496, dnscustom.go:292). Fix: delete those rows.

Fix soon (low):
6. dns.Rescope commits before moveResource (dnsrescope.go:106).
7. Record add returns standing sync error, 409 on retry (dnscustom.go:386/477,
   api/handler/v1/endpoint.go:73). Fix: 201 written:false + Note.
8. Trial window: writes on new panel lost on rollback (panel.go:231). Say it
   in the Update tab.
9. Docker timeout reads as "not swapping" (admin.go:494). Treat deadline as
   swapping.
10. Upgrade row mislabelled "restarted while this job ran" on a race
    (jobs.go:182 vs upgrade.go:136).
11. repoLocks comment stale, entries never freed, lock made before ownership
    check (orchestrator.go:306, dnsrescope.go:85).
12. Manage, Linked, AddDNSDomainRecord write dns_domains without the rescope
    lock (dnscap.go:229/302, dnscustom.go:461).
13. Waits: held reason hidden for job.All kinds (job.go:121).
14. connpanel/dns.go:49 org owners never see sync error (one server-wide string).
15. CLI --json -y rescope prints no plan (cmd/stackr/dnsconnector.go:152).
16. Namecheap username leaks to org owners in auth errors (namecheap/kind.go:107).
17. Stale `from` check compares Normal vs Scope form (dnsrescope.go:93).
18. Domain managed via another connector still offers Manage (dns.go:519).
19. No next suggestion after Manage/Unmanage (connpanel/dns.go:59).
20. Cosmetic: handback rows lack type; no 253-char FQDN check; gofmt
    dnsrescope.go; CLAUDE.md:6 says make install installs templ.

Parity gaps: serverconfig has no dns: section; record add 409 not idempotent;
InUse refusal unfiltered across orgs (server root_domain only).

## Plan decisions (one point at a time with darthvader)

- R1 rescope drops declarations: AGREED, drop the DeleteFunc + narrow/widen test.
- R2 rename MX live check: AGREED, drop it from renames (refreshAutoHosts), keep on UpdateDomain typed host.
- R3+R4+R9 upgradeSwapping per render/queue: AGREED, in-memory flag set when helper launched; new panel checks Docker once at boot and waits on the helper container; no per-request Docker call.
- R5 MX dropped shown written: COVERED by merged not-live label (customOut sets Written=false, NotLive). No work.
- R6 rescope half-done: AGREED, run moveResource checks before Rescope writes.
- R7 record add error/409: AGREED, return saved+written:false with reason in Note, no error on API.
- R8 trial-window writes lost on rollback: AGREED, refuse changes during trial (middleware on the R3 flag), reads still work. Exempt login/logout and node/worker endpoints.
- R10 upgrade row mislabelled: AGREED, at runner start finish stale upgrade rows succeeded when running version == job tag.
- R11-R20: AGREED as stackr-13 proposed (R11/R12 lock fixes, R13 held reason above All, R14 org sync error filtered by zones, R15 --json prints plan, R16 no Namecheap user in non-setup errors, R17 compare dnscap.Scope form, R18 non-managing state, R19 next suggestion after action, R20 cosmetics incl. CLAUDE.md:6).

## Rig upgrade test findings, 2026-11-02 (maintenance mode passed all 3 checks)

- U1 Failed trial: upgrade job still reads "done"; Update tab just offers the
  same upgrade again. Nothing says it rolled back.
- U2 Failed trial leaves stackr-proxy on the target image (replaced before
  the swap, never put back) and the target image in the head registry; it
  reached h4. A broken release could leave the proxy broken after rollback.
- U3 Banner does not show right after clicking Upgrade (only the Update
  section reloads); shows on next full page load.
- Note: `make release` hung 20+ min on Go module download in the build
  container; helper built from vendored modules instead.
- U2 failed trial leaves proxy/registry on target: AGREED, rollback restores proxy image and drops the failed tag from the registry.
- U1 failed trial reads done: AGREED, job failed 'Rolled back: the new panel did not pass its trial', shown on Update tab. U3 banner after click: AGREED, show it at once.
- AUDIT: darthvader agreed to all proposals 2026-11-02. Batches: A data/backup, B security, C ops/CI+cheap nice-to-haves.
  B3 decision: when the panel backup is on local disk and an off-box destination exists, the panel backup goes there too (archive keys travel off-box).
  Parked (big, his call later): release gating before publish, arm64, 2FA, AES-GCM AAD migration, MaxOpenConns, dup cluster package removal.

## Whole-repo review from stackr-13, 2026-11-02
Full validated report and per-area reviewer files: docs/rewrite/tasks/review-13/ (whole-repo-validated.md). 13 highs, ~45 mediums, many lows.

## Rig/QA test list (after all batches merge)
- U1-U3 failed upgrade: proxy restored, registry tag gone, Rolled back line, banner on click.
- R3/R8 upgrade flag + write refusal during trial.
- W1 UI-created private git tile: Deploy builds first release.
- W5 panel_domain to unresolved host refused; good host works.
- SEC5 large dump via web form and API -> 413.
- CFG2/CFG5 export server config is read-only; failed route update keeps the old route.
- Follow-ups not done: W5 old-host alias; SEC1 service-level principal check; CFG5 rollback when proxy sync fails.

## Rig/QA test list (after all batches merge)
- U1-U3 failed upgrade: proxy restored, registry tag gone, Rolled back line, banner on click.
- R3/R8 upgrade flag + write refusal during trial.
- W1 UI-created private git tile: Deploy builds first release.
- W5 panel_domain to unresolved host refused; good host works.
- SEC5 large dump via web form and API -> 413.
- CFG2/CFG5 export server config is read-only; failed route update keeps the old route.
- Follow-ups not done: W5 old-host alias; SEC1 service-level principal check; CFG5 rollback when proxy sync fails.
- Web batch merged. Headed QA: root domain confirm, job log page live follow, Back up now select, add-node expiry, invite wrong-user, tile URL redirect, Open nodes link.
- Not done from web batch: W4 (CLI batch C10), W11/W30 wizard, W12 recovery-key Problem, W16 wizard poll, W27 re-sync, W8 create-time check, W13 after-save list.
- SECURITY found while merging: org job routes look jobs up by ID only (cross-org read/cancel by UUID). Sent to API batch.
- CLI batch merged. Left: orphan key if /orgs fails after browser exchange; params set --json empty prints nothing.
- Review fixes (R batch) and API batch merged. Codex OUT OF CREDITS since ~22:30: R batch and API batch had no second opinion; rerun when topped up.
- Correction: org job routes were already isolated by Access.Require (ChildOf job); API batch added jobisolation_test.go proving it.
- Bug from CFG2: alert webhook param follows live URL at every plan; 2 tests fail on master; sent to config batch (G).
- API skipped: API11/12/23/24 route shapes, API13 response shapes, Last-Event-ID.
- Config batch merged; the 2 failing tests pass. NEW BEHAVIOUR: with auto on, daily re-plan (and at boot) auto-applies auto-OK drift, putting back panel edits the file doesn't hold. CFG4 CLOSED 2026-11-06 (by design): steps touch DNS/Docker so no tx can undo them; Terraform model, retry = Plan again (setup page) / Plan now (org drawer). Watch for flaky 'recovery' problem row in internal/service problem tests.
- Data batch merged (migrations 043, 044). Volume delete = orphan (final backup to LOCAL dest only). No backfill of default schedules for existing volumes. No 'volume has no backup' warning (none existed).
- CLASH found on merge: data batch made the panel helper exit 0 on clean rollback (restart policy), so undoFailedUpgrade (keys on non-zero exit) never runs -> proxy/registry/job not undone. Fix worker started.
- openapi.json stale on master (make lint fails) after combined API changes: regenerate at the end.
- Security batch merged. Migration renumbered 043_session_token_hash -> 045 (clash with data batch 043). Everyone re-signs in after upgrade (sessions hashed). CSP script-src 'self' in prod. Password change logs out every session incl. caller.
- Rig tests from security: worker build (builder net in cluster pool; existing stackr-builder-net must be removed), forward to host-net tile on worker, cert renewal in last third, host grant src: line UI, CSP in browser (terminal, add-domain preview), HSTS, rig DB with sealed columns.
- Gaps: first build on a node may run unfiltered up to 1 min; csrf cookie not __Host-.
- Ops batch merged. OPS29 CLOSED 2026-11-06 (won't fix): a docker restart mid-swap really rolls the node back and the panel says so; only a ms race can leave the new binary running until the next restart. OPS16 FIXED 2026-11-06: installer mounts the docker data root ro into the panel (installspec DockerRoot); old installs need an installer re-run to get it. SEC6 behaviour change: lower rung cannot exceed/lift parent cpu/mem limit. /api/health 200 degraded when docker down.
- docs/openapi.json regenerated after all merges (uncommitted).
- Browser QA (local): 7 pass, no bugs; fixed stale account password copy (0133469a). Not testable locally: job log details link on a live job, node-down banner, vars grid redeploy list, managed-elsewhere DNS state. CHECK: ?drawer=vars opens empty and no vars node on canvas; is the shared vars grid reachable? Root domain cannot be blanked once set (by design?).

## Backlog batches 2026-11-03 (merged to local master, not pushed)
- Ops: upgrade trial probes /api/health (helper on host net); parseTag takes vX.Y.Z-dev.N, offered only to dev builds; old stackr-builder-net off the cluster pool is replaced; build applies node tenant filter before the builder starts (one VIP rebuild per build).
- API: envs-order takes slugs (API23); job events stream sends ids + Last-Event-ID resume; SEC1 ShareConnector principal check in service; SEC9 tidyShare drops old options on read; CFG5 failed route row deleted before old route recreated.
- CSRF __Host- prefix: done via Echo CSRF directly (Path=/). Later: add CookiePath to hamr CSRFConfig and switch back.
- Web: backupWatch adds @daily keep 7 to unscheduled volumes with no runs, else backup.none warning; orphan final backup to last scheduled off-box dest, local fallback; W12 recovery.unsaved critical problem; W8 create-time connector check; W13 org defaults lists redeploys; W16 poll stops on expiry; W11 cert summary honest; W27 Sync records now; W4 installer re-asks admin; ?drawer=vars opens empty grid; README troubleshooting link.
- Skipped: API11/12 (delete/update verb style, product call), API13 (line-based tile fields), API24 (slashes in repo name), W30 (UX), W11 live ACME probe, alias removal UI.

## Production-readiness audit 2, 2026-11-09

Read-only, five helpers (regression check of the 2026-11-02 audit; Postgres
HA and PITR; multi-node, mesh and CA; auth and org isolation over ~720
routes; install, upgrade, backup and ops). No static pass (runs on release).
"checked" = re-read in code by the main session. No fix approved yet.

No blockers. Nearly every 2026-11-02 fix landed and holds.

### High

1. Org-bound API key can rotate the user's unbound key and read the new
   secret (checked). service/admin.go RotateKey and RevokeKey guard only
   KeyCapped / KeyStack, never KeyOrg. A CI key for one org becomes the
   all-orgs key, admin if the user is admin. Fix: refuse when KeyOrg != "".
2. Host-access grant bound to a mutable tag or git URL (checked).
   flow/deploy/mount_host.go tileSource hashes ImageRef or GitURL plus
   command, not a digest or commit. New code under the same tag keeps the
   grant: tenant to host.
3. One unreadable worker stops every HA primary (checked, panel side).
   service/lease.go leaseWanted sets w.err on any read failure and
   pushLeases then pushes to nobody; workers stop primaries after the 30 s
   lease. Same for any panel stall over 30 s. Fix: per-node errors.
4. Manual promote to a named node skips the replay LSN comparison
   (checked). service/failover.go failoverTarget returns the named member;
   pgsync.go syncTargetOK passes any SyncIn standby. With two sync standbys
   the one behind can be promoted: confirmed commits lost, nothing recorded.
5. 30 min job cap applies to volume restore, after the wipe (checked).
   orchestrator.go:840 Uncapped holds run, move-env, bucket-restore only;
   flow/jobs/jobs.go:381. Large volume = half-restored. Backups of the same
   volume reported to hit the cap too (helper).
6. Panel container has no --stop-timeout (checked). installspec.go RunArgs.
   Docker kills at 10 s, the job runner waits 30 s: a reboot mid-restore
   leaves a part-wiped volume.

### Medium

- Stuck failover raises no alert and mutes primary-down (checked):
  failover.go observePrimary returns while failoverActive.
- Org user can void sync replication (checked): synchronous_commit, fsync,
  full_page_writes, wal_keep_size not in pgOwned
  (flow/operated/pgsettings.go). Panel reported to still say Protected.
- Member can place a standby on any node by name (checked): members.go
  addStandby uses nodeByRef only, no placement or pin check. Same for
  promote to=. Member can also declare a primary node gone.
- Org delete makes its backups unreadable (checked): trigger
  backup_destinations_keep_runs (001_initial.up.sql:741) steps aside when
  the org is gone; runs and archive keys cascade away.
- Master key and install.json written without fsync (checked):
  installspec.go WriteKeyFile, Save.
- STACKR-XN has only ACCEPT rules, mesh-origin traffic then returns to
  Docker's FORWARD (checked chain; reach of loopback-published ports from a
  hostile worker NOT proven, needs a rig test). vip/filter.go renderFilter.
- One worker reporting the whole supernet makes the pool full for all
  (checked): service/mesh.go taken.
- /nodes/recover swaps WG keys on any validly signed request (checked):
  service/recover.go. Old node certs not superseded after renewal
  (deliberate, certrenew.go comment).
- Login-limited rows log the raw email form value, up to the 2 MB body
  limit (checked: service/security.go auditLogin, no truncation). Pruned
  after 90 days (the helper's "never pruned" was wrong), still a disk fill.
- Registry client SSRF: image refs dial any host, plain http for private
  addresses, follows the Bearer realm with org credentials
  (registry/registry.go, helper only).
- Shared git connector picked by host only, no per-repo check (helper,
  suspected; may be by design).
- Owner-created NFS/SMB shares need no admin grant, unlike other host-level
  access (helper).
- Builder-tagged node sees every org's build sources; relay routing follows
  self-reported facts; tenant firewall absent for seconds at boot (helper).
- VACUUM INTO holds the only DB connection during the panel backup (helper).
- Release gate: smoke covers install, upgrade, rollback; not backup/restore
  or a migration on populated tables (helper).
- Cert renewal and CA rollover ignore the upgrade trial hold;
  protect_password unsealed in settings JSON (helper).

### Dropped as false

- "Trial-window writes lost on rollback": middleware/upgradehold.go exists.

### Low (helper only)

Lost WAL range not recorded after rewind; switchover checkpoint proof off by
one record; restore can start mid-failover; SQLArchivedPast string compare;
registry login token outlives logout (12 h); org-bound keys reach Self
routes; job log uncapped within one job; disk alert watches the Docker root
not the data dir; /api/health 200 when degraded; alive port :7444 on all
interfaces; ClockSlack 1 h; password change guessable at 600/min.

### Still parked

2FA, arm64, single DB connection. AES-GCM AAD is done (secrets.go enc2).

### Verdict

- Single node, operator's own apps: go. Needs an off-box backup
  destination, the recovery key saved, alert webhook set.
- Multi-node, all nodes owned by the operator, single-instance DBs: go as
  beta.
- HA Postgres: not yet. Highs 3 and 4 and the stuck-failover alert first.
- Several orgs that do not trust each other, or paying customers: no.
  Highs 1, 2, 5, 6 first, then the member/standby and org-delete mediums.
- Untrusted or internet-exposed workers: no. Mesh drop rule, recover gate,
  pool report filter first.

### Not covered by a code read

Restore drill from off-box only; upgrade from the last release with real
data; onboarding end to end; black-box test of the rig; load and soak;
chaos on HA; Codex second opinion on the 2026-11-02 batches; SECURITY.md,
controller-down runbook, dependency licence check, CLA.

### Validation pass, 2026-11-09 (main session re-traced each high end to end)

- High 1 key rotate: CONFIRMED. service/clilogin.go CLICode refuses KeyOrg
  keys for this exact reason (B36); RotateKey and RevokeKey do not. The CLI
  login key is unbound (clilogin.go:76). Leaf checks same user only.
- High 2 host grant: CONFIRMED as written, but DESIGN CALL, not a bug. A
  git-built tile changes code on every commit; the grant can only pin a
  digest for image tiles. Down to medium.
- High 3 lease: MOSTLY WRONG. The worker's silence clock is reset by any
  controller request (worker/worker.go:152 heardController on every call),
  not only lease pushes. A skipped push does not stop primaries elsewhere.
  What is left: a primary that starts while any other node's read fails is
  stopped (leaseOnPrimaryUp -> syncLease returns w.err), and a panel silent
  over 30 s stops HA primaries on workers (by design). Down to medium.
- High 4 promote: CONFIRMED. ANY 1 over the whole sync set
  (flow/operated/replication.go SyncStandbyNames); PromoteStandby requires
  `to`; mostAdvanced runs only with no target. Needs 2+ sync standbys.
- High 5 job cap: CONFIRMED and wider. 30 min (flow/jobs/jobs.go:188)
  covers backup, restore, restore-instance, promote-restore, pg-upgrade,
  seed. Restore passes the capped ctx to Volumes.Untar
  (flow/backup/backup.go). A volume or database that needs longer can
  never be restored.
- High 6 stop timeout: WRONG FRAMING. Runner.Close cancels every running
  job at once on shutdown (flow/jobs/jobs.go:243); the grace only waits for
  handlers to return. A longer stop timeout saves no restore. Real point:
  any panel restart mid-restore leaves the volume part-restored, re-run
  needed. Down to low.
- Standby on any node: CONFIRMED. placement.write exists as a verb
  (configplacements.go mayPlace); addStandby checks only tile.write.
- Login rows: CONFIRMED. login/handler.go Limit passes the raw form value
  to LoginRefused on every refused request, no length cap.
- Registry SSRF: CONFIRMED shape. registry.go:33 plain http.Client, no
  dial guard, realm followed. Error text visibility to members not traced.
- NFS/SMB shares: CheckShare has no grant or address check (only that
  function read).
- Not validated: shared git connector repo check; mesh drop rule (rig).

Highs after validation: 1 (key rotate), 4 (promote), 5 (job cap).

### Fix status, 2026-11-10 (local master, not pushed)

- Key rotate/revoke: an org-bound key acts only on itself; RevokeOrgKey only within its own org.
- Job cap: per-kind caps; data-moving kinds (backup, restore, restore-instance, promote-restore, pg-upgrade, rollback, seed, panel/node backup, bucket sync) get 12 h.
- Promote: refused when another live standby has a higher replay LSN or a position is unreadable (allow_async_target overrides); candidates sorted by LSN.
- Stuck failover: critical problem when the cursor holds an error or is open over 10 min, for every instance with a cursor.
- Durability settings: fsync, full_page_writes, wal_keep_size owned; synchronous_commit only on/remote_write/remote_apply. Gap: ALTER ROLE at SQL level.
- Placement: standby add, promote, switchover and stack-file standby changes need placement.write.
- Lease: list errors per node.
- Login audit: email and user agent clipped (254 / 512 bytes), join too.
- Master key / install.json: durable write (unique temp, fsync, rename, dir fsync).
- Org delete: backup destinations with runs move to server scope (read-only, delete only), aged by orphan retention, admin delete purges runs and the org's own WAL.
- Registry: dial guard against private addresses (panel's own registry allowed), https realm.
- NFS/SMB shares: need an admin host grant; editing the share drops it (one tx).
- Recover: new WireGuard key only while a rotation is marked (in memory).
- Mesh pool: prefixes wider than /24 ignored, warning problem names the node.
- Node certs: older serials revoked once a newer one is seen with key proof; renew and mesh DNS check revocation.
- Shared git connector: false for org connectors; admin sharing grants the whole installation (by design).
- Mesh drop rule: claim as written false (Docker drops unpublished ports), but published ports were reachable over the mesh. Fixed with `-i <wg> -j DROP` after STACKR-XN, rig-verified on dev.530; flows now rebuild right after a deploy.

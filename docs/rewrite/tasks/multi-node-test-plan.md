# Multi-node test plan (handoff)

Written 2026-11-11. Self-contained brief for the agent that runs it.

## Goal

Prove that a multi-node stackr install is safe to put on the developer's own
server: all nodes owned by him, his own apps. Ten stages on the test rig, each
with a pass line. The output is the results table at the bottom of this file,
filled in, plus a list of findings.

## Why these stages

Two code audits (docs/rewrite/tasks/prod-audit.md, sections "audit 2" and
"Fix status, 2026-11-10") found and fixed the multi-node problems that a code
read can find. What is left can only be shown by running it:

- Controller restore with workers attached has never been proven. It is the
  only recovery path when the controller box dies. Stage 7.
- Three isolation fixes shipped in v0.8.13; only the mesh drop rule was
  checked on the rig. Stage 3.
- Exit links are untested since the cluster address pool change. Stage 9.
- HA Postgres failover has never been tested under real node loss. Stage 10.

## Build under test

Changed 2026-11-12, after the operated Postgres DX work shipped as v0.8.14:

- Stages 1 to 7: `v0.8.14-dev.3`, built from master 07753f53, the same
  product code as the published v0.8.14 (bf937f45 only fixed tests).
  Shipped with `scripts/rig.sh ship`, then installed per
  docs/guide/install.md.
- Stage 8 upgrades to a build of local master named `v0.8.15-dev.1`, never a
  plain version, never a git tag. Build it in a temp worktree off master:
  `make release RELEASE=v0.8.15-dev.1`, then ship with `scripts/rig.sh ship`
  from that worktree (rig.sh ships the bin/ of its own checkout). Do not build
  in the main checkout (`hamr dev` runs there).
- Names that changed since the plan was written: `stackr db ...` is now
  `stackr operated ...` (alias `opr`); `db promote` is `opr failover`;
  `tile promote-restore` is `opr accept-restore`; `tile pitr` is
  `opr pitr set|status`; standbys list and remove are `ls` and `rm`.

## The rig

| Node | Address | Role | Access |
|---|---|---|---|
| ct 109 | 192.168.1.109 | controller, panel at https://stackr-lxc.vulpe.dev | `ssh root@192.168.1.109` |
| ct 110 | 192.168.1.110 | worker | `ssh root@192.168.1.110` |
| vm 106 | 192.168.1.106 | worker, also hosts the rig S3 | user `darthvader`, docker group, no root ssh |
| h4 | 77.42.15.115 (private 10.0.0.3) | worker, public ingress, `*.h4.vulpe.dev` | `ssh root@77.42.15.115` |
| h3 | private 10.0.0.2 only | worker with no public IP, egress through h4 | `ssh -J root@77.42.15.115 root@10.0.0.2` |

- h3 has no public address on purpose. Its public IP not answering is
  expected. Never report it as down.
- If the laptop has no route to 192.168.1.x, jump through vm106's Tailscale
  name: `ssh -J root@stackr-test.inanga-degree.ts.net root@192.168.1.109`.
  Tailscale is for access only. Never enter a tailnet address or range into
  any stackr setting. The rig's real front proxy is 192.168.1.100/32.
- Rig S3: rustfs container `stackr-s3` on vm106, endpoint
  `http://192.168.1.106:9100`, bucket `stackr-backups`. Read its keys with
  `docker inspect stackr-s3` on vm106 into a file. It is not stackr-labelled,
  so removing stackr from vm106 leaves it. Do not delete its volume
  `stackr-s3-data` before stage 7 is done.
- Panel login: `admin@test.com`, password is `UNIVERSAL_STACKR_PASSWORD` in
  the repo `.env`.
- Admin CLI on the controller:
  `STACKR_CONFIG=/var/lib/stackr/cli.json /root/stackr-cli`.
- `scripts/rig.sh` (read its header): `save`, `ship`, `pave`, `upgrade`,
  `restore`, `snapshots`.
- Test repos for git-built tiles: FyrmForge/stackr-test and
  FyrmForge/stackr-qa-blitz. Pushing to them is fine. Clean up branches and
  PRs after.
- Guides to follow as a user would: docs/guide/install.md, nodes.md,
  placements.md, backups.md, upgrade.md, operated.md, remove.md,
  troubleshooting.md. A step that the guide gets wrong is a finding.

## Hard rules

- Never touch the Proxmox host (192.168.1.99): no ssh, no pct/qm. Only the
  nodes in the table. A test that needs a host change stops and goes to the
  developer.
- Never print a secret (password, key, token, recovery passphrase, S3 keys)
  to output or into this file. Read them into files in your scratch dir and
  pass paths.
- No git write to the stackr repo's master and no push. Fixes go on a branch
  in a worktree (see Findings).
- DNS: tests may change records only on fyrmforge.dev. Never vulpe.dev or any
  other zone. Namecheap rewrites a whole zone on every write.
- A new hostname served from the controller (109) needs a write on the
  developer's front proxy, which only he can do. Use hostnames under
  `*.h4.vulpe.dev` for every site in this plan. A controller-hosted name that
  does not resolve is not a stackr bug.
- Before any wipe: `scripts/rig.sh save`. It keeps the GitHub connector row,
  which cannot be recreated without the developer in a browser. After the
  fresh install and first org: `scripts/rig.sh restore --org <slug>`.
- The rig is disposable. Wipe, pave and reinstall it without asking.
- Panel checks run in a visible browser (headed Playwright scripts in Node;
  launch options come from `~/.playwright-mcp.json`). Screenshots stay out of
  the repo.
- Do each action in the panel first. Use the CLI only where the plan says so
  or the panel has no such action (note that as a finding).
- Do not run golangci-lint. Tests, if you run any: niced and core-capped.
- Run the stages in order. Stage 7 destroys the controller on purpose, so
  stages 2 to 6 must have left real data on the workers first.

## Stages

### 1. Install and join

1. `scripts/rig.sh save`. Remove stackr from all four workers
   (docs/guide/remove.md), wipe the controller.
2. Install v0.8.13 on 109 per docs/guide/install.md. Save the recovery
   passphrase to a file in your scratch dir. Stage 7 needs it.
3. Finish the setup wizard, create org `mn`, restore the GitHub connector.
4. Join ct110, vm106, h4, h3 with the panel's Add node command. Tag h4 as
   ingress and one LAN worker as builder.
5. Add the rig S3 as a backup destination and set it as the panel backup
   destination. Set an alert webhook to a catcher you control.

Pass: four workers online on the map; every link shows a recent handshake;
h3's link runs through h4; no problem rows.

### 2. Place and deploy

1. Placements: one per worker. Stack `shop`, one env pinned to each of ct110,
   vm106, h4.
2. On ct110: an image tile (nginx). On vm106: a managed Postgres with a
   table and a few rows. On h4: a git-built tile from FyrmForge/stackr-test
   with a domain under `*.h4.vulpe.dev`.
3. Link an app tile on ct110 to the Postgres on vm106 and have it read the
   rows.
4. On h3: any tile that makes an outbound HTTP call.

Pass: the h4 site loads over HTTPS with a real certificate; the cross-node
read returns the rows; the build ran on the builder node; h3's tile reaches
the internet.

### 3. Isolation (the three v0.8.13 fixes)

1. Publish a port on a tile on ct110. From vm106's host, dial it at ct110's
   mesh address.
2. Call the node recover endpoint for a worker, validly signed with its node
   key, with no WireGuard rotation in progress (service/recover.go).
3. Make one worker report a prefix wider than /24 as taken
   (service/mesh.go). Then join or re-address a node.

Pass: 1 refused or times out, while the tile's own linked peer still
connects. 2 refused. 3 the wide prefix is ignored, a warning problem names
the node, addressing still works.

### 4. Worker loss

1. Power off ct110 (shut the container down from inside, not from Proxmox).
   Wait five minutes. Start it by `ssh` if it is reachable, else ask the
   developer to start it.
2. Reboot h4 with `reboot`.

Pass: node-down banner and a webhook alert within the alert window; the
panel shows ct110's tiles as down, not stale-healthy; after boot every tile
returns with no action; the tenant firewall rules are back (repeat stage 3
step 1); the h4 site answers again.

### 5. Controller loss

1. Stop the `stackr` container on 109 for ten minutes. Leave the workers
   alone.
2. During the stop: load the h4 site, run the cross-node read, kill a tile
   container on ct110 by hand.
3. Start the panel.

Pass: site and cross-node read work throughout; the killed container
restarts without the controller; after the panel returns all nodes go online
with no rejoin and a deploy to each worker succeeds. Record anything that
stopped working during the outage, even if expected.

### 6. Backups on workers

1. Back up now: a volume on ct110 and the Postgres on vm106, to the rig S3.
2. Change the data. Restore both. Compare.
3. Give one volume a schedule a few minutes out and let it fire.
4. Take a panel backup to the rig S3.

Pass: restored data equals the backed-up data; the scheduled run appears as
succeeded; the panel backup object exists in the bucket.

### 7. Controller restore with workers attached

The stage that matters most.

1. Note what exists: nodes, placements, tiles, domains, backup runs, the
   Postgres rows.
2. Take a fresh panel backup to the rig S3. Then on 109 remove the panel
   containers and move the whole data dir aside (keep the copy until the
   stage passes). Workers keep running.
3. Restore on 109 from the S3 backup alone, with the recovery passphrase
   from stage 1 (docs/guide/backups.md, installer restore).
4. Check without touching the workers.

Pass: all four workers reconnect with no rejoin and no new certificate by
hand; tiles kept serving during the restore; a deploy to each worker works;
a volume archive taken in stage 6 still restores (archive keys survived);
the h4 site and its certificate are intact; nothing made after the backup is
shown as existing.

If it fails: write down exactly where, keep the moved-aside data dir, stop
the stage. Do not hand-repair the DB to get a pass.

### 8. Upgrade and rollback

1. Build and ship `v0.8.14-dev.1` (see Build under test). A rig on a plain
   release takes its first dev build with `scripts/rig.sh upgrade`; if the
   Update tab offers it, use the tab instead.
2. Roll the workers from the panel.
3. Failed trial: build `v0.8.14-dev.2` from a throwaway branch whose panel
   fails its health check, upgrade to it from the Update tab.

Pass: 2 leaves every node on dev.1 with tiles serving throughout, and the
maintenance banner shows during the rollout. 3 ends with the panel back on
dev.1, the job failed with "Rolled back", the proxy container on the dev.1
image, the dev.2 tag gone from the registry, and no worker on dev.2.

### 9. Keys, certificates, nodes, exits

1. Rotate WireGuard keys (panel button). Renew one node certificate. Roll
   the cluster CA (docs/rewrite/tasks/ca-rollover.md for what to expect).
2. Remove ct110 from the cluster, check the box, join it again.
3. Tag h4 as exit and route the h3 tile's outbound traffic through it.

Pass: 1 the cross-node read loops through all three with no failed request,
and the old node certificate is refused afterwards. 2 the removed box has no
stackr containers, networks or WireGuard interface, and rejoins cleanly with
its tiles redeployable. 3 the tile's outbound address is h4's public IP.

### 10. HA Postgres under node loss

1. Operated Postgres: primary on ct110, sync standbys on vm106 and h4
   (docs/guide/operated.md). Run a write loop that records every confirmed
   insert id on the client side.
2. Stop ct110's node. Wait for failover. Bring it back.
3. Cut the new primary off from the mesh only (block WireGuard on that box),
   then heal.
4. Switch over from the panel.
5. Pause replay on one standby so it lags, then promote it by name.
6. Rebuild a standby from the panel.

Pass: after every step each confirmed id is present on the primary; exactly
one primary at all times (the cut-off one stops itself); 5 is refused
without the override; the old primary returns as a standby; a failover that
gets stuck raises a critical problem within ten minutes.

## Findings

- Record every failure with: stage and step, what you did, what you saw,
  what you expected, the job id or log line, the node.
- Check a suspected bug in the code before writing it down. Name the file
  and function.
- A confirmed bug with a clear fix: fix it on a branch in a git worktree,
  commit there by file name (never `commit -a`), one-line message, no AI
  attribution. Do not merge to master, do not push. Retest on a new
  `-dev.N` build and say so in the finding.
- Anything that is a design question goes in the list unfixed.
- A stage that cannot run (missing access, needs the Proxmox host, needs the
  front proxy) is marked blocked with the reason. Never mark it passed.

## Results

Fill in. One line per stage, then the findings list.

| # | Stage | Result | Build | Notes |
|---|---|---|---|---|
| 1 | Install and join | PASS | v0.8.14-dev.3 | h3 needed NAT on h4 (rig, not stackr); dev image must be preloaded on each worker |
| 2 | Place and deploy | PASS | v0.8.14-dev.3 | build ran on ct110 only after the controller's builder tag was removed (by design: controller builds by default) |
| 3 | Isolation | PASS | v0.8.14-dev.3 | stale wide-prefix warning (fixed) |
| 4 | Worker loss | PASS | v0.8.14-dev.3 | offline alert +44s, containers back without action |
| 5 | Controller loss | PASS | v0.8.14-dev.3 | tiles serve; cron, alerts, reconcile, PG failover, PITR shipping, cert loops stop while the controller is down (design, parked) |
| 6 | Backups on workers | PASS | v0.8.14-dev.3 | PG dump restore needed a hand fix (fixed) |
| 7 | Controller restore | PASS | v0.8.14-dev.3 | installer panic and leftover networks on same-box recovery (fixed) |
| 8 | Upgrade and rollback | PARTIAL | v0.8.15-dev.1 | tiles served throughout; proxy not rolled back (fixed); panel Update path untestable on dev builds |
| 9 | Keys, certs, nodes, exits | PASS | v0.8.15-dev.1 | rollback had deleted the running image (fixed); no panel action to renew one node cert |
| 10 | HA Postgres | PARTIAL | v0.8.15-dev.1 | 0 of 3854 confirmed writes lost; failover unreachable in panel while primary node offline (fixed); old primary stuck once (open); stuck-failover alert not testable |

### Findings

Fixed and pushed. Retested on v0.8.15-dev.3 and v0.8.16-dev.1 (see Retest below):

- 3: wide-prefix problem only re-checked on a new env network, message showed the host address. internal/service/mesh.go. 079548d6.
- 3: `tile exec` without `-i` waited on stdin, then 60s Bad Gateway. CLI + internal/api/handler/v1/streams.go. 235339d5.
- 6: PG dump restore failed on the pooler schema. flow/operated/postgres.go pgWipe, flow/backup/backup.go rollback. b8b3ffd3.
- 6: two default backup schedules under concurrency; restore flags missing in docs/guide/backups.md. cf05ac61.
- 7: stackr-install panic (cmd/stackr-install/main.go controllerPorts) and refusal on leftover empty stackr networks. cf05ac61.
- 8: a confirm opener clicked before the script upgraded it was lost. internal/ui/components/confirm.templ. fe1704e3.
- 8: an open confirm dialog closed by an in-flight drawer poll; backup history poll had no guard. ui/static/js/main.js, internal/ui/drawer/volume/volume.templ. 5cdfeabb.
- 8: failed installer panel swap left stackr-proxy on the new image. cmd/stackr-install/upgrade.go. 038ee36e.
- 9: boot rollback undid the newest upgrade job, not the failed trial, and deleted the running version's image from the cluster registry. internal/service/upgraderollback.go. fa9e99b3.
- 9: refused cert renewal said "not a node of this cluster". internal/service/certrenew.go. f4987cc7.
- 9: Rotate all now had no confirm; a second click ran a second round. internal/service/rotate.go, internal/ui/drawer/admin/keys.templ. eff77789.
- 10: env page and instance drawer 503 while the primary's node is offline, hiding Failover. flow/graph/graph.go, internal/service/tile.go. 3dca6d3c.
- 10: deleting an operated tile after failover left the primary on the failover node. internal/service/jobs.go runDelete, failover.go dropRelocated. c9db750d.
- 10: Rebuild of a stuck old primary failed "volume is in use". internal/service/members.go dropMember. c63e5e62.
- 1: docs/guide/nodes.md said to load the image by digest; it is by tag. 3b4ec750.
- 1: scripts/rig.sh pave kept keys/recovery.pub. 3b4ec750.
- 1: a join nobody confirms stayed pending forever; now rejected after 24h. 7ff5ca42.
- 10: rejoin and member removal attempts in the watch tick have deadlines. 0930d23b.
- Retest R1: Rotate all now stranded ct110, h4, h3 (recover blocked on a peer push, worker reverted, panel refused its old key). internal/service/recover.go, rotate.go. df1c4542.
- Retest R2 (root cause of the stuck old primary): failover with the old node offline left the member subnet empty, so no flow was built. internal/service/failover.go fillSubnet. 898dad80.
- Retest R5: a standby stuck in starting over 5 min gets Rebuild. a61d4256.
- Recheck R6: a standby add that failed before the primary ran left a member stuck "removing". internal/service/internal/flow/operated/replication.go SyncReplHBA. eb47fa89.
- Test flakes: swap test timeout (d433b489), DNS rescope was a real race in dnscap Reconcile (8f42d815), problems order (f82b3b60).

Rotate all with a live HA Postgres (v0.8.16-dev.1): PASS. One primary throughout, 0 of 1486 confirmed writes lost, writes from h3 stalled ~2 min while h3/h4 rotated, h3 offline ~40s, one /api/v1/cluster 500 during h4's push.

Retest: v0.8.15-dev.3 PARTIAL (found R1, R2); v0.8.16-dev.1 PASS (rotate all: no node stranded, h3 ~60s offline; failover with old node offline: old primary streaming ~2 min after its node returned; relocated data volume listed as orphan). R5 and R6 not exercised on the rig.

Open:

- 8: the panel Update path (and its "Rolled back" job) cannot run on dev builds, so the rig cannot test it.
- 8: a failed version's tag stays in the cluster registry after an installer-driven trial.
- 9: no panel action to renew one node certificate; worker renew-now cannot force one (by design: a stolen cert must not mint fresh ones).
- 9: key rotation of a node reached through another (h3 via h4) takes it offline about 60s (was minutes before df1c4542); /api/v1/cluster 500 seen once during rotation, not rechecked.
- 10: failover recreates consumer tiles (about 1 min of failed writes); primary-down alert arrives about 5 min after a node dies.
- 10: stuck-failover critical problem not exercised (no failover got stuck).
- 5: everything scheduled runs in the controller (cron, functions, alerts, reconcile, PG failover, PITR, certs). Parked as the "workers run their own work" RFC.

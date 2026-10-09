# Review fixes, round 3: build plan

Status: built 2026-10-09, on the rig as v0.6.0-dev.27.

Source: second Codex review plus two Opus reviewers over the round-2 fixes
(`review-fixes-2.md`). All confirmed in the code.

## Must fix
1. Restore over another stack's volume. `RestoreBackup`
   (`internal/service/backup.go`) checks `target_volume_id` (request body)
   against the org only, so a stack key restores its backup over another
   stack's live volume. Fix: the target must be in the source volume's stack
   (or the principal's stack), checked in the service.
2. A recreated proxy joins no ingress networks. Only panel boot runs
   `ReopenIngress`; an installer re-run (panel kept) or the upgrade's
   `EnsureProxy` leaves every domain at 502 until a panel restart. Fix, one
   place: `watchTick` already re-pushes the config when the proxy's start
   time changes; run `ReopenIngress` there too.
3. Proxy replace has no rollback (Codex P1). `EnsureProxy` and the installer
   remove the old proxy before the new one runs; a failed run leaves no
   proxy, and later upgrades skip a missing proxy. Fix: rename and stop the
   old one, run the new one, on failure remove the new one and rename and
   start the old one back; remove the old one only on success.
4. Early rule race. `sweepEarly` can re-add a rule the off path just cleared
   (and the on path can land a stale rule after a revoke sweep). Fix: the
   sweep's and the on path's `Allow` happen under `earlyMu` after re-checking
   the id is still in `earlyIPs`; an entry leaves `earlyIPs` when its replica
   is routed by a VIP.
5. `dropTileGrant` revokes then re-approves (gap, lost lines on a failed
   approve, ignored read error, row recreated). Fix: compute the final line
   set for the slug and write it once, only when it changes; a read error
   aborts.
6. Stack keys cannot follow image-check and restore jobs (payload keys the
   job placement does not read), nor a tile delete job after the row is
   gone. Fix: stamp `stack_id` into every job payload at enqueue (as
   `withOrg` stamps `org_id`) and place jobs by it.
7. Proxy recreated by the panel publishes 80/443 on IPv4 only
   (`docker/containers.go` portBindings `0.0.0.0`). Fix: bind both families
   for the proxy, matching the installer's `-p`.

## Lower
8. Check order: on org-level child routes a stack key gets 403 for a missing
   id and 404 for another stack's real id; return the child error first.
9. Drop the release exception on `/promote|rollback|plan/:release`: a foreign
   release is a 404 (the plan blocker only gave a nicer message and leaked
   the release number).
10. Stack drawer Promote always 404s: `canvas/releases.go` puts the env id in
    `:env`, which resolves by slug. Use the slug.
11. `stream.Check` passes a malformed handshake (Codex P2): also require GET,
    `Sec-WebSocket-Version: 13` and a `Sec-WebSocket-Key`.
12. Installer `--dry-run` prints the live DNS-01 token read back from the
    proxy; mask it.
13. A PR event matches `pr-N` by slug only; also require `Type == Ephemeral`.

## Not fixing
- First self-upgrade from a release that predates the socket runs the old
  binary's flow, so the proxy moves on the second upgrade (Codex P1). No
  install predates it (no users); the rig upgrades by reinstall.

## Lanes
- A auth: 1, 6, 8, 9, 10 (`backup.go`, `orgof.go`, `jobs.go` enqueue,
  `middleware/access.go`, `canvas/releases.go`).
- B proxy: 2, 3, 7, 12 (`jobs.go` watchTick, `flow/upgrade`,
  `cmd/stackr-install`, `docker/containers.go`).
- C early and grants: 4, 5, 13 (`vipboot.go`, `hostgrantwaiting.go`,
  `leaf/hostgrant`, `promote/push.go`).
- D handshake: 11 (`api/stream/ws.go`).

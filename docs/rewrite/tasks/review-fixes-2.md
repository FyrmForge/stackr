# Review fixes, round 2: build plan

Status: built 2026-10-09, on the rig as v0.6.0-dev.26, QA passed.

Source: Codex review (`codex review --uncommitted`) plus four Opus reviewers
over the uncommitted review-fixes and roles work (dev.25). Every item below
was confirmed in the code. Codex found 4; all 4 are also in the Opus list.

## Must fix (security and breakage)
1. Web routes take API keys. `middleware.Access.Load` runs on the web
   `site` group (`internal/web/server.go`), so a bearer key reaches every web
   POST, and the capped-key guards live only in the API handlers. A viewer
   key mints an uncapped key through `/:org/-/drawer/keys` or
   `/cli/authorize/:org` (Codex P1, Opus high x2). Fix: the web group is
   session only; a bearer key there is a 401. Keep the guards in the service
   too (`MintKey`, `CLICode` take the principal) so no surface can skip them.
2. Stack keys reach other stacks by id. `middleware/access.go children()`
   checks only that a child id is in the org; domain update and detach,
   release get, volume actions and job events act on any id in the org.
   Fix: `children()` checks each child id against its route parent (tile,
   env, stack) and fills `Resource.StackID` from the row on org-level job
   and volume routes, so a stack key follows its own deploy (`stackr deploy`
   with a stack key 403s on the job stream today) and is refused elsewhere.
3. Early LAN rules leak (Codex P1, Opus high). `earlyLan` re-inspects the
   container on the way off; Docker has cleared its IP by then, so every run
   and failed replica leaves a `-s ip -j RETURN` that a later container with
   that IP inherits; revoke does not touch them. Fix: `Early` returns an undo
   holding the IP; the rollout's failure `Remove` path calls it; the rebuild
   drops early entries whose container is gone or whose grant is revoked.
4. PR push builds static envs. `promote/push.go`: with `ev.PR` it still
   matches every env on that branch, so a fork PR with head ref `main`
   builds into dev and auto-promotes. Fix: `ev.PR` targets only `pr-<n>`.
5. PR env grant guard is plan-only. Sync into pr-N or a hand-made tile there
   rides the slug grant. Fix: one guard in `deploy.resolve`/`checkAccess`
   (it has the env): an ephemeral env never gets grant flags.
6. Grants not dropped on the config-as-code path. Tiles removed by a promote
   or sync plan keep their lines. Fix: the plan's removed tiles go through
   `dropTileGrant`; `dropTileGrant` revokes every line no surviving tile of
   that slug still asks for (not "skip if the slug exists"); a rename cancels
   that tile's waiting deploy.
7. Deleting a tile cancels unrelated jobs. `cancelWaitingFor` matches any
   waiting job whose lock set holds the tile, so a parked promote of the whole
   env dies. Fix: cancel only tile jobs of that tile.
8. Self-upgrade never recreates Caddy (Codex P1, Opus high). The panel moves
   to the admin socket, the old proxy has no mount and still listens on
   :2019, so every push fails and the old hole stays. The rig hid it
   (`rig.sh upgrade` removes both). Fix: the upgrade and the installer
   recreate `stackr-proxy` when its spec differs (sites blip for seconds).

## Lower
9. `StopRun` matches the conflict by its text; use a typed sentinel (Codex P2).
10. The hang-up uses `/bin/kill`, missing on Debian-slim; use the shell
    builtin (`sh -c 'kill -HUP "$1"'`). No `sh` in the image: a clear error,
    not `strconv.Atoi`.
11. Deploy's `route` sets lan rules outside `vipMu`, so a revoke can come
    back for a minute; take the lock.
12. PR closed path deletes the env without `DeleteEnv`, so its parked jobs
    stay; go through it.
13. A capped or stack key can revoke the user's other keys (`/me/keys`);
    such a key may revoke only itself.
14. Org drawer domains tab shows domain resources (ACME email) to viewers;
    gate on `domain.resource` like the API.
15. An unknown stored key `level` parses as no ceiling; fail closed.
16. Export warns about `pr_envs` only when a PR env exists; warn whenever the
    stack has a config repo.
17. goimports grouping in `orgconfig/export.go`, `serverconfig/export.go`.

## Not fixing
- Bidi and zero-width characters in release messages (display only).
- Early rules lost on a panel restart (a run loses LAN for the rest of its
  run: fails closed).
- Running bridge tiles keep NET_RAW until their next deploy.
- A hand-made `type: ephemeral` env never builds (no users make one).

## Lanes
- W1 auth: 1, 2, 13, 14, 15 (`internal/web/server.go`, `middleware/access.go`,
  `service/access.go`, `admin.go`, `clilogin.go`, `api/handler/v1/orgs.go`,
  `web/handler/canvas/org.go`, `web/handler/auth/cliauth`, tests).
- W2 firewall: 3, 11 (`vipboot.go`, `vip/vip.go`, `leaf/tile/{tile,world}.go`,
  `flow/deploy/deploy.go` route and rollout Remove).
- W3 grants and PR: 4, 5, 6, 7, 12 (`promote/push.go`, `deploy/mount_host.go`
  or `resolve`, `hostgrantwaiting.go`, `jobs.go`, `tile.go`).
- W4 upgrade: 8 (`flow/upgrade/upgrade.go`, `cmd/stackr-install/main.go`,
  `installspec`).
- W5 small: 9, 10, 16, 17 (`run.go`, `flow/jobs/jobs.go`, `docker/exec.go`,
  `promote/export.go`, the two export files).

## Ship and QA
Release dev.26, rig upgrade through the panel's own self-upgrade path (not
`rig.sh`) to prove 8. Probes: a viewer key gets 401 on a web POST; a stack
key follows its own deploy and 404s another stack's domain id; a cron with
a LAN grant leaves no rule after its run; revoke clears a running job's rule.

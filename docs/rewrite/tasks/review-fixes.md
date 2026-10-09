# Review fixes: build plan

Status: built 2026-10-09, on the rig as v0.6.0-dev.24.

Source: code review of d3f0f93, d4985ec, f3da00b (five reviewers, findings
checked in the code). Paths are repo-relative; `flow/`, `leaf/`, `store/`,
`vip/`, `docker/` are under `internal/service/internal/`.

## How the work runs
Same rules as `leftovers.md`: one worker per lane, no two lanes share a
function, generated files regenerated never edited, every lane green on
`make test`, `make lint`, `make templint`, one test per behaviour change, no
git write ops, no em dashes in copy.

## Decided with darthvader (2026-10-09)
1. PR envs never get elevated grants. A tile in a PR env that needs a grant is
   skipped there with a note in the job log.
2. Fork pull requests are skipped. `pr_envs.forks: true` in the stack file
   opts in.
3. Deleting a tile removes its grant lines. Renaming one drops them too; the
   new name asks the admin again.
4. `lan:all` is gone. Any address or range is allowed, `0.0.0.0/0` included,
   and opens exactly what it covers: the server's own ports and the panel
   too. The approve screen shows the masked range (`192.168.1.5/24` reads
   `192.168.1.0/24`) and says in plain words when it covers the server, the
   panel, the cloud metadata address or the tailnet range.
5. Bridge tiles drop `NET_RAW` (stops address spoofing past LAN grants).
   Host-network and privileged tiles keep it. No separate grant
   (darthvader 2026-10-09: skip the toggle).

## Lanes (all in parallel)

### L1. Firewall (`vip/`, `internal/service/vipboot.go`, `orchestrator.go`
proxy admin, `cmd/stackrd/proxy.go`, `flow/deploy/deploy.go` route timing,
`leaf/tile/world.go` run role)
- Caddy admin API off the tile networks: tiles on a `stackr-ingress-*`
  network reach Caddy on :2019 with no auth and can rewrite routes to the
  panel or the LAN. Serve the admin API on a unix socket in a host directory
  the panel mounts, so no network reaches it.
- Panel lock always on: drop 10.213/16 to the panel port whatever Caddy's IP
  is, after the grant returns (decision 4). Panel port from the panel's
  `PORT`, not `installspec.PanelPort`.
- Rules survive a Docker restart: jump from `DOCKER-USER`, not position 1 of
  `FORWARD`.
- One rebuild at a time: the tick and the async revoke rebuild share a lock
  and read grants inside it, so a revoke cannot come back.
- Stale entries go: a deleted tile or one with no replicas is removed even
  when another tile fails to read.
- A replica's LAN rule exists before its health gate, so an app that needs
  the LAN at start passes the gate.
- Run containers (cron, job, one-off) get their tile's LAN rules.
- `lan:all` handling removed; grant ranges are applied masked, to FORWARD and
  INPUT.
Tests: filter rendering for each rule above; rig probe (L6).

### L2. Grants (`leaf/hostgrant/`, `internal/service/hostgrant.go`,
`hostgrantwaiting.go`, `tile.go` delete and rename, `leaf/tile/check.go`
ParseLAN, `internal/ui/access/`, `docker/containers.go`, `flow/deploy/spec.go`)
- Decision 3: tile delete removes its lines, rename drops them.
- Waiting jobs of a deleted tile, env or stack are cancelled; the admin list
  and the badge skip a grant whose stack is gone instead of failing.
- Decision 4: ParseLAN drops `all`, accepts any range, stores it masked; the
  approve screen warnings.
- Decision 5: `CapDrop NET_RAW` on bridge tiles. No `cap:net_raw` perm (darthvader 2026-10-09: host networking or privileged keeps it).

### L3. PR envs (`githubapp/webhook.go`, `internal/service/jobs.go` runPR,
`flow/promote/stackfile.go`, `flow/promote/push.go`)
- Decision 2: read `head.repo.full_name`; differs from the repo and
  `pr_envs.forks` is not true, skip with a log line.
- Decision 1: a PR env skips tiles that need a grant.
- PR envs off: a push no longer redeploys an existing pr-N env.
- `pr_envs`, `against:` and `tiles:` in an included file are refused, not
  dropped silently.

### L4. Terminal and forward (`internal/api/handler/v1/streams.go`,
`internal/api/stream/ws.go`, `internal/service/tile.go` Shell finish,
`docker/exec.go`, `ui/ts/term-pane.ts`)
- Check the upgrade and the origin before the exec or the dial. Today a
  cross-site link runs `?shell=<cmd>` in a tile before the check refuses it.
- Kill the shell when the client goes (exec inspect gives the pid).
- Forward: read limit 1 MiB per frame; keep reading after the eof frame so
  pings still get their pong.
- Terminal: read limit 1 MiB; the page splits a large paste into frames.

### L5. Export, runs, small fixes (`flow/promote/export.go`,
`internal/service/run.go`, `planfile/planfile.go`,
`flow/orgconfig/export.go`, `flow/serverconfig/export.go`,
`leaf/release/release.go`, `internal/service/stackexport.go`, `go.mod`)
- Export: a stack param an env overrides is declared without a value (or left
  out when the kinds differ), like the env-only branch. Test with stack
  `app.region=eu`, prd `us`: the exported file plans clean on every env.
- `StopRun` treats "the job already finished" as done and closes the row.
- `generate:` refused in org and server files.
- Protect password: anything not purely a `${{ }}` ref is left out of every
  export.
- Release message: control characters stripped in `SetMessage`.
- Export of a PR env names it ("a PR env is not on the ladder").
- Export warns that `pr_envs` is not carried.
- `golang.org/x/net` to v0.60.0 (five advisories).

## Not in this plan
- Grant approver per line (the row keeps one approver): audit gap, later.
- Shared and managed networks link envs that share an instance: predates
  this work, later.
- Docker client advisories: no fixed version yet.

## Ship and QA
Integration, then `make release RELEASE=v0.6.0-dev.24`, rig upgrade. L6 rig
probe: from a tile with a domain, Caddy :2019 is shut; panel shut from every
env; a revoke stays revoked over two ticks; `docker restart` leaves the
chains in front; a cross-origin terminal GET runs nothing; a dropped
terminal leaves no shell in `ps`; stack export of an override plans clean;
fork PR skipped (unit test, needs GitHub).

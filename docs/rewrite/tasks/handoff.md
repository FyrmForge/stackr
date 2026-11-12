# Handoff: where to pick up

Last updated 2026-11-02. Overwrite this file at the end of each session.

## State

- master pushed 2026-11-01 at a58686d8 (release.yml cuts a 0.8.x patch
  after CI). Push only when the developer says so. Releases stay on 0.8.x.
- The rig runs v0.8.8-dev.13. Commits after 6ce0f482 (the rig QA
  follow-ups merge ca544f1a) are not on the rig yet. Rig builds: `make
  release RELEASE=v0.8.8-dev.N` (make.run cannot pass RELEASE: build in a
  temp worktree off master), then `scripts/rig.sh upgrade 0.8.8-dev.N`. The
  laptop may have no route to 192.168.1.x; jump through the rig's Tailscale
  name for access only.
- ~/go/bin/templ is v0.3.1100 now (it was v0.3.1001 and hamr dev
  regenerated every *_templ.go in the old format).
- Second opinions: Codex only (developer dropped OpenCode). Codex tends
  to hang after writing findings: wrap it in `timeout`.

## Built this session (all merged, rig QA passed on dev.12/13)

- DNS RFC all three parts (fraedi, Projects/stackr/rfc): Change scope
  (3fd3c532), custom records (fa224b3c, migration 040), Managed DNS page
  (576725d4).
- Maintenance mode during panel upgrades and worker rollouts, rebuild label
  in jobs lists (5e4aadac). The panel-upgrade path (banner, Update tab line)
  cannot be tested from a dev build.
- Create a managed postgres from a dump (7fb6abb5), drawers show the last
  job (8bffc743).

## In flight (2026-11-02 late)

- All fix batches MERGED into local master (not pushed): stackr-13 review
  (R1-R20), failed-upgrade rollback (U1-U3 + clash fix), audit data/backup,
  audit security, audit ops/CI/docs, whole-repo highs, web, CLI, API,
  config, ops/security lows. Full `go test ./...` and `make lint` pass on
  master. Migrations 041-045 added.
- Uncommitted for the developer: templ pin (go.mod, go.sum, Makefile,
  hamr.toml, README.md, AGENTS.md, CLAUDE.md, ci.yml) and regenerated
  docs/openapi.json.
- Codex out of credits since ~22:30: most batches had no second opinion.
  Rerun reviews when topped up.
- Running: rig test of everything (dev build, upgrade, rollback, W1, W5,
  SEC5, builder net, grants, cert renewal, backups, CSP) and headed browser
  QA on local dev.
- Decisions/leftovers per item: docs/rewrite/tasks/prod-audit.md (bottom
  sections) and docs/rewrite/tasks/review-13/BATCHES.md.

## Known, not fixed

- A store failure mid-rescope leaves it half done (re-running finishes it).
- Seed: no upload size limit (trust model); superuser-only extensions fail.
- govulncheck: GO-2026-4887 and GO-2026-4883 in docker/docker v28.5.2
  (daemon plugin code), no fix released.

## Parked (raise when planning, don't build unasked)

- DB branching: create a managed DB from a backup run or another tile. The
  seed job's source (`seedSource` in internal/service/seed.go) is the hook.
- "Move an app in" from outside stackr: dropped, don't raise.

## Context worth knowing

- **DNS testing:** only fyrmforge.dev may be changed by tests. Never touch
  the rig panel's own domain or any other domain without the developer's
  explicit go in that moment. Namecheap rewrites the whole zone on every
  write: snapshot the zone (getHosts, read-only) before and diff after.
  fyrmforge.dev's mail setting is Email Forwarding; leave it that way.
- **htmx 2.0.4:** hx-vals is merged from every ancestor and ignores
  hx-disinherit. A form inside an element with hx-vals needs
  `hx-vals="unset"` to post only its own fields; never render
  `hx-vals="null"`.
- **Local dev nodes run on the fake host**, which cannot exec: seed loads
  and anything using tile exec must be tested on the rig.
- **Builds in the main checkout** go through the hamr dev MCP (make.run,
  rule.run). Helpers work in worktrees and use `go test ./...` there.

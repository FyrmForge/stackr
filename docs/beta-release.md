# Beta release

Status: open, **phase 1 done**. Revised 2026-09-14 after validating every
gate item against the code. Four phases, in order. Each phase depends on the
one before it.

## What changed in this revision

Two items came off the gate list:

- **Promote and rollback over the API already exist.** The API exposes both
  operations and the CLI drives them, so CI can drive the ladder today.
- **The migration policy is dropped.** It only protects installs that
  hold data someone cares about. There are none outside the rig.

One item is parked, not resolved: whether node groups are enough
isolation for a shared org, or whether it gets its own install. Groups
are a scheduling constraint, not a boundary. Not a blocker for beta.

---

## Phase 1: the gate — done 2026-09-14

All three built and verified on the rig, including a wipe and redeploy.
The compact implementation record is `docs/plans/41-beta-gate.md`.

1. **Panel backup carries the master key, and a restore script exists.**
   Plan 38. Today a panel backup is `stackr.db.gz` alone
   (`infra/backup/backup.go:266`) while every secret in it is ciphertext
   under a key the archive does not contain. Restore on a new host and
   every password, deploy key and app secret is gone, with the backup
   reporting success. Restore is refused outright at `backup.go:571`.
2. **Org creation gated to server admins.** `web/server.go:297` `POST
   /orgs` takes any signed-in user. Admins already have the power to add
   any user to any org (`org/members.go:32`), so no new flow is needed.
3. **Cookie `Secure` keyed off TLS, not `DEV_MODE`.** `STACKR_TLS=off`
   reaches only `proxy.New` (`main.go:339`); cookies key off `DEV_MODE`
   (`main.go:213`, `web/server.go:154-155`). A LAN install serves Secure
   cookies over plain HTTP. Reproduce on the rig before fixing.

Exit condition: a panel backup restored onto a fresh host with secrets
intact, done once for real.

## Phase 2: going public — done 2026-09-14

- Full-history and working-tree secret scans completed before the rewrite.
  Tracked history was clean; runtime keys remained ignored and untracked.
- Private hostnames, addresses and test credentials were replaced with
  examples. Developer-only deployment helpers moved to `scripts/dev/`.
- Completed plans became compact records; active plans kept their detail.
  QA artifacts, stale reviews, copied framework docs and superseded design
  documents were removed. `docs/notes.md` now contains open work only.
- Historical ticket markers and implementation-story comments were removed or
  rewritten as current rules.
- The code audit removed unused helpers, an unused browser dependency and the
  PostgreSQL brand asset. Remaining suggested abstractions were rejected where
  they would add coupling or remove test seams.
- `semgrep` scanned 494 tracked files with 328 community rules. Its remaining
  findings are intentional or contextual: privileged Docker control-plane
  containers, transport inside the authenticated private overlay, conditional
  cookie security, value-return error paths, an incremental map accumulator,
  and streaming an operator-selected backup directly into its restore process.
- `govulncheck` reported two Docker daemon plugin issues with no fixed release;
  Stackr uses the dependency as a client and does not expose either plugin path.
- README and CI metadata were replaced with Stackr-specific content; the dead
  deployment workflow was deleted. The project is Apache-2.0 licensed.
- Local history was replaced by one root commit before the first public push.

## Phase 3: the official images

- CI creates semantic releases after successful pushes to `master`, starting
  at `v0.1.0`.
- CI builds amd64 and arm64 panel and relay images from source:
  `ghcr.io/fyrmforge/stackr:<version>` and
  `ghcr.io/fyrmforge/stackr-proxyrelay:<version>`.
- Release images also receive minor, major, `latest`, and commit tags. Running
  installations are never updated automatically.
- The main image carries both `stackrd` and the `stackr` CLI. It is also the
  source image for node agents and volume tools; there is no separate agent
  image.

Closes the TODO plan 38 leaves behind: `restore.sh` ships in phase 1
without version pinning, because pinning needs a published tagged image.
Add `docker service update --image ghcr.io/fyrmforge/stackr:<VERSION>`
before the scale-up here.

## Phase 4: install and upgrade, done 2026-09-16

Plan and decisions: `docs/plans/43-panel-upgrade.md`.

- **The panel upgrades itself** from Admin, Update. Releases come from
  GitHub; the rail shows an arrow when one is newer.
- **Panel backup before the image swap**, written locally to
  `backups/pre-upgrade-<version>.tar.gz`. Reuses phase 1's archive.
- **Schema** migrations run on boot, forward only. Swarm rolls back an
  image that does not stay up; data goes back only with `restore.sh`.
- **Rollout order** is manager first, then workers: the new panel's boot
  rolls the agents to its own build.
- `install.sh --version X` installs once and refuses a live host.
  `restore.sh` moves the service to the archive's release when it is newer.
- **Supported upgrade for beta is N to N+1 only.** Skipping versions is
  not tested.

---

## Not gating, known

- Headless node join, storage attachments that do not pin consumers, 63-char
  tile names, API storage on literal `local`, and per-logical-db backups. All
  remain in `docs/notes.md`.
- Panel has no self-monitoring; a stuck work queue is silent.
- No config export to YAML on any surface, which is also what makes a
  hand-built org impossible to capture as config.

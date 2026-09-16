# Plan: upgrade from the panel

Status: done 2026-09-16. Verified on the rig: install.sh v0.1.1, panel
upgrades v0.1.1 to v0.1.2 and v0.1.3 to v0.1.4 with two orgs of running
tiles across two nodes (no tile task restarted, data intact, agents rolled),
and a boot-crashing update rolled back by swarm. Closes phase 4 of
`docs/beta-release.md`.

Working rules: discuss first, one point at a time, no code without a go, no
git writes, no edits to `*_templ.go` or `output.css`, terse UI copy.

## Where this came from

Re-reading the install path after v0.1.0 shipped. Three claims in the docs
did not hold against the code:

1. `docs/beta-release.md` says the upgrade path already exists in
   `install.sh`. It does not. `scripts/install.sh:312` removes the service and
   creates it again from re-typed answers. There is no update branch.
2. `docs/release.md` says upgrades start from the panel. Nothing in the panel
   does that.
3. `docs/notes.md` says the image does not ship the CLI. It does
   (`cmd/stackrd/Dockerfile:49`). What is still true is that the CLI has no
   localhost admin auth, so the `stackr` wrapper still needs a login.

## Decisions (2026-09-16)

- **The panel upgrades itself.** One button under `/admin`. The operator on
  the host is the fallback, not the primary path.
- **Backup is hot, from the panel, before the swap.** `writePanelArchive`
  (`infra/backup/backup.go:462`) already produces the restorable archive with
  `VACUUM INTO`, key and VERSION. It gains a local-file sink,
  `$DATA_DIR/backups/pre-upgrade-<oldversion>.tar.gz`. A failed write aborts
  the upgrade. `restore.sh` takes that file unchanged.
- **New versions come from GitHub releases.**
  `api.github.com/repos/FyrmForge/stackr/releases/latest`, one field
  (`tag_name`), no token. Checked daily and on opening the update page. Cached
  in the `settings` table.
- **Rail widget above Account, admin only.** Same shape as the notification
  bell in `components/shell.templ:61`: an `hx-get` fragment. Empty when
  current, an arrow with a dot when newer exists. Click goes to
  `/admin/update`.
- **Failure to boot rolls back the image, not the data.** The service update
  sets `UpdateConfig{FailureAction: rollback, Monitor: 60s}`, so a crash-looping
  new task brings the old image back on its own. Migrations are forward-only;
  the rolled-back panel opens a newer schema. Checked: hamr's `Migrate` treats
  an unknown current version as `ErrNoChange`, so the old binary boots.
  Acceptable in beta. The full way
  back is `restore.sh` with the pre-upgrade archive, and the update page prints
  that command and path after every run.
- **No rollback button.** The panel cannot restore the database it is running
  on.
- **`install.sh` refuses to rebuild a live install.** If the `stackr` service
  exists it points at `/admin/update` and prints the manual
  `docker service update` line for a dead panel.

## What the button does, in order

1. Pull the panel image and the relay image on the manager. Fails here, before
   anything is touched, when the tag is missing or ghcr is down.
2. Retag the relay to `stkr-proxyrelay:local`
   (`infra/runtime/runtime.go:79` hardcodes that name).
3. Write the pre-upgrade archive. Abort on failure.
4. Update the `stackr` service: new image, `STACKR_IMAGE` env set to the new
   tag, rollback policy. Swarm stops this task and starts the new one.
5. The new panel boots, runs migrations, and `agent.Ensure`
   (`cmd/stackrd/main.go:458`) rolls the node agents to the same build by
   digest. Nothing extra to do for workers.

Step 4 is where the browser loses the panel for about twenty seconds. The
old task keeps answering for a few seconds after the update call, so the page
cannot reload on the first healthy reply. `/api/health`
(`handlers/api/server.go:66`) gains a `version` field and the page polls until
it sees the target version.

## Landmines this has to respect

- `STACKR_IMAGE` is read first by `infra/agent/service.go:245` to pick the
  agent image. `--image` alone leaves it on the old tag: new panel, old
  agents, forever. The env has to change in the same update.
- Agent version mismatch is a hard refuse (`infra/agent/client.go:90`). The
  boot-time `Ensure` is what makes that safe; do not gate it on anything new.
- The relay image is looked up by the local tag, so an upgrade that skips
  step 2 leaves port-forward on the old relay with no error anywhere.

## Implementation

### A. Admin service

`internal/stackrd/service/admin.go`, `AdminService`, next to `AuthService`
in the existing `service` package. Upgrade is its first job; Maintenance and
the manual panel backup move in from the settings handler later.

- `CheckUpgrade(ctx)`: fetch latest release, store `upgrade_latest` and
  `upgrade_checked_at` in settings. Daily ticker started from `main.go`, plus
  on demand from the page. Skipped when the running version does not start
  with `v` (dev builds): badge stays empty, page says "dev build, no
  upgrades".
- `Upgrade(ctx, version)`: steps 1 to 4 above. Records `upgrade_previous_image`
  and the archive path in settings for the page. Holds a mutex; a second
  call while one runs fails with "upgrade already running".
- `handlers/api/handler/health/handler.go`: add `version` to the JSON.
- `infra/backup/backup.go`: export `WritePanelArchiveTo(path string)` over
  `writePanelArchive`.
- `infra/runtime/service.go`: export a thin `UpdateServiceImage(name, image,
  env, updateCfg)` over the private `updateService`.

### B. Admin page and rail widget

- `handlers/web/handler/settings/handler.go`: `Update` (GET), `RunUpdate`
  (POST), `UpdateBadge` (GET fragment). New `upgrade.templ` in the same
  package. Tab added to the admin nav at `settings.templ:27`.
- `handlers/web/server.go`: three routes under the existing `adminOnly`
  group.
- `components/shell.templ`: badge div above the Account icon, admin only.
- `cmd/stackrd/main.go`: construct `AdminService`, start the check ticker,
  hand it to web deps.

### C. Scripts

- `scripts/install.sh`: `--version X` flag, default `latest`, sets both image
  tags. Drop the "not published yet" header and the "re-run to upgrade" text.
  Existing-service branch as decided above.
- `scripts/restore.sh`: when the archive's version differs from the running
  image, pin the service to the archive's version before scaling up, in
  either direction. A restore always brings back the build that wrote the
  data, so the pre-upgrade archive alone undoes a bad upgrade.

### D. Docs

- `docs/beta-release.md`: phase 4 status.
- `docs/host-setup.md`: short upgrade section.
- `docs/notes.md`: fix the stale CLI line.

## Verification

On the rig: install `v0.1.0` with `install.sh --version 0.1.0`, push a
`fix:` to master to get `v0.1.1`, see the rail dot appear, press Upgrade,
confirm the archive exists, the panel comes back on `v0.1.1`, and on a
two-node rig the worker's agent reports the same version. Then run
`restore.sh` with the pre-upgrade archive and confirm the service is back
on `v0.1.0`.

## Not doing

- Upgrade from the CLI. Same API could serve it later; not in this pass.
- A `curl | sh` bootstrap. Still undecided (plan 16).
- Localhost admin auth for the in-container CLI. Own plan, see notes.
- Skipping versions. N to N+1 is what gets tested.

# Upgrade

One button upgrades the panel, then every worker, one at a time.

1. Log in as a server admin. Open Admin, Update.
2. The page shows the running version and whether a newer release exists. Press **Check for update** if it does not say.
3. Press **Upgrade to vX.Y.Z**.
   - The panel archives its database, then restarts on the new image.
   - Running apps keep running.
   - A box without Docker's log cap gets it now, and Docker restarts once before the workers roll. Apps come back on their own.
4. When the panel is back, it rolls the workers onto the new build. The Workers list on the same page shows each one as waiting, upgrading, done, failed or skipped.
   - One worker at a time. Nodes that other nodes route through go last.
   - A worker that does not come back on the new build rolls itself back, and the rollout stops.

## When a worker fails

The Workers list shows `<node> failed to upgrade, rolled back` and the error. The workers after it are not touched.

1. Fix the cause. The node's row has the error, and `stackr node show <name>` has its state.
2. Press **Retry** on the Update page. It starts the rollout again.

A worker that is offline is upgraded when it reconnects.

If workers are behind and no rollout is running, the button reads **Update workers**.

## From the CLI

```bash
stackr admin upgrade-check
stackr admin upgrade --tag v0.7.0
stackr admin upgrade-workers
```

`upgrade-workers` does the same as Update workers and Retry.

## From the installer

Run the `stackr-install` of the release you want on the panel's box, as root. No flags are needed, it reuses the saved answers.

```bash
./stackr-install
```

- Same version as the panel: nothing changes.
- Newer version: it does what Update does. The panel archives its database, the panel restarts on the new image, and the workers roll onto it. A panel that does not come up healthy is swapped back to the old one.
- Older version: refused, since migrations may not reverse. Restore the pre-upgrade archive instead, or pass `--allow-downgrade`.
- A dev build counts by its version: 0.8.2-dev.1 is after 0.8.1 and before 0.8.2.
- A box without Docker's log cap gets it first (see [Install](install.md)), and Docker restarts once. Apps come back on their own. Workers get it after their upgrade.

The archive is taken before anything changes. It is listed in Backups as a manual backup, under `/var/lib/stackr/backups/stackr/_panel/<install>/manual/`. The way back is `stackr-install restore <that file>`.

This is the way off a dev build, which the Update button cannot move.

## Notes

- A dev build cannot upgrade from the panel. Use the installer.
- The pre-upgrade archive is kept. See [Backups](backups.md).

# Upgrade

One button upgrades the panel, then every worker, one at a time.

1. Log in as a server admin. Open Admin, Update.
2. The page shows the running version and whether a newer release exists. Press **Check for update** if it does not say.
3. Press **Upgrade to vX.Y.Z**.
   - The panel archives its database, then restarts on the new image.
   - Running apps keep running.
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

## Notes

- A dev build cannot upgrade.
- The pre-upgrade archive is kept. See [Backups](backups.md).

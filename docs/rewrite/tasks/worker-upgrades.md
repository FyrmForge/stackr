# Worker upgrades

Status: agreed with darthvader 2026-10-14, not planned in lanes yet.

## Today

- A panel upgrade (`internal/service/internal/flow/upgrade`) only touches the controller's Docker.
- Workers keep their `stackrd` until someone reruns `stackr-install join`.
- `stackr-install join` installs the installer's own version from ghcr (`cmd/stackr-install/join.go` `fetchWorker`).
- Nothing compares controller and worker versions. An old worker answers unknown calls with Docker's 404, and a pre-TrustRoots worker stays offline after a root rollover.

## Agreed

1. **Source.** On upgrade, workers pull the panel image `stackr.system/stackr:<version>` from the cluster registry over WireGuard, take `stackrd` out of it, and swap their binary. No GitHub and no internet. Refuse when the worker's arch does not match the image (amd64 only today).
2. **Trigger and order.**
   - Upgrades happen only when the admin clicks update in the panel. There are no automatic upgrades.
   - The panel backs up, upgrades itself, publishes its image, then rolls the workers one at a time.
   - It waits for each worker to report the new version with its tunnel up.
   - A worker that is not back within about 2 minutes restores its old binary and restarts. The rollout stops and the panel shows "<node> failed to upgrade, rolled back"; retry is a button.
   - An offline worker is upgraded when it reconnects (its reported version is behind).
   - The map card and node drawer show "behind".
   - A worker more than one version behind is refused for new calls with "worker too old, upgrade it", not a silent Docker 404.
3. **Join version and installer.**
   - The join and rejoin commands the panel prints carry `--version <panel version>`, so a stale installer still installs the panel's version.
   - `stackr-install` goes into the panel image next to `stackrd`, and the worker upgrade swaps both.
   - The first join still pulls from ghcr (the node is not on the tunnel yet).

decision taken (Claude): the Update tab shows per-node rollout progress (version, state, error).

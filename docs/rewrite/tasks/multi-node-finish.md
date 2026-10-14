# Multi-node: finishing work

Status: every item agreed with darthvader 2026-10-14. Not planned in lanes yet.
Worker upgrades have their own file: `worker-upgrades.md`.

## On the table

1. UI round (`ui-round.md`): built on branch `ui-round-integ`, tests green. Merge into multi-node, release, rig proof, darthvader's click checks.
2. Tar backup fix (ce6be40): ships with 1.
3. Worker upgrades and version checks (`worker-upgrades.md`).
4. Config files (`files:`) go in read-only Docker volumes on the app's node, not a folder on the controller (`flow/deploy/mount_files.go` `fileBinds`). Docker 26+ required (volume subpath).
5. Env delete marks its volumes orphaned rather than deleting the rows, so the sweep keeps them for the retention, takes a last backup and deletes. The same for org and stack delete. The confirm says so.
6. Every container stackr starts: json-file logs capped at 3 x 10 MB. Before deploy: under 10% free disk warns, under 1 GB refuses with the node name. No free-memory check.
7. Removing a node cleans the box:
   - If it is online, the worker stops and disables its service, removes the stackr firewall rules, the tunnel, and the stackr containers and networks (volumes and images stay).
   - `stackr-install uninstall` does the same; `--purge` also deletes volumes and images, after the node name is typed.
   - A refused worker stops asking after a day and logs "removed from cluster, run stackr-install uninstall".
   - Remove still needs every env moved off first.
   - Remove first stops everything on the node, backs up what is left, checks every backup succeeded, and only then removes. A failed backup stops the removal and starts the apps again.
7b. "Back up node":
   - A node drawer button that backs up every volume on the node, consistently: one env at a time, databases by dump, then stop the env, archive its plain volumes, start it.
   - No live mode.
   - Shown as one group ("node h3, 14 Oct") with per-volume results.
   - With no destination it falls back to the controller's local folder, with a warning.
   - Offered pre-ticked before break glass.
8. Default ingress switch:
   - Look up the panel domain and the affected routes' domains, list those that don't point at the new node, and confirm by typing the node name (the panel domain called out).
   - Refuse an offline node, or one with no public IP and no port forward.
   - Remove node is refused while the node is the default ingress.
9. Expired node cert stays a manual rejoin. The card and drawer say "Cert expired, rejoin needed" with the rejoin button. Under 14 days left, they say "Cert expires in N days".
10. Offline nodes:
    - Errors name the node and say when it was last seen.
    - A deploy to an offline node is refused up front.
    - Env and tile pages show a banner.
    - Scheduled jobs there are recorded as "skipped: <node> offline".
11. Rig proofs:
    - Controller restore with all 4 workers attached: back up, wipe, restore, and the workers reconnect with the same names, envs and tunnels, apps answering.
    - Exit links through h4: traffic leaves through h4's IP, and h4 down means no internet.

## Backlog

- Alerting (node offline, cert expiry, disk full, failed backups), with the notifier connectors.
- Build source in Docker volumes on the builder, cleaned with the image; multi-builder support.
- Multi-arch images (ARM workers).
- Clock drift check.
- Registry: retry a push that spans a cleanup.
- Map polish: opening zoom, crowded labels, line bundling at scale.
- Observability (`observability.md`), connectors, personal devices on the mesh.

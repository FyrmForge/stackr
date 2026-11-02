# Remove a node

## Remove a healthy worker

1. Check what the node still carries. Remove refuses while any of these remain.
   - A placement on it, with its envs: repoint the placement with `stackr admin placement move <placement> <other-node>`.
   - The default ingress: pick another with `stackr ingress set <other> --default`.
   - Being another node's default exit: point that node elsewhere with `stackr node exit <node> <other>`.

2. Remove it. In the panel: Admin, Nodes, the node, Remove this node.

   ```bash
   stackr node remove vps-fsn1
   ```

What remove does, in order:

1. Stops everything on the node.
2. Backs up every volume on it and checks the backups.
3. Cleans the box: the service, containers, networks, firewall rules and the mesh.
4. Removes the node from the cluster.

If a backup fails, it stops there, starts the apps again and leaves the node in place.

The node must be online. For a dead node use `stackr node forget <name>`. It revokes the node for good and marks its envs lost. `stackr env lost` lists them. `stackr env restore --placement <placement> --to <node>` brings a placement's envs back from their backups.

## Uninstall on the box

Remove already cleans the box. Use `stackr-install uninstall` when the panel can no longer reach the machine. Run it on the worker, as root.

```bash
sudo stackr-install uninstall
```

It stops the worker service and removes stackr's containers, networks, firewall rules and mesh. Volumes, images, the binaries in `/usr/local/bin` and `/var/lib/stackr-worker` stay, so a rejoin finds its data.

```bash
sudo stackr-install uninstall --purge
```

`--purge` also deletes the volumes, the images stackr built or pulled, the binaries and the state. Other images stay. `/etc/docker/daemon.json` stays, with the log cap the installer set. It asks you to type the node's name. `--yes` skips the first question but not that one. `--dry-run` shows what would change.

## Remove the panel

Remove the workers first, or they keep calling back. If you may want this install again, keep a panel backup and its recovery key off the box.

On the controller, as root:

```bash
sudo stackr-install uninstall --purge
```

It asks you to type the panel's host name. Then it removes the panel and every app on the box with their volumes, the images stackr built or pulled, the docker login for the panel's registry, the firewall rules, `stackr0` and the mesh, `/var/lib/stackr`, and `stackr` and `stackr-install` in `/usr/local/bin`. `/etc/docker/daemon.json` stays. Without `--purge` it refuses on the controller.

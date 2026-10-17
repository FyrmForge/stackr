# Remove a node

## Remove a healthy worker

1. Check what the node still carries. Remove refuses while any of these remain.
   - Envs: move them with `stackr env move --stack shop --env prod --to <node>`.
   - The default ingress: pick another with `stackr ingress set <other> --default`.
   - Being another node's default exit: point that node elsewhere with `stackr node exit <node> <other>`.

2. Remove it. `--move-to` moves its envs to that node first, so you can skip the moves above.

   ```bash
   stackr node remove vps-fsn1 --move-to home
   ```

   With no envs on it, leave out `--move-to`. In the panel: Admin, Nodes, the node, Remove this node.

What remove does, in order:

1. Stops everything on the node.
2. Backs up every volume on it and checks the backups.
3. Cleans the box: the service, containers, networks, firewall rules and the mesh.
4. Removes the node from the cluster.

If a backup fails, it stops there, starts the apps again and leaves the node in place.

The node must be online. For a dead node use `stackr node forget <name>`. It revokes the node for good and marks its envs lost. `stackr env lost` lists them. `stackr env restore <env> --stack <stack> --to <node>` brings one back from its backups.

## Uninstall on the box

Remove already cleans the box. Use `stackr-install uninstall` when the panel can no longer reach the machine. Run it on the worker, as root.

```bash
sudo stackr-install uninstall
```

It stops the worker service and removes stackr's containers, networks, firewall rules and mesh. Volumes, images, the binaries in `/usr/local/bin` and `/var/lib/stackr-worker` stay, so a rejoin finds its data.

```bash
sudo stackr-install uninstall --purge
```

`--purge` also deletes the volumes, images, binaries and state. It asks you to type the node's name. `--yes` skips the first question but not that one. `--dry-run` shows what would change.

It refuses to run on the controller box.

## Remove the panel

`stackr-install uninstall` refuses on the controller, so a panel comes off by hand. Remove the workers first, or they keep calling back. If you may want this install again, keep a panel backup and its recovery passphrase off the box.

On the controller, as root:

```bash
docker ps -aq --filter label=stackr.managed=true | xargs -r docker rm -f
docker ps -aq --filter label=stackr.role=panel | xargs -r docker rm -f
docker rm -f stackr-proxy stackr-registry
docker ps -aq --filter name=^buildx_buildkit_stackr-builder | xargs -r docker rm -f
docker volume ls -q --filter label=stackr.managed=true | xargs -r docker volume rm
docker volume ls -q --filter name=^buildx_buildkit_stackr-builder | xargs -r docker volume rm
docker volume rm stackr-caddy stackr-registry-data
docker network ls -q --filter label=stackr.managed=true | xargs -r docker network rm
rm -rf /var/lib/stackr
rm -f /usr/local/bin/stackr /usr/local/bin/stackr-install
```

Images stay; `docker image ls` shows them. stackr's firewall rules and the `stackr0` interface go at the next reboot.

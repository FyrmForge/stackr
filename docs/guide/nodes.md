# Nodes

A node is a machine in the cluster. The controller is the first. Workers run envs and can carry public traffic.

The `stkr` commands below run on the controller and need a login first. See [Log the CLI in](#log-the-cli-in).

## Add a worker node

Needs a Linux box, root, curl, Docker 26.0 or newer (the join offers to install it), systemd, WireGuard in the kernel, UDP 51871 and TCP 7443 and 7444. The [README](../../README.md#system-requirements) has the rest.

The box needs its own way out to the internet while it joins: the join line fetches from the panel's URL and pulls the stackr image from `ghcr.io`. A box with only a private address needs NAT from its network first.

1. In the panel, open Admin, Nodes, Add node. Give it a name: lowercase letters, digits and dashes.
   - **Tags:** every new node has `egress`. `storage` is ticked by default: envs with volumes or operated databases only run on `storage` nodes. Untick it for an edge box, and tick `ingress` or `builder` for more roles.

2. Press Get join command. The panel shows one line. It works once and expires after 60 minutes.

   ```bash
   curl -fsSL https://stkr.apps.example.com/join/<token> | sh
   ```

3. Run it on the new machine as root. It checks the box (Linux, amd64, curl), downloads `stackr-install` from the panel into a temporary folder, and joins with the same token. If Docker is missing it asks to install Docker with Docker's install script. An installed Docker is never upgraded. With no terminal it stops and says what to do. The folder is removed when it ends. `/usr/local/bin/stackr-install` stays for upgrades and removal.

   A dev build of the panel has an image that is not on `ghcr.io`, so the join line cannot pull it. Load it on the new machine first, by its tag, and the line uses it: `docker save ghcr.io/fyrmforge/stackr:<version> | ssh root@new-machine docker load`. Saving by digest does not work: the saved image has no tag, and the line looks the image up by `ghcr.io/fyrmforge/stackr:<version>`. `docker image inspect ghcr.io/fyrmforge/stackr:<version>` on the new machine must succeed.

   A panel without a trusted certificate (`--https=false`) shows a two-step version instead: download the installer from GitHub, then run its `join` line. The one line is only served over https.

4. The new machine prints the panel's Nodes link and waits. Go back to the panel: the Add node dialog shows the machine that called in. Check it and press Add.

5. The new machine prints a link to the node in the panel. Open it to set tags and exits. Orgs reach a node through a [placement](placements.md).

From the CLI on the controller:

```bash
stkr node add vps-fsn1
stkr node add vps-edge --no-storage --tag ingress
stkr node ls --pending
stkr node confirm vps-fsn1
stkr node ls
```

A node made from the CLI gets `egress` and `storage`. `--tag ingress` or `--tag builder` adds roles, and `--no-storage` leaves storage off. The state is `online` when the node is connected. `stkr node show vps-fsn1` has the detail. To turn a request away: `stkr node reject vps-fsn1`.

## Proxmox container (LXC)

A worker can run in a Proxmox container. Docker, WireGuard and the mesh need more than the defaults give.

1. Create the container as privileged: untick "Unprivileged container". It cannot be changed later. Unprivileged containers are not supported for now.
2. Under Options, Features, tick Nesting. Tick NFS and SMB/CIFS as well if apps will mount network shares.
3. On the Proxmox host, load the WireGuard module and keep it across reboots:

   ```bash
   modprobe wireguard
   echo wireguard >> /etc/modules-load.d/stackr.conf
   ```

   Without the second line a host reboot breaks the join with "wireguard: the kernel has no module".
4. Join the container as in [Add a worker node](#add-a-worker-node).
5. To give the node its own disk, bind a folder from the host into the container as a mount point. On the Proxmox host, make a dataset or a folder for it. Then open the container, Resources, Add, Mount Point, and pick that storage or set the host path, with a path inside the container such as `/mnt/stackr`. Start the container and add `/mnt/stackr` as the node's disk, as in [Node disk](#node-disk). Grow the mount point in Proxmox later and the panel shows the new size without a restart. To move the disk to a new container, forget the old node, move the mount point, join the new container and adopt the disk.
6. For a GPU, pass the render device into the container: Resources, Add, Device Passthrough, device `/dev/dri/renderD128`, access mode `0666`. Then grant the device to the tile in the panel.

## Nodes and placements

Nodes are hardware, and only server admins see them: `stkr node`, `stkr cluster` and Server settings > Nodes are admin-only. Orgs, stacks and envs name a placement, never a node. A placement stands on one node and an admin repoints it. See [Placements](placements.md) for pins, access and moves.

To take a worker out, repoint its placements first (`stkr admin placement move`); `stkr node rm` refuses while a placement or an env is on the node.

## Node disk

By default a node's volumes live in Docker's storage, on the system disk. A node disk keeps them on a disk of their own. A node has at most one. Volumes made after it is added go there. Volumes that exist already stay where they are until you move them.

In the panel: Admin, Nodes, the node, Node disk. It shows the device, the folder, the size and what is used, and lists the volumes on it.

### Add a folder disk

Use a folder that is already mounted on the node: a disk you mounted yourself, an NFS export, or a Proxmox mount point. Nothing is erased.

1. Press Add disk and choose "A folder that is already mounted".
2. Type the folder and press Check. It must be a mount point, not part of the system disk, and empty apart from `lost+found`.
3. Press Use folder.

```bash
stkr node disk add vps-fsn1 /mnt/data
stkr node disk ls vps-fsn1
```

A folder on a network disk (NFS) adds with a warning: databases on it risk corruption and slow writes.

### Add a whole disk

stackr formats an empty block device xfs and mounts it. Everything on the device is erased. This is the kind that holds [size caps](#size-caps).

The controller can hold a whole disk only when the host's `/`, or the mount holding `/var/lib/stackr-disks`, is shared, which it is on systemd hosts. If that is later made private, the panel will not start after a reboot until the installer is run again.

1. Press Add disk and choose "A whole disk". The list shows each device and whether it can be used.
2. Pick one and press Continue. Type the disk's name (like `sdb`) to confirm, then press Format disk.

```bash
stkr node disk devices vps-fsn1
stkr node disk add vps-fsn1 --device sdb --confirm sdb
stkr node disk add vps-fsn1 --device ata-WDC_WD20EZ_WD-1234 --format-anyway --confirm ata-WDC_WD20EZ_WD-1234
```

A device that holds a filesystem, a partition table or an earlier stackr disk shows what is on it and is not offered. If it is the one you mean, and you have checked that nothing on it is needed, the button reads Format anyway (`--format-anyway`). It still asks for the name; on the command line it asks for the disk's serial or by-id name instead, as above. A bare kernel name (`sdb`) is refused for a disk that has neither. To keep the data on it, adopt the disk instead (see [Adopt a disk](#adopt-a-disk)).

Some devices are never offered, with no way around it: a mounted device, one with partitions, the system or Docker disk, and a member of LVM, RAID, LUKS or ZFS, in use as swap or held by another device. Fix that on the box first.

If adding stops half way, the strip at the top of the node says so. Try again picks up where it stopped.

### Grow

When the device behind a whole disk gets bigger (a resized virtual disk, a grown volume), the Node disk card says "New space on the device". Press Use it. The disk stays in use and the volumes keep running.

```bash
stkr node disk grow vps-fsn1
```

Never shrink the device under a disk. xfs cannot shrink, so a smaller device cuts off data the filesystem still holds. To get a smaller disk, add a new one, move the volumes and forget the old one.

A folder disk has no Use it button. Its size is whatever the mount gives, and the panel reads it again after you grow the mount.

### Size caps

A volume can have a size cap. On a volume's Settings, Size cap takes a number and a unit, and empty means no cap.

```bash
stkr volume cap pgdata 20GB
stkr volume cap pgdata none
```

The same cap can be set in the stack file, per volume. Absent keeps the cap as it is, a number sets it and `0` removes it:

```yaml
volumes:
  data:
    max_size_mb: 20480
```

A cap is a limit only on a volume that lives on a whole disk. Elsewhere it is saved and not enforced, and the panel says so. A cap under what the volume already holds is refused.

### Move volumes onto the disk

A new disk starts empty. Existing volumes copy over: the copy is checked, the volume switches to the disk, then the old copy is removed.

- One volume: its drawer, Stored on, Move to disk. Only the tiles that mount it stop while it copies.
- Every volume of the node: Node disk, Move all to disk. It shows the groups and what each restarts, then copies one env at a time, smallest first. It refuses when the disk lacks room.

```bash
stkr volume move-to-disk pgdata
stkr node disk move-all vps-fsn1
```

If a move stops half way, the volume's Stored on card says so and has Finish move.

### Move the panel database onto the disk

The controller's node disk can also hold the panel's own database. On the controller's Node disk card, Move the panel database onto this disk shows the plan first: the size, the free space on the disk and on the system disk, what restarts (the panel, for about a minute), and this warning:

> After this, the whole control plane depends on this one disk. If the disk is missing the panel does not start, and nothing alerts you while it is down. To bring the disk back without a reboot, run `stackr-install disk-check` on the node.

Only a whole disk can hold it, only on the controller, and only while no update or volume move runs. The plan refuses when the data folder path goes through a symbolic link or its filesystem cannot set `chattr +i`.

The helper stops the panel, copies the database (checked by hash and an integrity check), mounts the disk folder on `/var/lib/stackr/db` with a systemd unit, starts the panel and waits for it to be healthy. If anything fails the panel starts again on the old database. The keys, local backups and `install.json` stay on the system disk, so a stolen disk holds the database but not the master key.

When it is done the row says "On this disk", with Move back to the system disk. The old copy on the system disk shows as "Old copy, taken DD-MM". Remove old copy is the only thing that deletes it.

```bash
stkr node disk move-panel vps-fsn1
stkr node disk move-panel vps-fsn1 --back
stkr node disk remove-panel-copy vps-fsn1
```

While the database is on the disk, Forget and Adopt refuse: move it back first. An Update works as usual; it copies the database through a helper that sees the real mount.

If the disk is missing when the box boots, the panel exits with a clear line and Docker keeps restarting it. It never makes an empty database. Attach the disk, then on the node:

```bash
stackr-install disk-check
```

It starts the disk's mount units, and the restart loop brings the panel up without a reboot. If a move stops half way, `stackr-install` reads `/var/lib/stackr/move.journal` and finishes or rolls it back.

### Known limitations

- `stkr node disk move-panel` returns once the move starts. The node disk card shows the outcome.
- A failing quarantine job retries every 5 minutes while the disk stays down.
- Scheduled runs on a quarantined node show one failed job per tick until Check disk.
- A standalone database primary that was relocated onto the failed disk is not stopped. Nothing new starts on that disk.
- An app that had already exited when the disk failed is not redeployed by Check disk. Redeploy it by hand.
- The logs and metrics size cap is set when they move. To change it, move them back and onto the disk again.
- A disk that hangs instead of returning errors hangs the panel rather than stopping it.
- On a worker, after re-attaching a disk run `stackr-install disk-check` on that node before Check disk.
- Forgetting a missing controller disk that holds logs and metrics drops them. They start fresh on the system disk.

### A disk that is missing

If the disk is unmounted or the device is gone, the node shows "The disk is missing", and tiles that use it do not start. Reattach or mount it, then press Check disk.

```bash
stkr node disk check vps-fsn1
```

If it will not come back, press Forget disk and type the node's name. Its volumes are restored from their newest backup, and volumes without a backup start empty. The folder itself is not touched.

```bash
stkr node disk forget 3f2a9c1e
```

### Adopt a disk

A disk outlives its node. After [forgetting](remove.md) a dead node, put its disk on another node with its envs and volumes. The new node must have no disk of its own.

1. Move the disk to the new machine (or, for a Proxmox mount point, to the new container), and mount it.
2. In the node's Node disk card, press Adopt a disk. It lists the disks of forgotten nodes, or takes a folder.
3. The plan lists the envs, the volumes on the disk, the volumes restored from backup and the folders nobody claims, which are kept. Press Adopt disk.

```bash
stkr node disk adopt vps-new
stkr node disk adopt vps-new /mnt/data
```

Adopt is refused while the old node still exists, even offline, for a folder and for a whole disk alike, because two nodes writing one disk corrupt it and nothing stops an offline node. A plain Docker copy of the same volume on the new node blocks it: the plan names it, and `--discard <slug>` removes that copy first.

### Two copies of a volume, or a link to the wrong folder

A volume that has a folder on the disk and also a plain Docker copy shows "Two copies of this volume", and deploys, backups and delete are refused. Press Discard Docker storage copy. It removes only the plain copy, and the folder stays.

A volume whose Docker object points at some other folder shows "This volume points at the wrong folder". Press Remove link. Both folders keep their data.

```bash
stkr volume discard pgdata
stkr volume discard pgdata --bound
```

To do it by hand: stop the tiles that mount the volume, remove their containers, then run `docker volume rm <name>` on that node. The name is on the volume's drawer. The folders stay.

### Remove

`stackr-install uninstall --purge` removes Docker volume objects only. The folders of a node disk, and the data in them, stay on the device. Delete them yourself if you want them gone. See [Remove a node](remove.md).

## Ingress and egress

Ingress is the node public traffic enters through. Egress is the node an env's outgoing traffic leaves through.

**Ingress.** The node needs the `ingress` tag and TCP 80 and 443 free. With no pick, an env's routes enter on its own node when that node has the tag, else on the default ingress. A pick names a placement.

```bash
stkr node tag vps-fsn1 ingress
stkr ingress set eu-prod --stack shop --env prod
stkr ingress get --stack shop --env prod
```

A pick covers its org, stack, env or tile and everything below it. `stkr ingress set --clear --stack shop` goes back to inheriting. Point DNS at the node a host enters through. Hosts in a domain stackr manages need nothing: their records follow the ingress. See [Domains and DNS](domains.md).

The default ingress serves the panel and every route without a pick. Moving it lists where each host points today, then asks you to type the node's name.

```bash
stkr ingress set vps-fsn1 --default
```

Point DNS at the new default ingress. Hosts in a managed domain are left out of the list: stackr repoints them.

**Egress.** A node with the `egress` tag sends traffic out its own uplink. To send a scope's traffic out another placement's node, give it an ordered exit list of placements (or `own`).

```bash
stkr exit set eu-prod eu-db --stack shop --env prod
stkr exit ls --stack shop --env prod
```

With a list, traffic moves to the next exit when one is down. With one exit and it down, there is no internet.

**Default exit.** A node without `egress` still needs a way out. Give it exits:

```bash
stkr node exit vps-home vps-fsn1 vps-hel1
```

The exits are tried in order, up to three. A node without `egress` always needs at least one; tag it `egress` to send its traffic out its own uplink instead. The cluster-wide default exit is `stkr exit set vps-fsn1 --default`; `stkr exit set --default` clears it. With none set, every node uses its own uplink. See both defaults with `stkr ingress get --default`.

Change tags with `stkr node tag` and `stkr node untag`. The default ingress and exit and `stkr node exit` name nodes; they are admin settings.

## Rejoin a broken worker

For a worker that broke, was reinstalled, or shows `certificate expired` or `not rejoined`:

1. On the controller:

   ```bash
   stkr node rejoin vps-fsn1
   ```

2. Run the command it prints on that machine, as root. The node keeps its name, range and envs.

3. Confirm it like a new node: Admin, Nodes, Waiting to join, Add, or `stkr node confirm vps-fsn1`.

In the panel, the node's Security tab has the same Rejoin button.

## Break glass

Use it when keys or the cluster root may be exposed. It replaces the root and every WireGuard key. Tunnels between nodes stop until each node rejoins. Apps keep running where they are. Deploys and traffic between nodes stop.

1. Run it. The Nodes tab has the same button.

   ```bash
   stkr cluster break-glass
   ```

2. For each node it lists, run `stkr node rejoin <name>`, then the printed command on that machine, then confirm it under Admin, Nodes, Waiting to join.

3. Check with `stkr cluster trust`.

A second break glass within 10 minutes is refused.

## Log the CLI in

On the controller, `stkr` runs inside the panel container, so the browser login cannot reach it. Use a key.

The first install logs the CLI on the controller in as the admin for you, with a key bound to no org, so it runs server admin commands too. The key is stored in `/var/lib/stackr/cli.json` and never printed. It works once the panel's DNS and certificate do.

From a laptop, `stkr login <url>` opens the browser. The key it gets acts as you in every org you are in, and runs server admin commands if you are a stackr admin. `stkr org use <slug>` switches org.

For an admin key with no browser, mint one with `stkr admin key <name>` on the controller and log in with it:

```bash
stkr login https://stkr.apps.example.com --with-key "$KEY"
```

For one org's work, mint a key in the panel instead: open the org, Keys, Mint key. Then add `--org acme` to the login. A key bound to an org never runs server admin commands.

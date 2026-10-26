# Nodes

A node is a machine in the cluster. The controller is the first. Workers run envs and can carry public traffic.

The `stackr` commands below run on the controller and need a login first. See [Log the CLI in](#log-the-cli-in).

## Add a worker node

Needs a Linux box, root, Docker 26.0 or newer, systemd, WireGuard in the kernel, UDP 51871 and TCP 7443 and 7444. The [README](../../README.md#system-requirements) has the rest.

The box needs its own way out to the internet while it joins: the join line fetches from the panel's URL and pulls the stackr image from `ghcr.io`. A box with only a private address needs NAT from its network first.

1. In the panel, open Admin, Nodes, Add node. Give it a name: lowercase letters, digits and dashes.
   - **Tags:** every new node has `egress`. Tick `ingress`, `builder` or `storage` for more roles. Envs with volumes or managed databases only run on `storage` nodes.

2. Press Get join command. The panel shows one line. It works once and expires after 60 minutes.

   ```bash
   curl -fsSL https://stkr.apps.example.com/join/<token> | sh
   ```

3. Run it on the new machine as root. It checks the box (Linux, amd64, Docker), downloads `stackr-install` from the panel into a temporary folder, and joins with the same token. The folder is removed when it ends. `/usr/local/bin/stackr-install` stays for upgrades and removal.

   A panel without a trusted certificate (`--https=false`) shows a two-step version instead: download the installer from GitHub, then run its `join` line. The one line is only served over https.

4. Back in the panel, the Add node dialog shows the machine that called in. Check it and press Add.

5. The new machine prints a link to the node in the panel. Open it to set tags and exits. Orgs reach a node through a [placement](placements.md).

From the CLI on the controller:

```bash
stackr node add vps-fsn1
stackr node add vps-db --tag storage
stackr node ls --pending
stackr node confirm vps-fsn1
stackr node ls
```

`--tag ingress`, `--tag builder` or `--tag storage` adds roles; `egress` is always on. The node's address defaults to `<name>.<panel address>`; `--address` sets another, `--address ""` none. The state is `online` when the node is connected. `stackr node show vps-fsn1` has the detail. To turn a request away: `stackr node reject vps-fsn1`.

## Nodes and placements

Nodes are hardware, and only server admins see them: `stackr node`, `stackr cluster` and Server settings > Nodes are admin-only. Orgs, stacks and envs name a placement, never a node. A placement stands on one node and an admin repoints it. See [Placements](placements.md) for pins, access and moves.

To take a worker out, repoint its placements first (`stackr admin placement move`); `stackr node remove` refuses while a placement or an env is on the node.

## Ingress and egress

Ingress is the node public traffic enters through. Egress is the node an env's outgoing traffic leaves through.

**Ingress.** The node needs the `ingress` tag and TCP 80 and 443 free. With no pick, an env's routes enter on its own node when that node has the tag, else on the default ingress. A pick names a placement.

```bash
stackr node tag vps-fsn1 ingress
stackr ingress set eu-prod --stack shop --env prod
stackr ingress get --stack shop --env prod
```

A pick covers its org, stack, env or tile and everything below it. `stackr ingress set --clear --stack shop` goes back to inheriting. Point DNS at the node a host enters through. Hosts in a domain stackr manages need nothing: their records follow the ingress. See [Domains and DNS](domains.md).

The default ingress serves the panel and every route without a pick. Moving it lists where each host points today, then asks you to type the node's name.

```bash
stackr ingress set vps-fsn1 --default
```

Point DNS at the new default ingress. Hosts in a managed domain are left out of the list: stackr repoints them.

**Egress.** A node with the `egress` tag sends traffic out its own uplink. To send a scope's traffic out another placement's node, give it an ordered exit list of placements (or `own`).

```bash
stackr exit set eu-prod eu-db --stack shop --env prod
stackr exit ls --stack shop --env prod
```

With a list, traffic moves to the next exit when one is down. With one exit and it down, there is no internet.

**Default exit.** A node without `egress` still needs a way out. Give it exits:

```bash
stackr node exit vps-home vps-fsn1 vps-hel1
```

The exits are tried in order, up to three. A node without `egress` always needs at least one; tag it `egress` to send its traffic out its own uplink instead. The cluster-wide default exit is `stackr exit set vps-fsn1 --default`; `stackr exit set --default` clears it. With none set, every node uses its own uplink. See both defaults with `stackr ingress get --default`.

Change tags with `stackr node tag` and `stackr node untag`. The default ingress and exit and `stackr node exit` name nodes; they are admin settings.

## Rejoin a broken worker

For a worker that broke, was reinstalled, or shows `certificate expired` or `not rejoined`:

1. On the controller:

   ```bash
   stackr node rejoin vps-fsn1
   ```

2. Run the command it prints on that machine, as root. The node keeps its name, range and envs.

3. Confirm it like a new node: Admin, Nodes, Waiting to join, Add, or `stackr node confirm vps-fsn1`.

In the panel, the node's Security tab has the same Rejoin button.

## Break glass

Use it when keys or the cluster root may be exposed. It replaces the root and every WireGuard key. Tunnels between nodes stop until each node rejoins. Apps keep running where they are. Deploys and traffic between nodes stop.

1. Run it. The Nodes tab has the same button.

   ```bash
   stackr cluster break-glass
   ```

2. For each node it lists, run `stackr node rejoin <name>`, then the printed command on that machine, then confirm it under Admin, Nodes, Waiting to join.

3. Check with `stackr cluster trust`.

A second break glass within 10 minutes is refused.

## Log the CLI in

On the controller, `stackr` runs inside the panel container, so the browser login cannot reach it. Use a key.

The first install logs the CLI on the controller in as the admin for you, with a key bound to no org, so it runs server admin commands too. The key is stored in `/var/lib/stackr/cli.json` and never printed. It works once the panel's DNS and certificate do.

From a laptop, `stackr login <url>` opens the browser. The key it gets acts as you in every org you are in, and runs server admin commands if you are a stackr admin. `stackr org use <slug>` switches org.

For an admin key with no browser, mint one with `stackr admin key <name>` on the controller and log in with it:

```bash
stackr login https://stkr.apps.example.com --with-key "$KEY"
```

For one org's work, mint a key in the panel instead: open the org, Keys, Mint key. Then add `--org acme` to the login. A key bound to an org never runs server admin commands.

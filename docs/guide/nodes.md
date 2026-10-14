# Nodes

A node is a machine in the cluster. The controller is the first. Workers run envs and can carry public traffic.

The `stackr` commands below run on the controller and need a login first. See [Log the CLI in](#log-the-cli-in).

## Add a worker node

Needs a Linux box, root, Docker 26.0 or newer, systemd, WireGuard in the kernel, UDP 51871 and TCP 7443 and 7444. The [README](../../README.md#system-requirements) has the rest.

1. On the controller, make a join command. The name is lowercase letters, digits and dashes.

   ```bash
   stackr node add vps-fsn1
   ```

   Add `--tag ingress` or `--tag builder` for more roles. Every new node also gets `egress`. In the panel: Admin, Nodes, Add node.

2. The panel prints a command like this. It works once and expires after 60 minutes.

   ```bash
   stackr-install join --panel https://stkr.apps.example.com --token <token> --ca-sha256 <fingerprint> --version 0.7.0
   ```

   `--version` pins the worker to the panel's release. Leave it in.

3. On the new machine, download the installer as in [Install a panel](install.md), then run that command as root. It waits for you.

4. On the controller, confirm the node.

   ```bash
   stackr node ls --pending
   stackr node confirm vps-fsn1
   ```

   In the panel, the Add node dialog shows the request and an Add button.

5. Check that it joined.

   ```bash
   stackr node ls
   ```

   The state is `online` when it is connected. `stackr node show vps-fsn1` has the detail.

To turn a request away: `stackr node reject vps-fsn1`.

## Pin envs to nodes

New envs run on the controller unless you pin. A worker takes an org's envs only after you open it to that org.

1. Open the node to the org.

   ```bash
   stackr node open vps-fsn1 --org acme
   ```

2. Pin the org or a stack. New envs go there.

   ```bash
   stackr pin vps-fsn1 --org acme
   stackr pin vps-fsn1 --stack shop
   ```

   Envs that already exist stay where they are. To move one:

   ```bash
   stackr env move --stack shop --env prod --to vps-fsn1
   ```

   A move copies while the env runs, then stops it for a final copy. `--cold` stops first. `--dry-run` shows the plan.

3. Read a pin with `stackr pin --org acme`. Remove it with `stackr pin --clear --org acme`.

`stackr node close vps-fsn1 --org acme` stops new envs landing there. The envs already on it stay.

## Ingress and egress

Ingress is the node public traffic enters through. Egress is the node an env's outgoing traffic leaves through.

**Ingress.** The node needs the `ingress` tag and TCP 80 and 443 free.

```bash
stackr node tag vps-fsn1 ingress
stackr ingress set vps-fsn1 --stack shop --env prod
stackr ingress get --stack shop --env prod
```

A pick covers its org, stack, env or tile and everything below it. `stackr ingress set --clear --stack shop` goes back to inheriting.

The default ingress serves the panel and every route without a pick. Moving it lists where each host points today, then asks you to type the node's name.

```bash
stackr ingress set vps-fsn1 --default
```

Point DNS at the new default ingress.

**Egress.** A node with the `egress` tag sends traffic out its own uplink. To send a scope's traffic out another node, give it an ordered exit list.

```bash
stackr exit set vps-fsn1 vps-hel1 --stack shop --env prod
stackr exit ls --stack shop --env prod
```

With a list, traffic moves to the next exit when one is down. With one exit and it down, there is no internet.

**Default exit.** A node without `egress` still needs a way out. Give it exits:

```bash
stackr node exit vps-home vps-fsn1 vps-hel1
```

`stackr node exit vps-home` with no exits clears it. The cluster-wide default exit is `stackr exit set vps-fsn1 --default`; `stackr exit set --default` clears it. With none set, every node uses its own uplink. See both defaults with `stackr ingress get --default`.

Change tags with `stackr node tag` and `stackr node untag`.

## Rejoin a broken worker

For a worker that broke, was reinstalled, or shows `certificate expired` or `not rejoined`:

1. On the controller:

   ```bash
   stackr node rejoin vps-fsn1
   ```

2. Run the command it prints on that machine, as root. You do not confirm it again. The node keeps its name, range and envs.

## Break glass

Use it when keys or the cluster root may be exposed. It replaces the root and every WireGuard key. Tunnels between nodes stop until each node rejoins. Apps keep running where they are. Deploys and traffic between nodes stop.

1. Run it. The Nodes tab has the same button.

   ```bash
   stackr cluster break-glass
   ```

2. For each node it lists, run `stackr node rejoin <name>`, then the printed command on that machine.

3. Check with `stackr cluster trust`.

A second break glass within 10 minutes is refused.

## Log the CLI in

On the controller, `stackr` runs inside the panel container, so the browser login cannot reach it. Use a key.

1. In the panel, open your org and mint a key. Copy it. It is shown once.
2. On the controller:

   ```bash
   stackr login https://stkr.apps.example.com --with-key "$KEY" --org acme
   ```

Commands marked server admin need a key from a server admin.

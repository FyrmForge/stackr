# Placements

A placement is a name for a place envs run. It stands on one node at a time. Orgs, stacks and envs pin to a placement; only server admins see the node behind it.

## What a member does

List the placements open to your org:

```bash
stackr placements
```

Each row shows the tags, whether it has a public IP, whether it is online and whether you can use it now. A placement with no node yet, or an offline one, is not usable: new work waits.

Pin an org, a stack or an env. New envs go to the pinned placement, else to `default`:

```bash
stackr pin eu-prod --org acme
stackr pin eu-db --stack shop
stackr pin --clear --stack shop
```

A pin that puts existing envs on another node moves them, one job per env. The command prints each env's plan and needs `--yes`, which types the org, stack or env slug:

```bash
stackr pin eu-prod --stack shop --env prod
stackr pin eu-prod --stack shop --yes
```

The same pins go in `stackr-org.yml` and the stack files as `pin: eu-prod`. A pin to a placement the org cannot use, or one whose node lacks a tag the envs need, is refused, from the CLI, the panel or a file. Volumes and managed tiles a stack file declares count before their first deploy: they need a node tagged `storage`.

Pins, ingress and exits are an org owner's to set. A stack file promote that changes one waits for an owner to promote it; a member's promote and a push's auto promote are refused.

Ingress and exits name placements too:

```bash
stackr ingress set eu-prod --stack shop --env prod
stackr exit set eu-prod default --stack shop --env prod
```

## Tags

An env with volumes or a managed database only fits a placement whose node has the `storage` tag. A placement used as an ingress pick needs `ingress`; as an exit it needs `egress`.

To keep a database off the nodes that run the apps, put it in its own stack, pin that stack to a storage placement and link it across nodes. An env is never split between nodes.

## What an admin does

```bash
stackr admin placement ls
stackr admin placement add eu-prod --node h4
stackr admin placement add eu-db --node h3 --open acme,shop
stackr admin placement open eu-db all
stackr admin placement rm eu-db
```

`default` is reserved: it can be repointed and never deleted. Without `--node` a placement has no node yet, and its envs wait. Giving it a node starts them.

Access is per placement: `--open all`, or the org slugs that may use it. Closing a placement never moves envs already there.

The server file declares nodes and placements:

```yaml
nodes:
  h3: {tags: [egress, storage]}
  h4: {tags: [ingress, egress]}
placements:
  default: {node: ctl}
  eu-prod: {node: h4, open: all}
  eu-db:   {node: h3, open: [acme]}
```

The file never creates a node. A node it names that has not joined shows as "Add this node" with the join line filled in. A joined node the file does not name stays, shown as unmanaged.

## Move a placement

Repointing a placement moves every env on it to the new node.

```bash
stackr admin placement move eu-prod h5
stackr admin placement move eu-prod h5 --yes
```

It prints the checks first: target online, mesh link, tags, disk (volumes plus 10%), memory, CPU architecture and source online. A failing check stops it. With all checks passing it asks, naming the envs and their expected downtime; `--yes` answers with the placement's name. The move runs as a job, one env at a time; the command follows it, or pass `--no-wait`.

If an env fails, the placement keeps its old node and the envs already moved stay on the new one. Fix the cause and run:

```bash
stackr admin placement retry eu-prod
```

The same flow runs when a server file repoints a placement, and when a pin change moves an env.

## When a node is gone

A move waits for an offline source: bring it back, or forget it. Forgetting a node writes it off and marks its envs lost (`stackr env lost`). Repointing a placement whose node was forgotten restores its envs from their backups on the new node:

```bash
stackr node forget h3
stackr env restore --placement eu-db --to h5
```

Restore is a command-line action, and never part of a server file.

A volume with no backup stops the restore. To go on with it empty, pass `--start-empty`; in the panel, the move's typed confirm names the volumes. While the node stays forgotten, tile and volume edits on its envs change only the records, so `stackr volume rm` can clear a volume first.

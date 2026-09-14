# Docker Swarm runtime

Status: shipped 2026-09-12.

Every install is a Swarm, including a one-node install. Tiles, one-shots,
Traefik, registry, panel, relay and node agent run as services; compose tiles
were removed.

## Decision 2: networks

Tenant environments and shared instances claim attachable overlays from
pre-created pools so Traefik does not roll for every new environment.

## Decision 3: registry

Builds always push to the managed registry, and nodes pull the immutable
commit image over its internal TLS route.

## Steps 7–8: nodes and placement

The global node agent carries local Docker operations. Durable home-node and
node-group constraints keep stateful workloads on their data; the task
resolver handles exec, logs and stats for scheduled replicas.

## Volume addendum

Local volumes pin consumers to one node. Moves use two rsync passes through
the agents, stop for the final delta, switch placement, verify the target and
only then remove the source. NFS and SMB are for shared files, not databases.

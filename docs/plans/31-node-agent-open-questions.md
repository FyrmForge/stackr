# Node agent decisions

Status: shipped 2026-09-10.

The panel and every global agent use the same image and an authenticated HTTP
contract. Per-node keys are encrypted at rest; agents expose only the Docker
operations required for exec, stats, volumes, builds and maintenance.

## Step 8: placement

Tiles store a durable home node when local data requires it. `node_group` is
an optional scheduling constraint, replicas are stateless-only, and encrypted
overlay policy is resolved per environment. See `infra/agent`, `infra/nodes`,
`infra/placement` and the stack config placement fields.

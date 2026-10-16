# Multi-node config as code + placement (design talk 2026-10-16, after v0.7.1)

Agreed:
- No scheduler. Placement names in config, one node each, repointed by an admin (repoint = drain/move).
- Plan errors: undeclared placement name, node not open to the org, wrong capability tag (ingress/egress/storage).
- Plan warns: placement declared but no node yet (envs wait), node offline (new work waits).
- Moving off an offline node waits for it; restore from backup only via "Forget node" (admin), never from a file.
- `storage` capability tag: envs with volumes/managed DBs only pin to storage nodes (plan error otherwise).
- Mixed env (DB on storage, apps elsewhere) = DB in its own stack + cross-node link. No tile splitting.
- Server setup on first login, sibling of org setup: by hand or server config file. Restore is docs only, not in the UI.

- Server file shape: `nodes:` (name -> tags, open) and `placements:` (name -> one node). File declares, never creates; unjoined node -> "Add this node" with prefilled curl line; joined node not in file -> shown unmanaged, never removed.
- Placement node is live state (option C): file `placements: eu-prod: {node: h4}`; panel/CLI move wizard and file change use one move flow.
- Move flow: CI-style checks (target online, mesh link up, open to orgs, needed tags storage/ingress/egress, disk >= volumes + margin, memory, CPU arch, source online else forget+restore). Plan with a placement move is BLOCKED until checks pass; then Apply opens a confirm dialog naming from/to, the envs and their expected downtime. Apply starts the move job, does not wait. Partial failure: placement keeps old node until all envs moved; Retry resumes. Reuses the existing blocked/risky plan gate.
- Plan fills gaps in place: unset secret -> field on the plan; missing node -> "Add this node" opens the existing Add node dialog prefilled (name, tags, orgs from the file), shows the curl line.
- Org/stack files: `pin: <placement>` at org/stack/env. Ingress/exit follow the env's placement (its node if tagged, else cluster default); an override names a placement, never a node.
- Devs can list the placements open to their org: name, tags, public IP yes/no, online, usable by them.
- Access is per placement, not per node: `open: all | [orgs]` lives on `placements:` (default all). Nodes are admin-only hardware, abstracted away from orgs/stacks/envs; node names shown to admins only.

Open:
- Optional: installer restore --from a backup destination (S3).

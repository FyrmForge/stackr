# Multi-node UI

Status: shipped 2026-09-10.

The Servers surface lists Swarm nodes, produces one-time join scripts and
supports group changes, drain, removal and two-pass volume moves. Tile drawers,
plans and the canvas show placement, replicas, pending moves and per-node
health. The code lives in the server handlers, shared modal and plan
components, graph placement code, `infra/nodes` and `infra/volmove`.

# Cluster-only Docker access

Status: shipped 2026-09-12.

Thirty-three direct runtime call sites across eleven packages were moved
behind `infra/cluster`; storage volumes are created on their owning server and
the gate test prevents regressions. The cluster package is now the only code
allowed to choose a Docker endpoint for node-local work.

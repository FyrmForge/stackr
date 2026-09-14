# Cluster review fixes

Status: shipped 2026-09-13.

BuildKit left the ingress port, local storage began pinning consumers, volume
move waits for the redeployed service, the signer is loaded once, and tests no
longer persist their key in the tree. Implementations and regression coverage
live in deploy, placement, volume move, secrets and their tests.

# Cluster boundary

Status: shipped and rig-verified 2026-09-12.

`internal/stackrd/infra/cluster` is the single entry point for Docker work and
selects the manager socket or a node agent from resource placement. Managed
database provisioning, handlers and infrastructure packages no longer carry
their own local-versus-remote dispatch.

## Step 6: enforcement

`infra/cluster/gate_test.go` fails when container, image or volume operations
are added outside the cluster package.

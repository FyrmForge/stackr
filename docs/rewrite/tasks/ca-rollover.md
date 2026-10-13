# CA rollover, break glass, controller cert allow list

Status: plan agreed point by point with darthvader 2026-10-13. Not built.
Belongs with `registry-imports.md` (same round of multi-node work).

## Today

- `internal/clusterca`: one self-signed root made at install, 10 years
  (`CALifetime`), kept in `keys/cluster-ca.*` on the controller. Node and
  controller certs live 90 days and renew themselves (`certrenew.go`, through
  the tunnel or the panel).
- Every node trusts exactly that one root (`worker.LoadNodeCert`, the CA file
  written at join). Nothing can replace it.
- Workers check controller certs against the root only: no revocation
  (`cmd/stackrd/worker.go` `workerTLS`, `ponytail:` note).
- `RejoinNode` (`internal/service/nodeapi.go`) mints a join command for a
  broken worker; the node keeps its name, addresses and envs.

## Agreed

1. **Planned rollover (overlap).** The controller makes a new root and sends
   every node a trust list holding old and new. From then on every cert is
   issued from the new root, and the controller reissues every node at once
   instead of waiting for renewal. When every node holds a new-root cert, the
   old root leaves every trust list and is deleted. No node ever stops
   trusting another during the swap.
   - **Automatic** when the root has a year left.
   - **"Rotate root" button and CLI command** for doing it early.
   - A node offline during the swap gets the trust list when it returns
     (tunnel push or its recovery fetch through the panel) as long as the old
     root is still listed; the old root is dropped only when every node,
     offline ones included, holds a new-root cert, or an admin forces it
     (offline nodes then need a rejoin).
2. **Break glass.** "Something happened": the panel (and CLI) wipes the root
   and every WireGuard key in one action, makes new ones, and every node the
   admin wants to keep runs one command and comes back.
   - The command is a **rejoin**, not a fresh join: the node keeps its name,
     pinned envs and addresses; only its keys are new.
   - **One command per node**, single use, short lived; a leaked command
     rejoins only that node, and the panel shows which node joined.
   - Until a node rejoins, tunnels to it are down: apps keep running where
     they are, but traffic between nodes, routing through nodes and deploys
     stop. The confirm screen says so.
   - Nodes not rejoined stay listed as "not rejoined" with their apps flagged
     until removed.
   - Out of scope: a broken-into controller. A button on that panel can't be
     trusted; that is restoring the panel onto a fresh box (`recovery.md`).
3. **Controller cert allow list.** Workers accept only the controller certs
   the controller announced: the current one, plus the next during a renewal
   (announce first, switch after every reachable worker has it). Revoking =
   the controller issues itself a new cert and announces it; the old one stops
   working everywhere. The list rides the mesh spec each worker already gets
   (tunnel push and the panel recovery fetch). A worker that missed it refuses
   the unknown cert and recovers through the panel fetch, as for a missed
   mesh spec.

## Rules for every worker

No git write ops except commits on your own worktree branch (files by name,
never `-a`/`-A`, one-line messages, no AI attribution, never push, never touch
`multi-node`/`master`). Never hand-edit generated files (`*_templ.go`,
`ui/static/js/elements/*`, `output.css`, `docs/openapi.json`,
`staticmanifest.go`); regenerate. Touch only the files you own; needs elsewhere
go into `ca-rollover-seams.md`. No em dashes. Match neighbouring style;
`ponytail:` for ceilings. Never print secrets (keys, tokens, certs' private
parts). **Test first.** Finish with `go build ./... && go vet ./...`, the
tests of every touched package, `golangci-lint run` on them.

## Wave 0: contract (one worker)

1. `internal/clusterca`: verification against a **trust list** (several
   roots) everywhere a single root is taken today (`Verify`, `verifier`,
   `ServerConfig`, `ClientConfig`, `WorkerServerConfig`,
   `ControllerClientConfig`); `SaveCA`/`LoadCA` for a current and a previous
   root; PEM bundle helpers. A one-root list behaves exactly as today. Tests.
2. Mesh spec / recovery answer gain `TrustRoots []string` (PEM) and
   `ControllerSerials []string`; worker-side types and the controller-side
   builder fields, empty = today's behaviour.
3. Store: CA state (current root, previous root, rollover started/finished,
   break-glass time) in settings or a small table; node rows gain the root
   their cert was issued from (serial already stored in `node_certs`: add the
   issuer fingerprint).
4. Stubs with doc comments: `RotateRoot(ctx, by)`, `rootRolloverTick(ctx)`,
   `BreakGlass(ctx, by)`, `announceControllerCert(ctx)`.
5. `ca-rollover-seams.md` hand-off.

## Wave 1: in parallel

### C1 Worker side
Owns `internal/worker/cert.go`, `internal/worker/mesh.go` (trust parts),
`internal/worker/recover.go` (trust parts), `cmd/stackrd/worker.go`.
- Keep the trust list in a file next to the CA file; apply `TrustRoots` from
  the mesh spec or recovery answer atomically; TLS configs read it live (no
  restart).
- Accept a controller only when its serial is in `ControllerSerials` (empty
  list = today's behaviour, for old controllers during the upgrade).
- A worker refusing an unknown controller cert still reaches the panel
  recovery fetch.

### C2 Planned rollover (controller)
Owns new `internal/service/carollover.go`, `certrenew.go` (issuing side).
- `RotateRoot`: new root, trust list old+new pushed to every node, every node
  and the controller reissued from the new root, old root dropped when every
  node's cert is from the new root (or forced: offline nodes marked needs
  rejoin).
- `rootRolloverTick`: daily, starts `RotateRoot` when the root has under a
  year left; resumes an unfinished rollover after a panel restart.
- Join commands carry the current root's fingerprint.
- Every step audited to the security log.

### C3 Controller cert allow list
Owns `announceControllerCert` and the controller cert renewal.
- Renewal: issue next cert, announce current+next, switch when every
  reachable worker acknowledged (or after a bound), announce current only.
- "Revoke controller cert" = renew now; the old serial leaves the list.

### C4 Break glass
Owns new `internal/service/breakglass.go`, reuse of `RejoinNode` / `mintJoin`
and WireGuard key rotation (`rotate.go`).
- `BreakGlass`: new root (old one deleted, no overlap), new controller cert,
  new WireGuard keys; every worker marked "not rejoined"; per-node single-use
  short-lived rejoin commands on demand; rejoin keeps name, envs, addresses.
- Audited; the panel's own sessions stay valid (only cluster trust changes).

### C5 UI, API, CLI
Owns the admin cluster/security drawer parts, API routes, CLI
(`stackr cluster rotate-root`, `stackr cluster break-glass`,
`stackr node rejoin <node>` already exists: reuse).
- Root info: created, expires, rollover state (and which nodes still hold an
  old-root cert).
- "Rotate root" (confirm), "Break glass" (typed confirm, the gap spelled out),
  per-node rejoin commands after break glass, "not rejoined" state on node
  cards and the map.

## Wave 2: rig proofs

1. Rotate root: all nodes reissued from the new root, no tunnel or app gap
   (hello apps keep answering), old root gone afterwards.
2. Rotate with h3 offline: h3 comes back and picks up the trust list; old root
   dropped only after h3 is reissued.
3. Controller cert revoke: old controller cert refused by every worker
   (handshake with the old cert fails), new one accepted.
4. Break glass: every node offline from the cluster; rejoin ct110 and h4 with
   their commands; they come back with the same names, envs and addresses;
   vm106 and h3 show "not rejoined" until rejoined; then rejoin them.
5. Automatic trigger: with a test root that has under a year left (test-only
   lifetime knob or fake clock), the daily tick starts the rollover.

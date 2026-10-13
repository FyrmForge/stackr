# CA rollover: contract and seams (Wave 0 hand-off)

What Wave 0 put in place for C1 to C5 of `ca-rollover.md`. Everything below
builds and is tested; a one-root cluster behaves exactly as before.

## 1. `internal/clusterca` (all callers updated)

- `type Roots func() []*x509.Certificate`: the live trust list. A func so a
  listener reads it per handshake (no restart). `StaticRoots(certs...)` for a
  fixed list.
- `Fingerprint(c *x509.Certificate) string`: hex SHA-256 of the DER. The name
  of a root in join commands, `cluster_ca` and `node_certs.issuer`.
- `Verify(roots []*x509.Certificate, chain, role, nodeID, now) (Peer, error)`:
  was a single `caCert`. First root that signed the leaf and is inside its own
  validity vouches. Error text for one root is unchanged.
- `Peer.Issuer`, `Issued.Issuer`: fingerprint of the vouching / signing root.
- `ServerConfig`, `ClientConfig`, `WorkerServerConfig`,
  `ControllerClientConfig` take a `Roots` where they took `*x509.Certificate`.
- Files: `CAPrevCertFile = "keys/cluster-ca.prev.crt"` (previous root, cert
  only: nothing signs with it again). `LoadPrevCA(dir) (*x509.Certificate,
  error)` (`os.ErrNotExist` = no rollover), `RotateCA(dir, next *CA)` (current
  becomes previous, next becomes current; previous written first),
  `DropPrevCA(dir)` (missing is fine), `TrustRoots(dir)` (current, then
  previous if any). `SaveCA`/`LoadCA` are unchanged and mean "current".
- PEM: `BundlePEM([]*x509.Certificate) []byte`, `ParseBundlePEM([]byte)`,
  `RootsToPEM(roots) []string` (one PEM per root, the wire form),
  `RootsFromPEM([]string)` (empty in, nil out, no error).

Callers moved to the list:
- Worker (C1 owns the trust parts): `internal/worker/cert.go` (`renewOnce`,
  `renewTunnel`), `internal/worker/mesh.go` (`startRegistry`),
  `cmd/stackrd/worker.go` (`workerTLS`, registry client) wrap the single CA
  file read in `clusterca.StaticRoots(ca)` / `[]*x509.Certificate{ca}`.
  `Server.clusterCA()` still returns one cert. C1 replaces these with a live
  `Roots` backed by the trust-list file.
- Controller: `clusterPKI.roots()` (live list, current plus previous) feeds
  `renewTunnel`, `openSigned`, the registry front, and the worker clients
  (`probeClient` and the rest of `ClientConfig` users in `nodeclients.go`).
  `registry.go` `nodeFacts` reads `clusterca.TrustRoots(DataDir)`.
  `bootCluster` loads the list from disk.

## 2. Mesh spec / recovery answer

- `worker.MeshSpec` (`internal/worker/mesh.go`) gains
  `TrustRoots []string` (PEM, one root each) and `ControllerSerials []string`
  (hex serials), both `omitempty`. Empty = today's behaviour. The recovery
  answer is a `MeshSpec` (`RecoverNode` returns `o.mesh.Spec`), so one field
  serves tunnel push and panel fetch. The worker side only carries them; C1
  applies them.
- Controller: `meshSvc.Trust func(ctx) (roots, controllerSerials []string)`
  (`internal/service/mesh.go`), set in `multinode.go` to `o.meshTrust`
  (`nodeclients.go`). `plan` copies both onto every worker spec, never the
  controller's own. Today `meshTrust` sends the one current root PEM (two
  during a rollover) and an empty serial list.
- Source of the values: `clusterPKI.roots()/setRoots()` (C2 swaps the list on
  rollover), `clusterPKI.serials()/setSerials()` (C3 sets the announced list).
  After either, force a mesh sync (`o.mesh.Sync(ctx, "", true)`) so workers
  get it; unchanged specs are not re-pushed.
- C1 semantics to keep: a spec with empty `TrustRoots` must not wipe the
  worker's trust file; empty `ControllerSerials` = accept any controller from
  a trusted root.

## 3. Store

- Migration `023_cluster_ca.{up,down}.sql`:
  - table `cluster_ca` (single row, `id = 1`): `current_fp`, `previous_fp`
    (`''` outside a rollover), `rollover_started_at`, `rollover_finished_at`,
    `break_glass_at` (nullable times).
  - `node_certs.issuer TEXT NOT NULL DEFAULT ''` (root fingerprint; `''` for
    certs from before this).
- `store.ClusterCA` + `ClusterCAStore{Get(ctx) (ClusterCA, bool, error);
  Save(ctx, ClusterCA) error}` (`internal/service/internal/store/cluster_ca.go`,
  `Tables.ClusterCA`). `Get` answers `false` before the first `Save`: no
  code writes the row yet, C2 (and C4) do on first rollover / break glass.
  `ClusterCA.RollingOver()` = started and not finished since.
- `store.NodeCert.Issuer` filled by all three issue sites
  (`nodeclients.go` controller cert, `certrenew.go` `renewNodeCert`,
  `join.go` confirm) from `Issued.Issuer`. New `NodeCertStore.ListLive(ctx)`
  (unrevoked, oldest first) for "which nodes still hold an old-root cert".

## 4. Stubs (all return `errors.ErrUnsupported`; contract in each doc comment)

| Stub | File | Filled by |
|---|---|---|
| `(*Orchestrator).RotateRoot(ctx, by *Principal) error` | `internal/service/carollover.go` (new) | C2 |
| `(*Orchestrator).rootRolloverTick(ctx) error` | `internal/service/carollover.go` | C2 |
| `(*Orchestrator).announceControllerCert(ctx) error` | `internal/service/nodeclients.go` | C3 |
| `(*Orchestrator).BreakGlass(ctx, by *Principal) error` | `internal/service/breakglass.go` (new) | C4 |

`announceControllerCert` and `rootRolloverTick` are already called from
`dailyChores` (`multinode.go`), which ignores `ErrUnsupported`; C2 and C3 keep
that wiring (C3 may also call announce straight after a renewal).
`RotateRoot` and `BreakGlass` are not wired to any route yet: C5 calls them.

## 5. Notes for the workers

- C2: `caFingerprint(ca)` in `join.go` equals `clusterca.Fingerprint(ca.Cert)`;
  fold it when you make join commands carry the current root. The forced
  finish ("offline nodes need a rejoin") needs a node state: add it, and list
  it here.
- C2/C4: `clusterPKI.ca` is read without the lock in places; swap it only
  together with `setRoots` and hold no `c.mu` while issuing.
- C4: `clusterca.RotateCA` keeps a previous root; break glass does not use it
  (`SaveCA` of a fresh CA plus `DropPrevCA`).
- Security-log actions for the new events are not added; C2/C3/C4 add theirs
  in `internal/service/internal/leaf/securitylog/securitylog.go`.
- Surface (C5): nothing on API, CLI or UI exists yet.

## C4 for C5 (break glass)

- `BreakGlass(ctx, by) error` (`internal/service/breakglass.go`): admin only
  (`node.manage`). Refuses (`errs.Conflictf`) a second run within 10 minutes
  (`breakGlassGap`); no force flag. It mints no commands.
- State: C5's `ClusterTrust` (`BreakGlassAt`, `NotRejoined`, derived from
  "no live cert since break glass"); break glass also sets the worker row to
  `StateNeedsRejoin` so the mesh plan cuts it off.
- Per-node command: `RejoinNode(ctx, by, ref)` as before (single use, 1h
  default TTL, carries the new root fingerprint). Minting again is fine; the
  older command of that node stays valid until spent or expired. The admin
  still confirms the node (`ConfirmJoin`, Pending joins) as for any rejoin.
- Workers in `needs-rejoin` are skipped by the mesh plan (`mesh.go`), so
  their tunnels are down from the moment the state is set, including a plain
  `RejoinNode` on a live node (behaviour change from before).
- Security log: `ca.break_glass` (twice: "started", then "done: ...") and
  `node.rejoined` (on confirm of a rejoin, next to `node.confirm`).
- Break glass also: expires every unspent join token, revokes every worker
  cert, announces the new controller cert serial alone, rotates the
  controller's mesh key (`rotateController`), clears `PreviousFP` and the
  rollover stamps in `cluster_ca`.
- Not done: pending join requests stay pending (confirm issues from the new
  root, but the box holds the old fingerprint and refuses; reject and
  re-mint).

## C4 needs

- `cmd/stackr-install/join.go` (+ `worker.EnsureWGKey`): a rejoin reuses
  `wg.key` and `exit.key` in the state dir, so the worker keeps its old mesh
  keys. For "new WireGuard keys" the join must delete both when a
  `node.crt` already exists (a rejoin) before `EnsureWGKey`. Not changed by
  C4 (not its file).
- C1: a rejoining worker writes `cluster-ca.crt` from the join answer; the
  trust-list file next to it must be reset to that one root, or the old root
  survives there (join writes only the CA file today).
- `JoinTokenStore.GetByNode` now returns the newest row; it returned an
  arbitrary one, so a rejoin of any node that joined with a token before hit
  "not found" at confirm.
## C3 for C5, C2 and C4 (controller cert allow list)

File: `internal/service/controllercert.go` (and `nodeclients.go` for issuing).

- C5: `(*Orchestrator).RevokeControllerCert(ctx, by *Principal) error` is an
  immediate renewal: current+next announced, controller switches once every
  reachable worker took the list (bound `controllerAckBound`, 5 min, logged),
  next announced alone, old serial revoked in `node_certs`. Audited as
  `securitylog.ControllerCertRevoke`; a due renewal is `ControllerCertRenew`.
  `errors.ErrUnsupported` without a cluster. Blocks up to the bound: run it in
  a goroutine or tell the admin it can take minutes.
- C2: after swapping `o.cluster.ca` (with `setRoots`), call
  `o.announceControllerCert(ctx)`. It sees a controller cert not signed by
  the current root as due and reissues it from the new root through the
  same announce, switch, announce-alone steps. Hold no `c.mu`; the call takes
  `rollMu`.
- C4: break glass must not announce the old controller serial. After
  `SaveCA` of a fresh CA, set `o.cluster.ca` and `setRoots`, then
  `issueController(ctx, st, ca)` + `.save(DataDir)`, swap `o.cluster.own`, and
  `setSerials([]string{serial})` (no overlap). `announceControllerCert` would
  announce old+new first, which is wrong there.
- The list is set at boot (`bootCluster` announces the current serial), so
  workers enforce from the first sync after an upgrade. `controllerCert` no
  longer renews a due cert at boot (only a missing, expired or wrong-root one);
  `announceControllerCert` does, from `dailyChores`, which `New` runs at boot.

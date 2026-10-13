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

## 6. C2 (planned rollover) as built

Files: `internal/service/carollover.go` (+ test), `certrenew.go` comment,
`join.go` (`caFingerprint` = `clusterca.Fingerprint`), security log actions
`root.rollover_start`, `root.rollover_push`, `root.rollover_node`,
`root.rollover_finish` (`securitylog.RootRollover*`).

- Trust list order: current (signing) root first, previous second. After
  `RotateRoot`, `clusterPKI.ca` is the new root; every issue site signs with it.
- A worker is never handed a cert (its key stays on it): it asks. The
  controller cert is reissued at once; each worker is on the new root after
  its next renewal. State is `cluster_ca`; the key files are the truth
  (`reconcileRollover` mends a crash either way).
- Finish: every confirmed worker's newest live cert is from the new root and
  at least `rolloverSettle` (6h) old (a worker that lost the renewal answer
  keeps its old cert and must still verify). The daily tick checks; there is
  no event-driven finish.
- New node state for a forced finish: the existing `StateNeedsRejoin`
  (`needs-rejoin`) with its certs revoked.
- Join commands: `mintJoin` reads the current root from disk, so the
  fingerprint follows a rotation; a node joining mid-rollover gets a new-root
  cert and the new root as its CA file, and the full list from its mesh spec.

### C2 for C5

On `*Orchestrator` (`internal/service/carollover.go`), all node admin
(`nodeAdmin`):
- `RootInfo(ctx, by) (RootInfo, error)`: `Fingerprint`, `CreatedAt`,
  `ExpiresAt`, `Rolling`, `PreviousFingerprint`, `OldRootNodes` (node names
  still on an old-root cert, only while rolling), `RolloverStartedAt`,
  `RolloverFinishedAt`.
- `RotateRoot(ctx, by) error`: Conflict while one runs.
- `ForceFinishRootRollover(ctx, by) error`: Conflict when none runs. The
  confirm must say nodes still on the old root are marked needs-rejoin.

### C2 needs

- C1 (worker): renew the node cert at once, not only in the last third, when
  the cert's issuer is not `TrustRoots[0]` of the applied mesh spec (the first
  root is the one the controller signs with), and keep asking until it is
  (hourly check is enough). Today `renewOnce` only renews when due or on the
  `renewNow` flag. Without this the rollover waits for natural renewal (up to
  60 days) and the admin must force it.
- C3 (`announceControllerCert`): the rollover reissues the controller cert
  through `clusterPKI.renew` (new serial) and then calls
  `announceControllerCert`. With an allow list active the new serial must be
  announced before workers see it; C3 should have `renew` announce
  current+next first when the signing root changed, or accept the short gap.

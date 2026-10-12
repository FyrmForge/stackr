# Multi-node, manual placement

Status: plan written 2026-10-12, not started. Agreed with darthvader
2026-10-10 to 2026-10-12 (voice). Design: fraedi folder `stackr/multi-node`
(RFC 7ff2684d, graph 8bb91374, lifecycle flows 499410c4). Step 0 (network
test on pve) done 2026-10-12, all four tests passed; its fixes are folded
into the steps below.

Add machines when you want more power and pin orgs, stacks or envs to them
by name. No scheduler. The env is the smallest thing that can be pinned and
never spans nodes. The controller node runs the panel; every other node is a
worker and runs `stackrd worker`. WireGuard on every link. (In code and docs
"head" still means the controller; users only see controller and worker.)

## Rules for every step
- Each step ships on its own and ends with a proof on the rig. A step that
  needs a second machine proves it on two throwaway LXC containers on pve,
  like step 0.
- Fake mode grows with the feature: anything a worker does has a fake that
  `hamr dev` can run on a laptop (see step 2).
- No users yet: no compat code, pave the rig when a schema or range changes.
- Every certificate and node action, however small, writes the security
  audit log (step 1, task 9).
- Steps 2 to 8 are outlines. Each gets its task list when the step before it
  is done, with what that step taught us.

## Step 0: network test on pve (done 2026-10-12)
Three throwaway CTs, Docker 29.9, nft iptables. Results in the RFC. What
changed the plan:
- Docker 28+ drops wg0 to container traffic in `raw PREROUTING` per
  container. Every node needs a `STACKR-RAW` accept from wg0 for the
  cluster supernet into its own range.
- The no-SNAT rule into wg0 must be `ACCEPT` in nat, not `RETURN`.
- Node ranges sit in one aligned supernet, `10.208.0.0/12`, one /16 each.
- wg0 INPUT is default drop, or every peer reaches every host's sshd.
- Accepts match the pre-NAT tuple (`--ctorigdst`), as the reviews said.
- Exit nodes: one extra wg interface per exit, CONNMARK reply routing on
  the exit, health by ping.
- Relay: a small UDP forwarder; re-learn a side's port only from handshake
  packets.
- Replay after reboot works: wg, routes, our chains, then the jumps, after
  `docker.service`, waiting for `DOCKER-USER`.

## Step 1: groundwork on one node
Nothing visible changes for a single-node install. Everything below is the
shape the later steps need, built while there is only the head. Order and
fixes from the Codex review of 2026-10-12.

Placement terms: `environments.node_id` is where the env actually runs.
Org and stack pins only decide where new envs go. Changing a pin never moves
an existing env until step 8 (moves); before that, the UI says so.

1. **Schema.** Migration `011_nodes`:
   - `nodes` (id, name, slug, role `controller` | `worker`, tags, range CIDR,
     wg public key, wg address, status, created_at). No controller
     generation in v1; it comes with promotion.
   - `environments.node_id` (not null) and `environments.subnet` (the env's
     /24, null until reserved).
   - `orgs.node_id`, `stacks.node_id`: nullable pins for new envs.
   - `security_events` (time, actor kind and id, action, target kind and
     id, outcome, detail JSON), append only.
   Files: `internal/db/migrations/011_nodes.up.sql` and `.down.sql`.
   Done when: migrations run up and down on a fresh DB.
2. **Stores and leaves.** `store/nodes.go`, `store/security_events.go`,
   and a leaf each: `leaf/node` (owns node rows, status, ranges) and a
   security-event leaf (append, list with filters). Placement resolution
   (env, then stack, then org, then the controller) lives in
   `leaf/environment`, used by every env create path; `CloneRow` copies
   `node_id` itself. A `storetest` helper seeds the controller; direct env
   fixtures in tests set `node_id`.
   Files: new store and leaf files, `store/stacks.go`, `store/orgs.go`,
   `leaf/environment/environment.go`, `storetest/storetest.go`, env test
   fixtures (`leaf/environment`, `store/store_test.go`, others that insert
   envs).
   Done when: `make test` passes; a leaf test covers pin resolution.
3. **Controller bootstrap, the head as a role.** At boot the node leaf
   makes the controller row when missing (range `10.213.0.0/16`) and writes
   one `node.bootstrap` security event. "The head" in code means the
   controller row, never "this process". The orchestrator builds its node
   maps from the rows.
   Files: `internal/service/orchestrator.go`, `leaf/node`.
   Done when: a fresh DB boots with exactly one controller and one event.
4. **Subnets reserved, not picked inside Docker.** Cluster supernet
   `10.208.0.0/12` as a constant. A reservation in the node leaf hands out
   the next free /24 of the node's range, checked against host interfaces
   and existing Docker networks, inside a store transaction; env networks
   store it on `environments.subnet`. `EnsureNetwork` takes the resolved
   subnet instead of choosing one. Every allocator goes through it: env
   networks, managed shared networks, ingress networks.
   Files: `internal/service/internal/docker/networks.go` and test,
   `leaf/environment/environment.go` (network create), `leaf/domain/domain.go`
   (ingress networks), the managed network path, `fakehost/fakehost.go`
   (stops reading `docker.EnvRange`), `dockerfake/fake.go`.
   Done when: the picker test covers a range other than 10.213; fakehost
   and dockerfake compile against the new signature; `hamr dev` with the
   fake host still deploys.
5. **Docker router.** One `service.Docker` implementation that picks the
   node client from a node id in ctx (`WithNode`). A missing node id is an
   error, so every caller decides:
   - env-bound work (deploy, logs, exec, terminal, forward, shares,
     volumes, limits): the env's node;
   - controller-only work (proxy inspect, ingress reopen, panel backup):
     the controller;
   - multi-env jobs (push, org config, server config, image watch): per env;
   - per-node work (VIP rebuild, prune): a loop over nodes.
   `watchTick` splits into those three scopes. Cleanup prunes per node and
   only then decides which image rows to delete. `checkLimits` takes the
   env's node now, not in step 4.
   Files: `internal/service/docker.go`, `orchestrator.go`, `jobs.go`,
   `limits.go` and its callers, `tile.go`, `forward.go`, `cleanup.go`,
   `leaf/image/image.go`, `vipboot.go`.
   Done when: all tests pass through the router; a test proves a missing
   node id fails loudly.
6. **VIP state per node.** `o.vip` becomes a per-node lookup, and so do
   `earlyIPs`, `sweepEarly`, their locks and the rebuild bookkeeping.
   `vip.Base.Range` is the node's range; a new `Cluster` field carries the
   supernet.
   Files: `internal/service/internal/vip/vip.go`, `filter.go`,
   `internal/service/vipboot.go`, `orchestrator.go`.
   Done when: VIPs render the same as today on one node (golden test).
7. **Firewall core.** Transit net `10.207.255.0/24` (outside the supernet,
   out of 100.64/10). In STACKR-FWD and STACKR-IN, DROPs for the supernet
   and the transit net go before the lan RETURNs and right after the local
   range's own RETURN; the `! -s <local range> RETURN` line is kept but
   tested against a remote-range source. New empty chains with jumps:
   `STACKR-RAW` (raw PREROUTING, rule 1), `STACKR-NAT` (nat POSTROUTING,
   rule 1). `STACKR-XN` is jumped from inside `STACKR-FWD`'s parent slot,
   not a second rule-1 claim on DOCKER-USER.
   Files: `internal/service/internal/vip/filter.go`, `vip.go`,
   `filter_test.go`.
   Done when: render tests cover a local tile with a `10.0.0.0/8` lan grant
   and a remote-range source, both kept out of the supernet;
   `iptables-save` on the rig shows the chains hooked once.
8. **Panel state set.** One list in `installspec` names what the panel
   needs to come back: the DB, `keys/` (each file named), the Caddy volume,
   the head's WireGuard key and the cluster CA key (both from step 2 on),
   later the registry storage. `PanelBackup` builds the archive from it;
   `stackr-install restore` accepts and restores exactly that list. That
   restore is also v1's manual promotion: on a fresh or wiped box it brings
   back the same head (same CA key, same WireGuard key).
   Files: `internal/installspec/installspec.go`,
   `internal/service/internal/flow/backup/backup.go` (`panelTar`),
   `cmd/stackr-install/restore.go` and its tests.
   Done when: backup and restore round-trip a panel with an extra file under
   `keys/`; a test fails if the two sides disagree.
9. **Security audit log writers, page, API, CLI.** Writes happen in the
   orchestrator, in the same store transaction as the action:
   - `Orchestrator.Login`: success and bad credentials, with request
     metadata passed in; rate-limit refusals count as failed logins;
   - API keys: in the shared `mintKey` (covers the CLI browser exchange)
     and revoke;
   - elevated grants: approve and revoke (`RevokeHostGrant` gains the
     actor);
   - secret reveal: an orchestrator method replaces the `slog` line in
     `internal/api/handler/v1/data.go`, given the principal.
   Admin page "Security log" with filters, `stackr audit ls`, an admin API
   route with authz, regenerated OpenAPI.
   Files: `internal/service/users.go`, `admin.go`, `clilogin.go`,
   `hostgrant.go`, `internal/api/handler/v1/data.go`,
   `internal/api/routes.go`, `internal/authz/authz.go`,
   `cmd/stackr/cmds.go`, the web login handler, one admin page, OpenAPI.
   Done when: each writer leaves exactly one row, and a failed audit insert
   rolls the action back.
10. **Proof.** `make test` and `hamr dev` on the fake host. Then pave the
   rig with the new build: deploys, managed DBs, backups and restore,
   grants, PR envs all still work; `iptables-save` shows the chains; a 10/8
   lan grant cannot reach the supernet; the security log shows logins, a
   key, a grant and a reveal.

Dropped after review: "managed instances get a VIP". Every non-host-network
tile, managed ones included, already has a VIP on its env network
(`flow/deploy/deploy.go` `route`); a cross-node link targets that one.

## Step 2: worker process, certificates, fake mode (outline)
- Slim mTLS. One cluster CA key made at install, kept in `keys/` with the
  other panel keys and in the normal panel backup. It signs the head's cert
  and each node's cert directly. Go stdlib only. The control channel never
  assumes WireGuard is underneath.
- `stackrd worker`: a gatekeeper in front of the local docker.sock. It checks
  the controller's certificate (mTLS) and passes the Docker API through
  unchanged, streams included; the controller uses the normal Docker client
  pointed at the node. Plus a small versioned extras API (facts, iptables
  payloads, wg config, saved state and replay, Caddy admin socket). Node
  role rules (a build-only node refuses runs) are enforced on the controller.
  Upgrades: workers first, then the panel; the controller accepts workers one
  version behind; the extras API only ever adds calls, with a test of the new
  controller against the previous worker.
- Join flow: token (hashed, single use, short TTL, POST over https), CSR,
  CA fingerprint in the command, admin confirm, cert back. Rejoin button.
- Renewal at a third of lifetime left; expired cert renews only over the
  node's own WireGuard peer (step 3); one hour of clock slack.
- Every issue, renewal, heal and revoke writes the security audit log.
  A leaked CA key in v1 means a new CA and every node rejoins.
- Fake mode: `hamr dev` runs N workers, each on `data/fakehost/<node>`,
  over the real worker API on localhost. Router entries for remote nodes
  point at workers.

## Step 3: mesh (outline)
- wg0 per node, transit net outside the supernet, peer push from the head,
  routes per node range. Installer join mode.
- Firewall fill: STACKR-RAW wg0 accept, STACKR-NAT `-o wg0 -s supernet -j
  ACCEPT`, wg0 INPUT default drop with an allow list.
- Path order: private on a shared subnet, public, relay; handshake-age
  failover in the worker. Reports filtered as untrusted.
- Relay: UDP forwarder in the worker, one port per pair, re-learn only on
  handshake packets.
- The worker persists its full applied state; replay unit after
  `docker.service`.
- Fake mode: topology file; recorded rules per node.
- Proof: two throwaway LXC nodes plus the rig head.

## Step 4: placement (outline)
- The controller is tagged ingress, egress and builder at install and is the
  default ingress and exit. Removing its ingress tag is refused until the
  panel route and every route have another ingress node. The panel's domain
  is a route like any other and follows the default ingress node.
- Pin org, stack, env by node name in UI, CLI and the org file (pins
  place new envs; moving existing ones is step 8).
- Node card: status, disk, usage, containers, tags, connection.
- Refuse lan, port and network:host grants and shares on envs not on the
  head; approve screen warns for elevated grants on the head.
- Range allocation checks every node's reported interfaces; forgotten
  ranges never reused.
- Remove node (move first) and forget node (cascade, recovery list).

## Step 5: cross-node links and ingress nodes (outline)
- Links: source RETURN, destination ctorigdst accept, host output = the
  instance's env-network VIP when nodes differ, skip the managed network
  join.
- Ingress: Caddy per ingress-tagged node, config generated per node and
  pushed to its local admin socket; HTTP-01 on the node; wildcard certs
  issued on the head (DNS token stays there) and pushed.

## Step 6: registry and builder nodes (outline)
- Standard registry image as a system container on the head, local only.
- stackr fronts it: node certs scoped to pinned envs' images, builders to
  their jobs; each worker serves `localhost:5000`.
- Devs: `registry.<panel domain>` through Caddy, `stackr registry login`,
  scoped per org. All tokens audited.
- Builder tag gets build jobs; context tar to the builder.

## Step 7: exit nodes (outline)
- Ordered exit list per org, stack, env or tile; default own node.
- One wg interface per exit in use, source policy routing with
  `suppress_prefixlength 0`, exit-side CONNMARK, AllowedIPs scoped to the
  assigned envs, private and metadata targets refused.
- The worker runs the ladder: ping through the exit, next rung on failure.
- Exit-routed envs get a public resolver.

## Step 8: moving envs (outline)
- Helper image with rsync. Live pass, stop, checksum pass, start in the
  new node's range. One firewall rule on the target for the source node.
- Rewire links, ingress routes and exits; remove old containers, keep old
  volumes.

## Later
- Panel standbys and promotion (flows in fraedi), and with them the full
  certificate setup: an intermediate under a sealed root, controller
  generations to fence an old head, rotation without rejoining, a recovery
  key, and the cluster security cert page.
- Bring-your-own CA (maybe a paid feature), hardware-held keys.

## Wave 1: parallel lanes (started 2026-10-12)
Each lane is one Sonnet worker in its own git worktree, branched from HEAD.
This file is untracked, so workers read it at its main-tree path:
`/home/darthvader/FyrmForge/stackr/docs/rewrite/tasks/multi-node.md`.

### Rules for every lane
- Read `AGENTS.md` (layering rules) and this file first. Follow the repo's
  existing patterns; reuse before writing.
- Own only the files your lane lists. Never edit a file on another lane's
  list or on the "nobody" list below. If you need a change there, write it
  down in your final report instead.
- Nobody edits in wave 1: `internal/service/orchestrator.go`,
  `cmd/stackrd/main.go`, generated files (`*_templ.go`, `generated/`,
  `docs/openapi.json`, `docs/llms*.txt`). Wiring into the orchestrator and
  main happens at integration.
- No UI pages, templ files or new CLI commands: designs are under review.
  Backend, store, API handlers only where your lane says so.
- Never run git write commands (commit, add, stash, reset, checkout of
  files). Leave every change unstaged in your worktree.
- Build and test with `go build ./...`, `go vet` and `go test` on your own
  packages inside your worktree. Do not run `make` targets that need the dev
  server.
- Migrations: only the number your lane is given.
- `go.mod`: only lane W6 adds dependencies, and only
  `golang.zx2c4.com/wireguard/wgctrl` and `github.com/vishvananda/netlink`.
- Terms: node roles are `controller` and `worker`. The node program is
  `stackrd worker`. Note `flow/jobs` also says "worker" for job runners; keep
  node code clear about which one it means.
- Plain comments matching the surrounding density, no em dashes.
- Final report: files changed, tests run and their result, anything you
  could not do, and every change you need in a file you do not own.

### Lanes
- **W1 Groundwork** (step 1 tasks 1 to 7). Migration `011`. Owns: the
  migration, `store/nodes.go`, `store/stacks.go`, `store/orgs.go`,
  `store/store.go`, new `leaf/node/`, `leaf/environment/`, `storetest/`,
  env fixtures in tests, `internal/service/internal/docker/networks.go`,
  `leaf/domain/domain.go`, `internal/service/managed.go`,
  `flow/managed/flow.go`, `flow/deploy/facts.go`, `flow/promote/promote.go`,
  `fakehost/`, `dockerfake/`, `internal/service/docker.go`, `jobs.go`,
  `limits.go`, `tile.go`, `forward.go`, `cleanup.go`, `leaf/image/`,
  `vipboot.go`, `internal/service/internal/vip/`. Orchestrator wiring it
  needs goes in its report as a patch.
- **W2 Panel state set** (step 1 task 8). Owns:
  `internal/installspec/installspec.go`, `flow/backup/backup.go`,
  `cmd/stackr-install/restore.go` and their tests.
- **W3 Security audit log backend** (step 1 task 9, no page, no CLI).
  Migration `012`. Owns: the migration, `store/security_events.go`, new
  `leaf/securitylog/`, new `internal/service/security.go`, and only the audit
  lines in `users.go`, `admin.go`, `clilogin.go`, `hostgrant.go`,
  `internal/api/handler/v1/data.go`, plus an admin list endpoint in
  `internal/api` and its `authz` verb.
- **W4 Cluster CA** (slim). Migration `013` (node certs: serial, node id,
  role, not after, revoked at). Owns new `internal/clusterca/` (pure Go
  stdlib: make CA, issue controller and node certs from CSRs, verify a peer
  chain and role, renewal due at a third of lifetime left, one hour clock
  slack, server and client `tls.Config` builders) and
  `store/node_certs.go`. Top-level `internal/` so the worker can import it.
- **W5 Worker process.** Owns new `internal/worker/` and new
  `cmd/stackrd/worker.go` (a `Run(ctx, cfg)` the integration calls from
  main). mTLS listener taking a `*tls.Config` from the caller; reverse proxy
  of the Docker API to the local socket, upgrades and hijacked streams
  (attach, exec) included; extras API v1 under `/v1/`: facts (interfaces
  with prefixes, which addresses are public and directly on an interface,
  disk, docker version, kernel, CPU, memory, uptime), iptables payload apply
  (`iptables-restore --noflush` of whole stackr chains), wg config apply
  through an injected interface, saved applied state (write, read, replay),
  Caddy admin socket pass-through. Version header; calls only ever added.
  Also new `internal/service/internal/workerclient/`: Docker client options
  (TLS, host) for a node plus a typed extras client.
- **W6 Mesh.** Owns `go.mod`/`go.sum`, new `internal/mesh/` (controller-side
  pure logic: peer lists, endpoint candidate order private same subnet then
  public then relay, untrusted report filtering, supernet and transit
  constants shared with W1 via this package), new `internal/wgapply/`
  (node-side: create wg interface with netlink, set peers with wgctrl, routes,
  handshake-age failover loop, a recording fake for fake mode), new
  `internal/relay/` (UDP forwarder, one port per pair, re-learn a side only
  from WireGuard handshake packets type 1 or 2).
- **W7 Exits.** Migration `014` (exit ladders: scope kind, scope id,
  position, node id). Owns new `internal/exits/` (node-side: per-exit wg
  interface plan, `ip rule` and route argv plans with
  `suppress_prefixlength 0`, exit-side CONNMARK and private and metadata
  refusal rules, ladder state machine with an injected pinger and runner)
  and `store/exit_ladders.go`, plus a pure resolver of the ladder for a tile
  (tile, env, stack, org, default controller) in `internal/exits/`.
- **W8 Registry front.** Migration `015` (registry tokens). Owns new
  `internal/service/internal/regfront/` (token mint and verify, scopes: node
  pulls images of envs pinned to it, builder pushes its jobs, user per org,
  admin all; Docker token-auth compatible; reverse proxy to the local
  registry), `store/registry_tokens.go`, and new
  `internal/installspec/registry.go` (the system container spec, same
  pattern as the proxy container).
- **W9 Ingress config per node.** Owns `flow/deploy/proxy.go` and
  `leaf/domain/caddy.go`: build one Caddy config per ingress node; upstream
  is the tile VIP and port when the tile runs on another node; the panel
  route follows the default ingress node.
- **W10 Moves.** Migration `016` (move state). Owns new `flow/move/` (cold
  move: stop, stream each volume with the existing tar helpers, untar on the
  target, start; then the live variant: rsync pass while running, stop,
  checksum pass), `store/moves.go`, and new `images/rsync/Dockerfile`.

### Wave 2 (after integration and design review)
Join flow and installer join mode, fake mode with N workers in `hamr dev`,
placement UI and CLI, node card and drawer, cluster graph, security log page
and CLI, registry login CLI, exit and ingress pickers, move dialog, forget
node, real two-node proof on pve.

## Wave 1b: integration lanes (started 2026-10-12, base commit f5b79b8 on `multi-node`)
Same rules as wave 1, with these changes:
- Base is the `multi-node` branch; all wave 1 code is in it.
- Only lane I1 edits `internal/service/orchestrator.go` and `cmd/stackrd/main.go`. Every other lane puts its wiring in its own new file as `func (o *Orchestrator) initX(...)` (or similar) and reports the one-line call to add.
- Integration todo per lane is in each lane's prompt (from the wave 1 reports).

Lanes:
- **I1 Worker and mTLS.** `stackrd worker` branch in main; cluster CA created on the controller at boot when missing, plus the controller cert (files `keys/cluster-ca.*`, issued rows in `node_certs`, audited); worker TLS from `clusterca.ServerConfig` with a revoked check; controller builds a Docker client per worker node from `workerclient.DockerOptions` and registers it in the router from the `nodes` rows; the extras client per node. Owns orchestrator.go, main.go, new `internal/service/nodeclients.go`, `internal/service/internal/docker/` client constructor changes.
- **I2 Ingress per node.** `wiring.go` switches to `ProxyConfigs` (default ingress, NodeRoute wrap, Placement from env node and tile VIP); the syncer pushes each node's config: the controller to its local socket, workers through the extras API Caddy pass-through. Owns `wiring.go` and the proxy sync code.
- **I3 Registry.** Mount regfront (token endpoint and `/v2/`) for `registry.<panel domain>` behind Caddy; Authenticate facts (node cert, user session or API key, admin); audit issued tokens to `registry_tokens` and the security log; key derived from the master key; installer and upgrade create the registry container from `installspec.Registry()`. Owns new `internal/service/registry.go`, the installer and upgrade files it needs, the web route registration for the registry host.
- **I4 Mesh and exits runtime.** Controller side: new `internal/service/mesh.go` builds peer lists (`mesh.Peers`), relay pairs and exit plans per node and pushes them to workers through a new additive extras call (`/v1/mesh`). Worker side: the worker runs `wgapply.Kernel` (or the recorder in fake mode), the failover loop, `relay.Manager`, the exit ladders with real runner and pinger; persists it all in the saved state; replay at start. Owns `internal/worker/`, new `internal/service/mesh.go`, `internal/exits/` runtime glue.
- **I5 Moves.** Job kind "move env", orchestrator verb (Begin + enqueue), job runs `Run`, boot resumes `Unfinished`; the Env adapter (stop on the current node, start on the target by setting `node_id` and deploying, remove containers); the one firewall rule on the target for the source during a move; `images/rsync` build in the Makefile release path. Owns new `internal/service/move.go`, `jobs.go`.
- **I6 Join flow backend and audit leftovers.** Migration `017` join tokens (hashed, single use, TTL, typed name and tags); API: admin creates a join token (returns the command with the CA fingerprint), unauthenticated POST join with token + CSR + WireGuard public key + facts creating a pending node, admin confirm or reject, node cert issued from the CA; all audited. Plus: admin params reveal goes through `RevealSecrets`; regenerate `docs/openapi.json` and `docs/llms*.txt` with the repo's generator. Owns migration 017, new `internal/service/join.go`, new store and leaf for join tokens, the API handlers and routes for join, `internal/web/handler/admin/params.go`.

## Wave 2: UI, CLI, installer, fake mode (started 2026-10-12)
Designs: fraedi UI 8 board 0877c0d0 (artboards also at
`/tmp/claude-1000/-home-darthvader-FyrmForge-stackr/10d11dff-c68c-46af-81a5-851d1685d8e0/scratchpad/mn-design/out/*.light.html`)
and the CLI journeys doc b5414190. Build the real screens with the app's
templ components, matching the artboards; terse copy; controller and
worker in all copy; no em dashes. Orchestrator verbs go in new files under
`internal/service/`; lane I7 (final wiring) owns orchestrator.go and main.go
while it runs; report any one-line wiring instead. Route registration files
(`internal/web/server.go`, `internal/api/routes.go`, `cmd/stackr/cmds.go`)
are shared: add your lines, keep them grouped, expect a merge.

- **U1 Nodes admin:** admin Nodes tab, node cards, empty state, node drawer (Overview, Envs, Network, Tags with the controller untag refusal, Security with Rejoin, danger area remove and forget), add-node dialog (3 steps on the join API), forget confirm and lost-envs page, node open to orgs.
- **U2 Cluster map:** the Cluster rail entry and canvas (same canvas as the org/stack/env graphs): network boxes from reported interfaces, public nodes on the box edge, router marker, tunnel links with latency, ingress and exit arrows, S3 backups, stacked env cards three deep with health dots, legend; node click opens U1's drawer.
- **U3 Placement:** Runs on, Ingress and Exits in org, stack and env settings; tile ingress and exits; exit ladder editor with live rung state and the no-internet state; move dialog and move progress view.
- **U4 Security and registry pages:** admin Security log page with filters and row expand; controller warning on the elevated-grant approve screen; org Registry tab.
- **C1 CLI:** every command in the CLI journeys doc: node (add, ls, show, tag, untag, open, close, rejoin, remove, forget, confirm), pin, ingress, exit, env move, env lost/restore, cluster status, admin audit, registry login; --json, exit code 3 for needs --yes.
- **I8 Installer join and fake mode:** `stackr-install join --panel --token --ca-sha256` (installs Docker and WireGuard, makes keys, requests, polls status, writes certs, starts `stackrd worker`); fake mode: `hamr dev` runs two fake workers (`stackrd worker` on fakehost dirs, Recorder WG) that join the local panel automatically.

## Wave 3: after the real-machine proofs (planned 2026-10-12)

Proofs so far: 109 controller with LAN workers 110/111 (scratchpad
`pve2/RESULTS.md`), then Hetzner h1 (89.167.81.29) and h2 (2.29.100.118)
joined to 109 across the home NAT. Nothing below may be dropped.

### W3a Cluster map (agreed, build next)

1. **One LAN for /32 networks.** Hetzner gives each box a /32 on the
   private interface; the real net is a route (10.0.0.0/16 via 10.0.0.1).
   Facts gain each interface's routes (`internal/worker/extras.go`). A /32
   address takes the route prefix that contains it, both for map grouping
   (`netPrefix` in `internal/service/cluster.go`) and for the mesh's
   private-path check (`Candidates` in `internal/mesh/mesh.go`), so h1 and
   h2 tunnel over the private net.
2. **Cards inside the box.** No NAT drawn on the edge; a card with a public
   address gets a "Public IP" row (`internal/ui/cluster/`).
3. **Drag nodes and networks, saved on the server.** Reuse the canvas
   store the map already uses for notes (`ClusterScope`,
   `MoveClusterNote`): node and net ids next to note ids, shared by every
   admin, no schema change.
4. **Port-forward probe (own commit).** Each ingress node's Caddy answers a
   secret path with its node id. A public worker (fallback: the controller,
   marked uncertain since hairpin NAT is unreliable) requests the NAT's
   public IP on that path; the node that answers is the forwarded one. The
   NAT's public IP is the endpoint public workers see for the controller.
   The map's NAT arrow points at it; an ingress node behind a NAT that did
   not answer gets "not reachable from the internet, check your port
   forward".
5. Tests: /32 route grouping, mesh path choice, probe result. Then live on
   109, h1, h2.

### W3b Open items (to plan, one at a time)

- **Cross-node links (agreed):** not built. Keep the link's name; when nodes
  differ the consumer gets an ExtraHosts entry name -> instance VIP (no DNS
  server), skips the managed-network join (deploy facts.go, managed.go), and
  one flow opens that env to that port (vipboot.go). A database move
  recreates its linked apps. Later, maybe: a per-node resolver as Docker's
  `--dns` upstream for a stackr zone, so moves need no recreate.
- **Rule (darthvader):** all node-to-node traffic goes over WireGuard. Caddy
  is for external traffic only. The one exception is the join itself (no
  tunnel exists yet). Audit: the controller's fallback that dials a
  worker's extras API on its public address (d656b72) goes.
- **Node registry pulls (agreed):** the panel gets a listener on its wg
  transit address that only takes node certs; join writes
  `--registry-url` (that address) and `--registry-host` into the worker
  unit; the worker's 127.0.0.1:5000 front pulls through the tunnel.
- **Relays (agreed):** plumbed but `meshSvc.Relays` is never set. The
  controller picks one automatically for every pair where neither node has a
  public address and they share no LAN: host = any node with a public
  address, one port per pair. Last candidate only; the map shows "via <node>".
- **Outside the tunnel:** liveness checks only ("are you alive"), no data,
  no commands. A new worker gets its first mesh spec in the join answer
  instead of a push to its public address.
- **IPv6 endpoints** are tried when the controller has no IPv6; minutes of
  failover before IPv4.
- **Git builds (agreed):** every git build runs on a builder-tagged node
  (most free memory wins), pushes to the registry; the tile's node pulls
  over the tunnel. Moves pull, never rebuild. Cold-move alpine goes through
  the registry too.
- **Live move rsync (done, 5640e1c):** helper image in the registry, pulled
  over wg.
- **Unbound admin key on-box** still needs --org to log in with several orgs.
- **Cluster map rebuild** on the base graph (done, dev.57; darthvader to try).
- **Portability (agreed, beta):** stackr must install anywhere, nothing tied
  to one setup. (1) WireGuard interface `stackr0`, default port 51871, an
  installer flag to change it. (2) Docker: a minimum version, checked; when
  missing, ask before running Docker's install script. (3) Every other host
  dependency (systemd, iptables, ip, kernel WireGuard, ...) checked with a
  minimum version and a clear error. (4) Fixed ports (7443, 7444, 5000,
  relay range, 80/443 on ingress) checked free up front, clear error. (5)
  Fixed ranges (transit 10.207.255.0/24, supernet 10.208.0.0/12): hard error
  at controller setup and join when a box's networks overlap them, listed as
  a system requirement; configurable ranges later.
- **Release images not on ghcr (last, the final test):** join cannot pull
  the worker image.
- **Rsync helper image** is not distributed to nodes (live moves).
- **On-box CLI** `stackr login --with-key` hangs; the box key is org-bound.
- **Ingress on a worker (agreed):** not built. Tagging a node ingress makes
  the controller start the proxy image on it (80/443, own cert volume);
  untag removes it. The worker exposes that Caddy's admin API to the
  controller over wg only; the existing per-node configs
  (`proxyConfigs`, `caddyFor`) are pushed there. Own ACME certs per
  ingress node. Upstream = tile address locally, VIP over the tunnel
  otherwise. The panel domain stays on the controller. Test on h1
  (`*.h1.vulpe.dev`).
- **Worker firewall** carries the controller's LAN addresses (DNS
  192.168.1.101, front proxy 192.168.1.100) on remote boxes: check.

Done in this wave: join's Docker install proven on h2; a mesh peer added
before its addresses were known now gets an endpoint (147ec2c).

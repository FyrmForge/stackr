
# misc
- [ ] `files:` only work on the manager. The per-deploy copy is written to the
      manager's data dir and bind-mounted, so a tile placed on a worker mounts
      a path that is not there. True for single files and folders alike. Fix
      with docker configs, or by pinning tiles with files to the manager
- [ ] the recovery CLI the install script promises cannot act as admin. The
      `/usr/local/bin/stackr` wrapper (scripts/install.sh) execs `/app/stackr`
      inside the panel container, and the image ships it
      (cmd/stackrd/Dockerfile), but it still needs a login: there is no way
      for it to act as admin with no browser and maybe no API key. Options: a
      localhost-only admin key the server writes into the data dir on boot,
      a direct-to-db mode, or drop the promise. Needs its own plan
- [ ] the join script needs an interactive terminal for sudo, so it cannot be
      run over a plain `ssh host '...'`. Fine for a person at a keyboard, but
      it means the script is only ever half exercised in QA, and a headless
      installer has no way in
- [ ] a managed s3 tile cannot be a backup destination, which is the obvious
      self-hosting move. The panel is a swarm service on `stkr` only and
      reaches an instance by attaching its own container for the length of one
      call, which a destination cannot rely on ("Not a
      bug")
- [ ] per-logical-db backups. A dump of a shared instance covers the
      instance's own database only; the panel now says so, but the ten slices
      on the rig have no backup of their own short of a volume archive
      (phase 4 of docs/features/shared-infra.md)
- [ ] a postgres restore drops and recreates the `public` schema only. A dump
      that creates its own schemas restores into whatever of them is already
      there (a known ceiling)
- [ ] a storage attachment does not pin its consumer. A local pool is a
      directory on one host, so `storagetiles.ensureForConsumer` refuses an
      attachment whose consumer resolves to another node, but a stateless tile
      resolves to no node at all and swarm may schedule it anywhere. On a
      single-node install that is fine. On a multi-node one the right answer is
      for a local-backed attachment to add a placement constraint the way
      `node_group` does, which is a placement change, not a storage one
- [ ] the API creates every storage on `ServerID: "local"` as a literal
      (`handlers/api/v1/storage.go`). The web page now writes the server whose
      page it is, so the two disagree; giving the API route a server is an API
      change
- [ ] a tile whose own scoped name is over 63 characters cannot deploy, with
      only the daemon's "name must be 63 characters or fewer" to show for it.
      Run names are capped now, tile names are not, because a tile's service
      name is its identity for life and shortening it would orphan the running
      service
- [ ] a build node runs one buildkit daemon shared by every org, so the
      per-org cache split only holds on the manager. One service per org on
      its own port when strangers share a build node (docs/plans/37-builds.md)
- [ ] a deploy has no per-step timeout, only the 30-minute one on the whole
      run, so any step that hangs looks identical to a step that is slow, and
      Cancel cannot tell them apart either. Worth doing
      on its own
- [ ] `managedtiles.Deploy` runs `PullImage` and `LocalDigest` on the manager
      whatever node the instance deploys to. The pull is only wasted, swarm
      pulls on the target itself, but `LocalDigest` baselines the registry
      watcher from the manager's copy, which can be a different digest from
      what the worker runs. Node calls; one-argument change once plan 35
      lands (docs/plans/35-cluster.md, "Not doing")
- [ ] remove the mariadb, mongo and redis managed engines, and re-add them
      only when there is capacity to test them properly. Step 0 of
      docs/plans/35-cluster.md. They are half-built,
      not deprecated: nothing has ever exercised them on the rig in ten QA
      rounds. Removal is three entries out of the `Engines` map in
      `infra/managedtiles/managedtiles.go`, deleting `engine_mariadb.go`, and
      four tests that name them (`restorecmd_test.go`, two `fork_test.go`,
      `config/varref/varref_test.go`). Everything else reads the map by key
      and already handles a missing entry, and all five call sites that reach
      a func field off an entry guard for absent or nil first. The unknown
      engine error at `config/stackconf/stackconf.go:809` should say "not
      supported", not "no longer supported". What each one needs on the way
      back in:
  - mariadb: the provisioner is already written and complete, provision,
        drop, fork, dump, restore. It has simply never been run. Its dump
        relies on `mariadb-dump` writing `DROP TABLE IF EXISTS`, which is what
        keeps its restore from merging
  - mongo: image and dump/restore only, no provisioner, so no slices
  - redis: image and an RDB-copy dump only, no provisioner, and restore was
        never wired. Also needs a licence decision: `redis:7` resolves to
        7.4.x, which is RSALv2 + SSPLv1 and not open source. Valkey is the
        BSD-3 fork and the likely replacement, but check whether its image
        ships a `redis-server` symlink, since the engine's Cmd calls that name
- [ ] **the panel attaches its own container to instance shared overlays and
      never detaches, and the attachment can take the instance service's own
      VIP address, which kills the overlay load balancer for that network.
      Every consumer loses the instance, silently, while the panel and docker
      both call it healthy.** Seen on the rig with the s3 instance
      (not fixed). Hand fix:
      `docker network disconnect -f <net> <panel container>`. Cheap code fix
      is a refcounted attach released on return, which three of the four
      `reachInstance` callers fit; `s3ClientFor` hands its client to the file
      browser and outlives the call. Real fix is to provision through the
      instance node's agent so the panel is never on those overlays
- [ ] `managedtiles.Start` scales an existing service back up without
      reconciling its placement, so a stopped instance restarts under whatever
      spec it already has. Not reachable today, a move now ends in Deploy
      (not run, and left alone)
- [ ] a managed instance the reconciler marked `stopped` cannot be started by
      a config apply: updateTile leaves a stopped instance alone on purpose.
      The way back is the Deploy button on its drawer. Related to the parked
      question of whether the file owns "should this be running"
- [ ] Show log severity with row backgrounds instead of red text.
- [ ] Allow config-managed orgs to retain UI-created unmanaged stacks through
      an explicit setting such as `allow_unmanaged_stacks`.
- [ ] Add a narrowly scoped tile SDK for triggering jobs and reading permitted
      stack or org state without exposing the whole control plane.
- [ ] Consolidate mismatched drawer, settings, metrics and log-viewer patterns.
- [ ] (parked 2026-09-09, during the multi-node UI board) refine and redesign
      the tile drawer's Settings tab. One long form split by a pill jump row
      (`sectionJump` in `handlers/web/handler/app/app.templ`, sections Source,
      Build, Runtime, Networking, Resources, Health, Startup) with Domains,
      Scheduled jobs, Managed databases and Storage stacked under it. Plan 30
      step 8 adds a Placement section (home node, replicas, node group) into
      the same form, so the mess grows before it is fixed. Start from a board
      of the current tab against a proposal, not from code
- [ ] think about functions — tiles (`type: function`, `run_on_deploy`, manual
      runs) shipped. The remaining triggers (db table update, sqlite events)
      are the same open question as plan 20; tracked under `# function tiles`.
- [ ] explore implications (resource usage) of adding support for pg for the main stackr db, that would also allow us to provision dbs from it
    - today sqlite is hard-wired: `cmd/stackrd/main.go` opens by file path, the whole repo layer is `store/repo/sqlite`
- [ ] (follow up after plan 30 step 8) how nodes map onto orgs, stacks and
      envs. Agreed for now: flat pool by default, plus opt-in node groups, a
      tag on the node that an org, stack or env can require, inherited down
      and overridable, turned into a Swarm placement constraint on a node
      label. Pinned tiles must sit inside their group, moving out is refused
    - open: hard tenancy for enterprise (an org owns nodes and nothing else
      lands there), quotas per group, whether a group can span providers,
      and what the panel shows when a group has no free node
    - (2026-09-09) later: free-form docker node labels beyond the one group
      tag, `disk=ssd`, `region=home`, `gpu=1`. Needs a labels editor on the
      server page (`docker node update --label-add`) and a constraint picker
      in the tile's Placement section. Parked on purpose: the group tag is
      enough for the first multi-node release, and every extra label is one
      more way a pinned tile ends up with nowhere to go. Display names for
      servers are a separate, cheap thing and are in the multi-node UI board
- [ ] (later, after plan 30) panel failover. Not on the plan, but the path
      is decided: the node agent (`stackrd agent`, a global service on every
      node, decided 2026-09-09) is the surface. Same binary everywhere, so
      any node can be promoted to run the panel; Swarm's own manager
      failover covers the cluster side once there are three managers
    - the blocker is state, not process: the panel is one SQLite file on
      the manager's disk and a promoted agent has none. Needs the db
      replicated ahead of time, Litestream shipping the WAL to shared
      storage, LiteFS, or the pg move above. Pick that first, the promotion
      logic is small once the data is there
    - plan 30's failover section has the three-problem split (stateless
      tiles, stateful tiles, the panel); this item is the panel one
- [ ] Identify reusable graph.js patterns and standardise them.
    - [ ] next rung: fold pan/marquee/node-drag into `track()` — they
          interleave with the two-finger pinch state, riskiest third
    - [ ] then re-decide the framework question; cross-file duplication with
          main.js/logs.js is ~30 lines today, not worth a shared module yet
    - [ ] we need to assess how the actual js is written, then see what we can fix, then start extracting some of it into other files, i doubt that the entirety is 1.6k is needed for every graph
- [ ] sqlite browser for sqlite files found in volumes browsers and s3 browsers
    - nothing today: `filebrowse` is browse/download/upload/delete only; the
      sql browser (`infra/managedtiles/sqlbrowse.go`) is psql-in-container,
      server engines only — nothing opens a file
- [ ] make sure host network (published ports) show network activity
    - the topology node + `port` edges ship; traffic doesn't:
      `sampleFlows` (`internal/stackrd/infra/metrics/metrics.go`) drops flows
      whose ends aren't known container IPs, and host-mode containers get no
      IPs recorded at all (`infra/runtime/runtime.go`)
- [ ] Add Vim-style keyboard navigation.
- [ ] need to look into creating custom VPCs inside a stack and bridging connections
    - for least priv access
    - or maybe look at bridging stacks
    - maybe just look at being able to define extra docker networks within stacks, then ill need to figure out bridging
    - already there: per-env bridge nets (`infra/envnet`) and cross-stack nets
      for shared managed instances (`infra/managedtiles/scope.go`); the missing
      piece is operator-defined `networks:` in stackconf
- [ ] investigate more feature parity with actual docker-compose
    - target enough Compose parity to host a complete homelab
    - (decided 2026-09-09, plan 30) compose tiles go away with the Swarm
      move. `docker stack deploy` silently drops `build`, `depends_on`,
      `container_name`, `restart`, `privileged`, `cap_add` and host
      networking, so a compose file under Swarm is a different file anyway.
      Source type `compose`, `ComposeUp` / `ComposeDown` in the runtime
      package and `ComposeProject` naming all get deleted; 23 files in
      `internal/stackrd` mention compose today
    - the gap gets filled later by a `stackr` CLI command that converts a
      `docker-compose.yml` into stackr config: services to tiles, volumes to
      volume tiles, `depends_on` as is, and a printed list of what did not
      map. One-shot conversion, not a runtime
- [ ] just make the ability to make shared and manged tiles cross orgs as well
    - scoping tops out at one org (`managedtiles/scope.go` `Eligible`);
      env/stack/org live, `server` not implemented
- [ ] look into sql proxy which might be useful for provisioned tiles — keep
      for later as a security measure. Wire-protocol counterpart of the shipped
      psql data browser (`infra/managedtiles/sqlbrowse.go`)
    - dev access already covered by `stackr forward`
      (`docs/features/port-forward.md`); this item is a security/inspection
      layer on top, not the access path
- [ ] (after plan 30) traefik 80/443 through the Swarm routing mesh instead
      of host mode. Mesh SNATs, so traefik sees the ingress gateway IP and not
      the visitor: access logs, rate limits and forwarded headers all break.
      Swarm ingress has no PROXY protocol. Look for a fix (iptables skip of
      the SNAT, a host-mode front proxy, or a cloud LB header) before ever
      leaving host mode
- [ ] (after plan 30) keep secret values out of `docker service inspect` and
      the swarm raft store with Swarm secrets plus a shim
    - today every secret is a plain env var in the service spec: visible in
      inspect, in raft on every manager, and in `/proc/<pid>/environ`
    - shape: stackrd creates one Swarm secret per value, named by tile, var
      and a hash of the value; the spec references names only; a tiny
      entrypoint of ours reads `/run/secrets/*` into env and execs the image's
      real command, so the app still sees env vars. `/proc` still has them,
      that is the floor
    - immutability handled by naming: a changed value is a new secret, the
      service updates to it, the old one is removed once unreferenced. One
      restart per change, same as now
    - per-tile switch, default on, off for images whose entrypoint the shim
      cannot wrap. The plan says "not a reason to do Swarm"; this is the
      side effect the plan allowed for
- [ ] ssl on DBs
    - [ ] and on any other managed tiles that can equivalent connections
    - nothing today: connection strings are built plaintext across
      `infra/managedtiles`
- [ ] (decided 2026-09-09, with plan 30) the managed registry stops being
      opt-in: always on, every build pushes, tasks pull from it, the per-tile
      `push_to_registry` flag goes and a failed push fails the deploy
    - today `registry.EnsureManaged` (`infra/registry`) only runs from the
      Settings button and the engine pushes only when the flag is set
      (`infra/deploy/engine.go`, `pushToRegistry`); the 2026-07-26 handoff
      left "auto-enable?" open and nobody answered
    - TLS from the start: a traefik domain and cert like any tile, so workers
      pull without an insecure-registry entry. The no-TLS install mode
      (localhost, tailnet names) is the one case that still needs the entry
    - later: external push from CI (GitHub Actions) into it, a per-org or
      per-stack push credential on top of the single htpasswd user
- [ ] image watch, later rung: ghcr `registry_package` webhook for instant
      updates instead of polling (`packages:read` already granted; existing
      connectors need the event enabled manually)
- [ ] private panel: reach the admin UI and the API only over a mesh net
      (tailscale or netbird), never the public internet
    - today the panel publishes on `127.0.0.1:8080` (`scripts/install.sh`) and
      is reachable only because `proxy.routablePanel` writes it a Traefik route
      on the same public `:80/:443` entrypoint the tiles use
    - `install.sh` already asks "is $host reachable from the public internet?"
      and drops TLS for tailnet names, so the private case is half-anticipated
    - rungs, cheapest first:
        - publish on the mesh IP (`100.x.y.z:8080:8080`) and skip the panel
          Traefik route entirely; the host runs the mesh daemon, near-zero code
        - a second Traefik entrypoint bound to the mesh IP: panel route goes
          there, tile routes stay public, TLS from tailscale certs
        - embed `tsnet` so stackrd joins the tailnet as its own node, no host
          daemon; tailscale only, netbird has no clean Go embed
    - want it as a first-class connector like the git ones (`connectors` table,
      org-scoped, provider + config json) holding the auth key or API token
    - org-scoped is wanted: separate real organisations can share one server,
      each reaching only its own panel and API. that forces rung 3
        - rung 1 gives one mesh IP for the whole box, cannot split by org
        - rung 2 needs a mesh IP per org, so the host joins N tailnets anyway
        - rung 3 does it cleanly: one `tsnet.Server` per org connector, each
          with that org's auth key, each its own node in that org's tailnet.
          netbird drops out here, no embed
    - a per-org listener is not per-org authz. if every listener serves the
      same mux, someone on org A's tailnet with valid creds still reaches org
      B's URLs. the listener has to pin org context and refuse the rest;
      `handlers/middleware/orgctx.go` resolves org from the request today but
      has no notion of the listener as a source
    - (2026-09-09, plan 30) node to node traffic is the same problem. Swarm's
      control plane is mTLS, but the overlay data plane is plain VXLAN, and
      the per-node daemon port (2376, mTLS) and registry pulls ride the same
      wire. Supported shapes for now: nodes on one private network (provider
      private net, home LAN), or the user brings a tunnel and passes its IP
      to the join script so Swarm, the daemon port and the registry all bind
      to it. Refuse a public IP on the add-node screen
    - direction when this gets built: stackr runs its own WireGuard mesh
      between nodes, not a dependency on tailscale or netbird. Key exchange
      through the panel at join time, one `wg` interface per node, Swarm
      advertises on it. Tailscale or netbird then plug into that mesh as an
      optional way in for people, not as the thing the cluster stands on.
      Rung 3 above (`tsnet` per org) becomes a door onto our mesh
    - first shape is hub and spoke, not a full mesh: the manager is on the
      public internet, a worker can sit behind NAT on a private net and dials
      out to the manager with a persistent keepalive. Worker to worker
      traffic is forwarded through the manager (ip_forward on, AllowedIPs
      covers the whole mesh subnet). NAT traversal between workers is a later
      problem, and so is the manager being the bandwidth chokepoint
    - research, not decided: the panel as WireGuard coordinator. WireGuard
      itself never discovers peers, it sends to the endpoint it was given.
      The panel knows every node's addresses from join, so it can hand two
      workers on the same private net each other as direct peers, and leave
      workers on different nets routing via the manager. Peer list updates
      apply live through `wgctrl`, no interface restart. That is tailscale's
      coordinator minus hole punching. Open: how to tell "same private net"
      (same subnet is a guess, a reachability probe is the real answer),
      and whether a worker that later gains a public IP should be promoted
      to a direct peer automatically

# graph

- [ ] Explore more graphical primitives such as shapes and arrows.
- [ ] Add clearer indicators for connections and builds.
    - shipped already: status dot + label footer, cron/function outcome
      footer, six styled edge kinds, busy-edge pulses, traffic lanes
    - actually missing: connect-health (does the tile's db connection work)
      and build progress beyond the single amber "building" dot
# panel state

- [ ] BUG: stack card sits on "building" while every tile inside reads green
    - card status is `WorstStatus` in `handlers/web/graph/graph.go` rolling up
      tile rows; unconfirmed whether a tile row is left at `status='building'`
      or the card just never re-renders. Catch it live with
      `select slug,status from tiles` before picking a fix.
# variables ui

- [ ] add a button that shows what variables resolve to, references expanded
      and secrets included
    - the resolver already exists: `varref.EnvLines` is what the deploy engine
      and the cron runner call; the button is that plus a read-only render
    - note the panel currently promises secrets are never shown, so this is a
      deliberate change of that stance, not just a view
# ids & docker naming

- [ ] move off full uuids to short ids for anything referable
    - nothing parses ids as uuids (zero `uuid.Parse` calls) and every id column
      is `TEXT PRIMARY KEY`, so this is a generator change, not a migration —
      old and new rows coexist
    - the codebase already fakes it: `[:8]` truncation in container names,
      image tags, `ComposeProject`, deployment rows and the panel
    - `ComposeProject` is the one truncation that can actually collide (flat
      docker namespace across every compose tile) — size the id for that,
      ~12 base32 chars
    - security tokens keep full entropy: sessions, api keys, share links,
      invite tokens, the PR webhook secret
# promotion

- [ ] a slice rename leaves the old logical db behind on the instance and
      its card on the canvas (`cart_stg`, `cart_stg_2`, `cart_staging` on
      the test box). Nothing deletes them.
    - the plan warns "moving a slice re-provisions it empty; the old data
      stays behind" (`config/stackconf/plan.go`), and apply does exactly
      that: new `managed_resources` row, old one untouched
    - fix: a rename keeps the row and renames the logical db, or the old
      row gets a delete on its card
    - the file order (`EnvOrder`, `config/stackconf/serialize.go`) decides
      who builds on push and who is a rung above (`apply.go`, `a.upper`)
    - the store order is `created_at` (`ListEnvironmentsByStack`,
      `store/repo/sqlite/stacks.go`) and drives the colours, the compare
      reference, the commit log chips and `allAuto`'s default slug
    - fix: `environments.position` written by apply from `EnvOrder`, list by
      position then created_at; swap the two blocks in the demo repo's
      `stackr-compose.yml` so staging comes first
# org defaults

- [ ] the `defaults:` block carries two settings; it wants the promotion ones
    - the block itself shipped: `orgconf.Defaults` (`config/orgconf/orgconf.go`)
      holds `ui_edits` and `env_colors`, landed on the org row by apply so a
      stack falls back to it. The fallback path exists, this is not greenfield.
    - missing is what the item was originally for: an org saying "we use
      promotion with config pinned". Nothing promotion-shaped is a default yet,
      and plan 27 (auto rollback) adds another per-org/stack/env setting that
      belongs here.
    - unchanged below it: the stack -> env half is `base:` in
      stackr-compose.yml, merged by `mergeEnvConf`
      (`config/stackconf/stackconf.go`).

# local dev

- [ ] `stackr dev`: bring a stack up on the developer's own machine from the
      local config file
    - context: every command in `internal/cli/cmd/` is a pure API client today.
      Nothing in the CLI touches docker or parses config, even
      `stackr plan preview` uploads the file and lets the server diff it. This
      would be the first command doing real work locally.
    - decided: a cut-down local engine in the CLI, not a local stackrd and not a
      generated docker-compose file
    - most of it is reuse, all pure Go with no server in it: `config/stackconf`
      to parse and resolve one env, `config/stackconf/deps.go` for ordering,
      `varref` for `${{ }}` references, `config/runpolicy` for what each kind
      does. What gets written fresh is build, network, start in dep order.
    - decided: builds come from the working tree, not a clone. `build.context`
      is already a relative path (`BuildConf`), so a local run is a docker build
      against that path relative to the config file.
    - deliberate divergence from prod, to document rather than discover:
      no traefik routing (publish ports instead), no health gating, no backups,
      no plans, no promotion. Secrets need a local source, probably a dotenv.
    - open: tiles carrying their own `git_url` have no working tree locally.
      Skip them in v1, clone into a cache dir when someone asks, explicit
      local path mapping last.
- [ ] multi-repo stacks: today the model is one git repo per stack, but a tile
      can already carry its own `git_url` and `branch`
      (`diffSource`, `config/stackconf/plan.go`). Worth working through what
      multi-repo does to the rest of the feature set.
    - it refines the promotion model rather than breaking it. An env is not "one
      commit": it is one config commit (the bound repo, already pinned via
      `cp.CommitSHA`) plus a set of image tags, one per built tile, each from
      whatever repo produced it. Promotion carries both, identically for one repo
      or five.
    - what does change: "prod is 3 commits behind" stops being a single number
      and becomes per-tile. Affects the plan UI and whatever shows how far behind
      a pinned env is.
    - also hits `stackr dev`: which working trees exist on the machine.
- [ ] fake GitHub for local dev: exercise plans, PR envs, ci gate and
      promotion on a laptop without a real connector
    - a connector is a GitHub App installation, not a repo. `githubapp.Client`
      (`infra/githubapp/`) has 14 methods to fake: connect flow, `ListRepos` /
      `ListBranches`, file reads, `CloneAuth`, `CIState`, `DeployFinished`,
      `RefreshPRComment`, `RegistryAuth`. Plus inbound push and PR webhooks.
    - the read seam already exists: `FileSource` in `config/stackconf/runner.go`,
      faked in-process by `journey_config_test.go` today
    - decided: git faked, docker real. HTTPS needs nothing: `proxy.New` already
      takes `noTLS`, domains resolve via `*.localhost`
    - lean: repos are real local git repos under `data/fakegit/`, so commits,
      shas and clones are genuine; the fake only invents the API side (tokens,
      repo list, CI state, PR events)
    - dev-facing shape: `STACKR_FAKE_GITHUB=1`, wizard connects instantly, edit
      + `git commit` in the fake repo, then a dev-only push / open PR / CI
      passed button fires the webhook
    - out of scope for now: fake traffic (`infra/metrics` samples /proc, not
      docker), fake cert state
    - chunky. Park until the small items are cleared.

# git providers

- [ ] connector setup: after the GitHub App is created but not yet installed,
      the "Manage installation" button is too dim to read as a required step
    - say it outright: setup is not finished, go approve / install the app,
      then come back. A half-connected connector today looks done.
- [ ] more providers: GitLab, Gitea/Forgejo, Bitbucket, and plain git remotes
    - `Connector.Provider` (`store/repo/models.go`) already exists as a field but
      `githubapp` is the only implementation; the 14 methods on its Client are
      the de facto provider interface, never declared as one
    - raw git is half there for tile source: tiles take `git_url` + `ssh_key`
      and the deploy engine clones with the stored key
      (`infra/deploy/engine.go`). Missing: raw git as a *config* source
      (`FileSource` in `config/stackconf/runner.go` is githubapp only), and
      a connector row for it
    - raw git loses what an API gives: no repo listing, no branch picker, no CI
      state, no commit status, no PR events. Config reads become a shallow
      clone or `git archive`, "webhook" would need a generic
      deploy hook again (the per-tile one was removed in plan 23)
    - auth for raw git: ssh key (exists) or https token, stored on the
      connector, not per tile
    - same seam as the fake GitHub work under `# local dev`: extracting the
      provider interface serves both

# function tiles

- [ ] (parked 2026-09-09, revisit after the Swarm migration, plan 30) rethink
      the run primitives as one concept: three tile kinds (`service`, `cron`,
      `function`, the comment on `repo.Tile.Kind` in `store/repo/models.go`
      still says two) plus `scheduled_jobs` rows hanging off a service tile in
      exec / image / custom mode. Two schedulers' worth of shape for what is
      "run this, on that image, on this trigger". Under Swarm every one-shot
      becomes a `replicated-job` service, which is the moment to collapse them
- [ ] finalise [plan 20](plans/20-event-triggered-functions.md): functions
      triggered by pg `NOTIFY` or S3 object events, and the two triggers that
      used to sit under `# misc` (a db table update, sqlite events)
    - discussion only, nothing agreed. The open questions at the end of the
      plan are the blockers, starting with whether a function is a new tile
      kind or a second trigger type on the existing cron job
    - the network question is the interesting half: stackrd joins stack
      networks today via `reachInstance` and never leaves, which makes the
      control plane reachable from tenant containers on 8080

# audit log
- [ ] Any org member with stack access can promote to prod, and
  nothing records who did. Promote, rollback, plan approve and reject want
  an audit trail. The secrets audit table (`repo.AuditEvent`: actor,
  action, owner kind and id, name, when) already has the shape; reuse it
  with actions promote | rollback | approve | reject, owner env, name the
  commit, and an Activity section on the Releases page (plan 26). Decide
  with plan 26 or right after it.

# node agent
- [ ] Upgrade path. Panel and agent never run different versions on
  purpose. A panel upgrade rolls the agent global service to the same image
  first, then restarts itself. The version handshake (plan 30 step 7) refuses
  a mismatch only as the fallback for nodes that were offline during the roll.
- [ ] `scripts/install.sh` grows into a small interactive guide, a
  form-style TUI walking the operator through hostname, TLS, ports and the
  data dir. Today it is a chain of `ask` / `yesno` prompts, which is enough
  until then. (The encryption switch this used to name went away with the
  feature on 2026-09-11; see plan 31, overlay encryption.)

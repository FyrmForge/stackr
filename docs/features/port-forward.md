# Feature: Port Forward (`stackr forward`)

## Summary

`stackr forward <tile>` opens a local TCP listener and tunnels every
connection to a container port on the tile's environment network —
kubectl-port-forward for tiles. Nothing is published to the internet: no DNS,
no TLS, no Traefik route. Auth is the existing API key with the
`tiles:forward` scope (a write scope — a tunnel is unrestricted access to the
container).

```
$ stackr forward main-postgres --port 15432
Forwarding 127.0.0.1:15432 -> Main Postgres:5432   (Ctrl-C to stop)

$ psql postgres://user:pass@127.0.0.1:15432/mydb
```

Port syntax is kubectl's `[local:]remote`; with no `--port` the tile's own
port is used on both sides (database engines resolve to their engine default).
Listens on `127.0.0.1` unless `--address` says otherwise. A redeploy of the
tile does not break the tunnel — the relay dials by DNS alias and re-resolves.

## The pieces

```mermaid
flowchart LR
    subgraph laptop["developer laptop"]
        psql["psql / curl / anything"]
        cli["stackr CLI<br/>local listener :15432"]
    end
    subgraph host["docker host"]
        subgraph snet["stackr network"]
            panel["stackr panel<br/>(auth, audit, presence)"]
            relay["proxyrelay container<br/>~2 MB, FROM scratch"]
        end
        subgraph envnet["environment network"]
            relay2["proxyrelay<br/>(same container)"]
            tile["tile container<br/>postgres :5432"]
        end
    end
    psql -->|TCP| cli
    cli -->|"websocket per connection<br/>(x-api-key)"| panel
    panel -->|"TCP to relay IP :15000"| relay
    relay -.->|same container,<br/>two networks| relay2
    relay2 -->|"dials tile-&lt;id8&gt;:5432<br/>(DNS alias)"| tile
```

Three processes touch the bytes:

- **The CLI** accepts local TCP connections and opens one websocket per
  connection to `GET /api/v1/tiles/:id/forward` (`internal/cli/client.go`
  and the forward command in `internal/cli`).
- **The panel** authenticates, resolves the port, writes the audit log line,
  and bridges the websocket to a TCP connection
  (`internal/stackrd/handlers/api/v1/forward.go`,
  `internal/stackrd/handlers/web/wsproxy/wsproxy.go`).
- **The proxyrelay** (`internal/proxyrelay`, `cmd/proxyrelay`,
  `cmd/proxyrelay/Dockerfile`) is a disposable container that pipes TCP between
  the `stackr` network and the tile's environment network.

## Why the relay exists

Environment networks are isolated bridges, and the panel is deliberately
**not** on them — the process holding the docker socket should not be one TCP
connection away from tenant code. But then it cannot reach a tile's
`172.x` address at all.

The relay is a container that is legitimately *given* a foot in both worlds:
created on the `stackr` network, then connected to the target's environment
network. It is a ~40-line accept-loop (`listen :15000` → dial the tile's DNS
alias → `io.Copy` both ways) built as a **separate 2 MB binary in its own
`FROM scratch` image** (`stackr-proxyrelay:local`, built by `make proxyrelay`
and the deploy script) — a distinct `main` makes it structurally impossible
for a relay container to boot panel code.

One relay per **(tile, port)**, named `stackr-proxyrelay-<tile8>-<port>`,
labelled `stackr.proxyrelay=<tileID>`. Concurrent forwards share it (creation
is serialized in `internal/stackrd/infra/runtime/runtime.go`); it dials the tile by alias so
a redeploy heals itself; and it reaps itself: `--rm` plus a 60-second idle
timeout. If the panel crashes, every tunnel's websocket dies with it, every
relay goes idle, and the fleet is gone inside a minute with no bookkeeping.

## Life of a forward

```mermaid
sequenceDiagram
    participant U as psql
    participant C as stackr CLI
    participant P as panel
    participant R as proxyrelay
    participant T as tile

    Note over C,P: startup
    C->>P: ws /tiles/:id/forward?presence=1
    P->>R: ensure relay (create if missing), dial + close
    P-->>C: 101 + X-Stackr-Port (or an HTTP error: bad scope, tile stopped…)
    Note over C,P: presence ws stays open for the CLI's lifetime

    Note over U,T: each connection
    U->>C: TCP connect :15432
    C->>P: ws /tiles/:id/forward
    P->>R: TCP dial relay-ip:15000
    R->>T: TCP dial tile-<id8>:5432
    P-->>C: 101 upgrade
    loop until either side closes
        U->>T: bytes (TCP → ws → TCP → TCP)
        T->>U: bytes (…and back)
    end

    Note over R: 60s after the last<br/>connection closes
    R->>R: idle exit, --rm removes it
```

Failures happen **before** the upgrade, so the CLI prints a real HTTP status
("403 missing scope: tiles:forward", "409 tile is not running", "502 cannot
reach…") instead of a dead socket. The `X-Stackr-Port` response header carries
the port the server resolved, so `stackr forward db` with no `--port` prints
an honest line.

## Presence — who has a forward open

The first websocket (`?presence=1`) carries no data. It exists so the server
knows *who has a forward open*, for the canvas:

- The env canvas draws an **ephemeral card per (tile, port)** — one circular
  avatar badge per user with a forward open; hovering a badge shows headshot,
  full name, and `role · :port`. The card is the union of two sources: the
  relay container (docker, survives a panel restart) and the in-memory
  presence registry (`internal/stackrd/infra/forward`). A relay whose users are gone shows
  "idling — closes soon" for its last 60 seconds.
- Stack and org canvases roll it up into a **count chip** on the env / stack
  card, the same way `DomainCount` stands in for hostnames.
- Cards appear and disappear **without a page reload**: they are marked
  `ephemeral` in the graph payload and diffed client-side by `graph.js`,
  outside the node-count check that reloads the canvas on structural change.

If the panel restarts, presence websockets die; the CLI re-registers with a
3-second retry loop, so rows survive deploys. Data tunnels are unaffected
either way — they are opened per connection.

Per-tunnel audit stays in the server log: one `port-forward opened/closed`
line per connection and one `session opened/closed` pair per CLI, with user,
tile, port, and duration.

## How Kubernetes does the same thing

`kubectl port-forward` has the same user-facing shape and a different floor
underneath — because every k8s node runs a privileged agent and a Docker host
doesn't.

```mermaid
flowchart LR
    subgraph k8s["kubernetes"]
        kubectl --> apiserver["API server"]
        apiserver --> kubelet["kubelet (node, root)"]
        kubelet -->|"CRI PortForward"| runtime["container runtime"]
        runtime -->|"setns into the pod's netns,<br/>dial localhost:port"| pod
    end
    subgraph stackr["stackr"]
        cli2["stackr CLI"] --> panel2["panel"]
        panel2 --> relay3["proxyrelay container"]
        relay3 -->|"dial tile alias over<br/>the env bridge"| tile2["tile"]
    end
```

The runtime daemon holds a file descriptor to the pod's network namespace, so
it steps in with the `setns` syscall and dials `localhost:<port>` *from
inside* — no binary injected, no sidecar, the pod can't even tell. Docker's
daemon holds the same namespaces and simply exposes no API for it. So k8s's
"relay" is a permanent root process on the host; ours is an unprivileged
throwaway container. Same topology, different privilege level:

| | k8s (runtime + setns) | stackr (proxyrelay) |
|---|---|---|
| privilege | root on every node, always | none |
| reaches `127.0.0.1` inside the target | yes (it is inside the netns) | no — enters via eth0, so a localhost-only process is unreachable |
| lifecycle | permanent daemon | exists only while forwarded, self-reaps |
| observable | not meaningfully | `docker ps`, and it *is* the canvas card |

Transport history rhymes too: kubectl streams ran on SPDY for a decade
(websockets couldn't multiplex stdin/stdout/stderr in 2015) and are migrating
to plain websockets (KEP-4006) now that SPDY-hostile middleboxes and an
unmaintained library forced the issue. We started where they are heading —
with the simplification that a forward is a single raw byte stream, so we
need one websocket per connection and no channel framing at all.

One transport subtlety worth remembering: the hamr server wraps every request
context in a 30-second timeout, which silently killed any tunnel older than
that. `wsproxy` and the presence handler detach with `context.WithoutCancel`;
a tunnel's lifetime is decided by its two ends, not by the request that
opened it.

## Boundaries

- Any tile with a running container is forwardable, not just databases —
  same trade kubectl makes (bypasses Traefik middleware; the scope is the
  only gate).
- Compose tiles forward to the first labelled container, same as logs.
- The relay listens on all its interfaces; containers in the same environment
  can reach it, but its only target is a port they already share a bridge
  with — no added reachability.
- UDP, multiple `--port` flags, and a container selector are out of scope
  until someone needs them.

## Files

| Piece | Where |
|---|---|
| CLI command + reconnect | `internal/cli` (`cmd/stackr/main.go` is a shim) |
| client websockets | `internal/cli/client.go` |
| API handler (tunnel + presence) | `internal/stackrd/handlers/api/v1/forward.go` |
| ws↔TCP bridge | `internal/stackrd/handlers/web/wsproxy/wsproxy.go` |
| relay ensure/dial/sweep | `internal/stackrd/infra/runtime/runtime.go` |
| relay process | `internal/proxyrelay/`, `cmd/proxyrelay/` (incl. its Dockerfile) |
| presence registry | `internal/stackrd/infra/forward/registry.go` |
| canvas cards + chips | `internal/stackrd/handlers/web/graph/graph.go`, `internal/stackrd/handlers/web/components/canvas/canvas.templ`, `frontend/static/js/graph.js` |

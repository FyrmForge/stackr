# External routes: pass-through and routes to other machines

Status: draft 2026-10-06, under discussion.

## Problem
The serverconfig Traefik carries six hosts that are not containers on the
box: raw TLS pass-through (`*.fyrmforge.dev`, `*.dokploy.vulpe.dev`,
`*.stackr-test.vulpe.dev`) and plain reverse proxies to LAN hosts (Proxmox
over self-signed HTTPS, Pi-hole, TrueNAS and its S3 ports). stackr's proxy
can only route to tiles; a domain row needs a tile. Without this stackr
cannot be the front door, so Traefik would have to stay in front of it.

## Decisions
- Pass-through uses the layer4 module (`github.com/mholt/caddy-l4`,
  v0.1.2, pins the Caddy version stackr already uses; proven locally on
  2026-10-06: compiles, raw SNI pass-through keeps the upstream's own cert,
  unmatched hosts fall through to normal HTTPS, HTTP/2 intact, silent
  clients cut after the 3 s matching timeout).
- Wrapper wiring: the layer4 wrapper sits on the HTTPS server's listener,
  before the `tls` wrapper. No internal port, no PROXY protocol hop. The
  install spec keeps publishing 443 as today.
- One resource, "route", covers both gaps: a host that goes to an address
  outside stackr. Modes:
  - `passthrough`: raw TLS to `target:443` by SNI; `:80` for that host is
    reverse-proxied to `target:80` so the backend runs its own ACME.
    stackr issues no certificate.
  - `http`: stackr terminates TLS and reverse-proxies to `http://target`.
  - `https`: same, upstream over TLS; `insecure: true` skips verification
    (Proxmox). Both get force-HTTPS on `:80` like a tile domain.
- Wildcard hosts: `passthrough` takes `*.x` with no DNS-01 (no cert is
  ours). `http`/`https` keep the existing rule (wildcard needs DNS-01).
- Owner: the server, admin only (darthvader 2026-10-07; an org never points
  the proxy at the LAN). Routes live in the admin drawer, the admin API and
  the CLI (`stackr admin route ls|add|rm`); not in any org file. The squat
  check that guards tile domains guards route hosts too, both ways.
- Plugin set is ours: layer4 compiles into `stackrd proxy`. No user-added
  modules (a bring-your-own-image install flag stays an option later).

## Schema
New table `routes`: id, host UNIQUE, mode
`passthrough|http|https`, target (host[:port]), insecure, created_at.
`host` is unique with `domains.host` and `domain_resources.host` checked in
code (DECIDE: one host, one owner).

## Caddy
`domain.Install` gains nothing; `Build` takes `[]Route` beside `[]TileRoute`:
- passthrough → `listener_wrappers: [{wrapper: layer4, routes: [{match:
  [{tls: {sni: [host]}}], handle: [{handler: proxy, upstreams: [{dial:
  [target:443]}]}]}]}, {wrapper: tls}]` on the `https` server, plus an
  `:80` reverse_proxy route for the host.
- http/https → a reverse_proxy route on `:443` (transport `http` with
  `tls: {insecure_skip_verify}` for https) and a force route on `:80`.
- TLS off (`STACKR_TLS=off`): passthrough routes are refused at create
  ("pass-through needs HTTPS on").

## Files
- `cmd/stackrd/proxy.go`: import `github.com/mholt/caddy-l4`; `go.mod`.
- `internal/service/internal/store`: `routes` table, migration, store.
- `internal/service/internal/leaf/route` (new): validate, create, list,
  delete, squat check against domains and resources.
- `internal/service/internal/leaf/domain/caddy.go`: `Route` type,
  wrapper and routes in `Build`; `caddy_test.go`.
- `internal/service/internal/flow/deploy/proxy.go`: load routes into Build.
- `internal/service/route.go` (new verbs), `internal/service/domain.go`:
  squat both ways.
- `internal/api/handler/v1/route.go`, route table, openapi regenerated.
- `cmd/stackr/cmds.go`: admin noun `route`.
- `internal/ui/drawer/admin`: Routes section (list, add, remove).
- `docs/rewrite/schema.md`, `verbs.md`.

## Tests
- `Build`: passthrough yields the wrapper and the `:80` proxy; http/https
  yield reverse_proxy with and without insecure; wrapper order.
- Leaf: host squat against a tile domain and a resource; wildcard rules;
  passthrough refused with TLS off.
- Rig: `*.stackr-test.vulpe.dev`-style pass-through to a second VM is not
  available; use a local TLS upstream container as the target and check the
  cert serial through the proxy, plus an `http` route to a LAN address.

## Out of scope
UDP/HTTP3 pass-through (wrapper is TCP only). Forward-auth key (separate
task). The leftover F PROXY protocol stays deferred.

## Migration list (serverconfig onto stackr), agreed 2026-10-06
1. Routes (this file).
2. Network shares: NFS or SMB declared once per org, tiles mount a sub path;
   databases refused on a share. Agreed in principle.
3. Config files: `files:` lines fetched from the config repo at the
   release's commit, written under the data dir, bind-mounted read only;
   folders too; a writing app gets a volume with the file overlaid on it;
   config-file only (no upload in v1); `${{ }}` refs (params, secrets, tile
   outputs, env rules) expanded on write only for lines flagged `:template`
   in the stack file, plain copy otherwise. Agreed 2026-10-07.
4. Forward-auth: a `forward_auth` entry in a domain's `proxy` block (URL plus
   headers to copy), Caddy standard modules only; the provider is reached by
   its public URL. An unauthenticated path is a second domain row with that
   path. Agreed 2026-10-07.
5. Small: `/udp` published ports refused by `parsePorts`.
6. Host access (monitoring moves in; agreed 2026-10-07): tiles may use
   `host:/abs/path:/container[:ro]` volume lines, `privileged` and devices,
   but a deploy or promote whose host lines or privileged differ from the
   stack's approved set parks blocked, "needs a server admin"; a server
   admin approves from the job, which records the set per stack; later
   releases with the same set pass; a redeploy never replans. UI and stack
   file alike. Closes the hole that `privileged` is ungated today.
7. After the migration: volume file browser back (the old
   `ListVolumeFiles`/`Read`/`Write`/`Delete`, see extracts/tar-mechanics.md).

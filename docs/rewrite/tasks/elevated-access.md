# Elevated access and network isolation

Status: agreed with darthvader 2026-10-08 (voice), not built. Grows the
host grant from the migration blitz (W5, `leaf/hostgrant`,
`flow/promote/hostaccess.go`, `flow/deploy/mount_host.go`) into one set of
elevated permissions a tile asks for and an admin approves.

## Found on the rig (2026-10-08, v0.6.0-dev.19)
Probe containers on three env networks:
- Other envs' tiles, their VIPs and another env's managed postgres: shut.
  Docker's bridge isolation holds.
- The panel (`172.17.0.1:8080`): open from every env. It runs with host
  networking (DECIDE 48, kept: the VIP iptables need the host netns), so
  bridge isolation does not cover it.
- The LAN: router DNS `192.168.1.1:53` and the front proxy
  `192.168.1.100:443` open; the internet open.
- `TRUSTED_PROXIES` is all of RFC 1918 (`installspec.DockerRanges`), so a
  tile talking to the panel directly can set `X-Forwarded-For` and get a
  fresh login limit per request.

## Baseline: what every tile gets with no grant
- Internet: yes.
- Its own env network (and shares it was given): yes.
- Other envs, other orgs' tiles and managed instances: no (Docker).
- The LAN: no. Private ranges (RFC 1918, link-local, CGNAT 100.64/10 which
  covers a tailnet on the host) are dropped for traffic a container starts.
  Replies to connections that came in (LAN users opening a site through
  Caddy, published ports) are untouched.
- Hairpin exception: the server's own address and the front proxy on 80
  and 443 stay open, so a tile can call a site hosted here by its public
  name when local DNS answers with a private IP.
- The panel: no, only Caddy (separate point, "Panel lockdown" below).
- Server ports: none unless granted.

## Elevated permissions (per tile, admin approved)
| Permission | What it allows | Status |
|---|---|---|
| Host folder mount | A server path inside the tile | built (W5) |
| Docker socket | Controls every container on the server | built, as a host mount; show it as its own row |
| Devices | A host device (GPU, USB, /dev/dri) | built (W5) |
| Privileged | Every kernel capability, all devices | built (W5) |
| LAN access | Listed addresses (IP or CIDR, optional port), or all of the LAN | new |
| Server ports | Bind listed ports on the server | new (today ungated) |
| Host networking | The server's own network: discovery (mDNS, SSDP, DHCP), no isolation | new, full trust |

Host networking implies LAN access and every port; it refuses replicas
above 1, gets no env network or VIP, and Caddy reaches it through the
server's port. Homelab users: Home Assistant, Homebridge, Scrypted,
Pi-hole or AdGuard as DHCP, Plex and Jellyfin discovery, UniFi adoption,
node exporter and Netdata.

Later candidates, not agreed: added capabilities (NET_ADMIN for VPN
tiles), panel access for a container SDK or agent.

## Flow
- The tile asks: stack file or the tile's UI settings. The plan lists what
  is new and needs approval.
- The deploy parks (as W5) until a server admin approves; the admin sees
  the request and can edit it before approving, and change or revoke it
  later. Revoke leaves running tiles; the next deploy parks.
- Grants become per tile. Today the row is per stack and its lines are
  stack wide, so tile B can use a mount approved for tile A. The row keys
  each line by tile slug.
- The stack drawer badge "host access" stays; the tile shows which
  permissions it holds.

## Decided 2026-10-08
- An admin approves a subset of what was asked (untick to narrow). What
  is left stays waiting; the deploy does not go ahead with less. To get
  unblocked the tile drops the permission from its plan, or waits
  (darthvader).
- Existing per-stack grants are reset; stacks ask again (no users).
- "All of the LAN" is everything the baseline blocks, tailnet range too.
- `published_ports` keeps its name and becomes the gated server ports;
  privileged and devices move into the tile's Elevated access section.
- The docker socket stays a host mount line, shown as its own row.
- A card chip names the strongest permission the tile holds.
- DNS: Docker's resolver forwards a LAN upstream (the rig's router) from
  the container's netns, so the configured resolvers on port 53 are an
  exception to the LAN block, or every tile loses DNS.
- Hairpin to this server's own 80/443: Docker's bridge isolation drops it
  today; the filter chain ACCEPTs bridge to Caddy on 80/443 before Docker's
  chains (the same flow as reaching the site publicly). Verify on the rig
  first.
- A `${{ tile.x.url }}` ref to a host-network tile gets a plan warning;
  other tiles reach it by the server address and a granted port.
- IPv6 not covered (off on the rig).
- An admin learns a request waits from a count badge on the Admin rail
  icon; email later, once mail is wired.
- Editing or revoking an approved grant: LAN access changes at once (the
  firewall rules rebuild on the grant change); mounts, devices,
  privileged, ports and host networking change on the next deploy,
  since they are baked into the container.

## Panel lockdown (next point, not yet agreed)
Only Caddy reaches the panel; the panel trusts `X-Forwarded-For` only from
Caddy's address. Rules live in the stackr-owned iptables chain beside the
VIP rules.

## Open
- Stack file syntax: `lan: [ip|cidr[:port] | all]`, `published_ports`, `network: host`.
- UI: fraedi `stackr/design` board 5 "Elevated access" (67dc8754).
- Implementation plan: `elevated-access-blitz.md` (Fable, 7 waves).

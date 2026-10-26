# DNS connectors (plan, 2026-10-22)

Status: draft for darthvader's review. Decisions agreed in chat on
2026-10-22; research in the session scratchpad (`connectors-research.md`).

## Decisions

- The controller is the only thing that talks to a DNS provider. Caddy keeps
  doing ACME, but a small stackr DNS module (`dns.providers.stackr`) hands
  every record add/remove to stackrd over a socket in the proxy admin dir.
  Tokens never reach the proxy env, the Caddy config or its autosave.
- A DNS connector is a connector with the `dns` capability. Built in:
  Cloudflare and Namecheap. Server level (admin, shared with orgs like git
  connectors) and org level (an org owner's own account, that org only).
  The server owner is fully trusted; no UI note about it.
- Managed domains: a connector lists the zones its token sees; the admin (or
  org owner) ticks which ones stackr manages. Each ticked zone becomes a
  domain resource (instance level shared with chosen orgs, or org level).
  Members pick from their allowed domains when adding a tile domain.
- For a managed domain stackr: gets certs (wildcard where it fits), creates
  A/AAAA records pointing at the serving ingress node, repoints them when
  ingress moves, deletes them when the domain goes. It only touches records
  it created, tracked in its own table.
- Record target: the ingress node's detected public IP, with an optional
  per-node override (floating IP, load balancer). Hetzner floating IPs also
  need the IP on the box's interface: docs say so.
- Repointing: planned ingress moves only. When an ingress node is down the
  panel names the affected domains and offers one-click "move ingress".
  Automatic failover later.
- Dropped (no users, no migration): `dns_provider` setting, `DNS_API_TOKEN`
  on the proxy, the installer's `--dns-token` flag and prompt, the
  `caddy-dns/cloudflare` import. The setup wizard's Certificates step gets
  the provider picker.
- Connector rows get a `capabilities` list (`git`, `dns`); `provider` is the
  kind (`github`, `cloudflare`, `namecheap`). `/hooks/connectors/<id>` stays
  generic.
- Credentials are pasted API tokens/keys in v1. "Connect with Cloudflare"
  (one public FyrmForge OAuth client with PKCE, a static FyrmForge bounce
  page back to the install) is backlog.
- Backlog, not this round: third-party providers, provider connectors
  (Hetzner, DigitalOcean, Proxmox; e.g. moving floating IPs), automatic DNS
  failover, folding registry credentials and backup destinations into
  connectors.

## UI and CLI (agreed 2026-10-22)

- v1 builds Namecheap only; Cloudflare later on the same interface. The
  install-time Cloudflare wildcard goes.
- Namecheap connector form: username, API key ("Controls your whole
  Namecheap account."), server IPv4 prefilled from the probe ("Add this IP
  to Namecheap's API allow list."), a short how-to. Save lists the domains;
  errors name the cause (bad key; IP not allowlisted, naming the IP).
- The connector's domain list (only who manages the connector: server
  admins for a server one, org owners for an org one) shows every domain on
  the account: Managed, Not managed (one click), DNS elsewhere ("Take over
  DNS": switches the domain to Namecheap DNS via the API, typed-domain
  confirm, warns that records at the old host stop applying).
- Tile domain field: a dropdown of the domains that tile may use (computed
  from the connectors its org reaches: shared server domains + the org's
  own) plus a subdomain box; "Other (I'll set DNS myself)" keeps today's
  free host. Domains not usable or not managed never show.
- CLI by level, like git connectors today: `stackr server connectors
  add namecheap ...` (admin), `stackr org connectors add namecheap ...`
  (owner, `--org`). `... connectors domains <connector>`, `manage
  <connector> <domain>`, `unmanage <connector> <domain>`, `takeover
  <connector> <domain>` (typed confirm).

- A managed domain on a server connector has "Available to": all orgs
  (default) or chosen orgs, like sharing a backup destination. An org
  connector's domains are only ever that org's.

- Unmanage or connector delete is refused while tiles use the domain (the
  error names them). Once unused, stackr deletes only the records it
  created, then forgets the domain.

- A name that already has a record stackr didn't create is refused, with
  "Replace it" behind a confirm; replaced records are tracked as stackr's.

- Certs for managed domains: a wildcard per level in use (a `*` covers one
  label only; auto names are tile.stack.org.root, so e.g.
  `*.<stack>.<org>.<domain>` is issued when a tile there first needs it;
  typed one-level names share `*.<domain>`). Anything else falls back to a
  per-host cert. Hand-pointed ("Other") domains keep per-host certs.

- Records: TTL 300 s; A to the ingress node's public IPv4, AAAA only when
  the node has a public IPv6.

## Provider notes

- Cloudflare: `libdns/cloudflare` (already an indirect dep). Token scoped to
  Zone:DNS:Edit. Lists zones.
- Namecheap: `libdns/namecheap`. setHosts rewrites the whole zone, so one
  writer (stackrd) with a per-zone lock is required. API calls must come
  from an allowlisted IPv4: the form takes the client IP explicitly,
  prefilled from stackr's probe, and the check names error 1011150 as
  "allowlist this IP". The key controls the whole account: the form says so.
  A sandbox endpoint exists for the rig.

## Tasks

Each sized for one helper in its own worktree off master.

### D1. DNS capability and connectors

- `internal/service/internal/leaf/dnscap/` (new): `Provider` with
  `Zones`, `Get`, `Append`, `Set`, `Delete` (libdns records), built-in
  adapter for Namecheap (Cloudflare later), a per-zone mutex around each.
- Connectors: `capabilities` column, `provider` as kind, DNS config
  (encrypted, like GitHub's). Add/check/zones verbs at server and org level,
  owner/admin gated; sharing as git connectors.
- Panel: Admin > Connectors and org Connectors > Add > DNS: Cloudflare /
  Namecheap; save runs a check and lists zones. CLI: `stackr connector add
  dns <kind>`, `stackr connector zones <name>`.
- Check: fake provider tests for the lock (two writers to one zone never
  interleave), check errors, org connector invisible to another org.

### D2. Caddy DNS module and ACME through stackrd

- `dns.providers.stackr` Caddy module in the proxy build: forwards
  Append/Delete to stackrd over a unix socket in the proxy admin dir; worker
  ingress proxies reach the controller the same way their config arrives.
- `leaf/domain/caddy.go`: wildcard policy per domain resource with a DNS
  connector (replaces `Install.DNSProvider`); drop `dns_provider`,
  `DNS_API_TOKEN`, `--dns-token`, the Cloudflare import.
- Check: caddy golden tests (no token anywhere in config), a socket test
  with a fake provider, worker ingress gets the policy.

### D3. Managed domains and records

- Zone picker → domain resources (instance shared with chosen orgs, or org
  level). Refuse a zone already a resource elsewhere.
- Records table: what stackr created (zone, name, type, value, resource,
  tile domain). Create on domain add, repoint on ingress move, delete on
  domain remove; never touch rows not in the table.
- Node: optional public IP override (panel node drawer, `stackr node set
  --public-ip`). Down ingress: affected domains listed with "move ingress".
- Tile domain form: pick allowed domain + subdomain.
- Check: record lifecycle tests with a fake provider (add, move, remove,
  foreign record untouched), org B can't use org A's zone.

### D4. Wizard and docs

- Setup wizard Certificates step: add a DNS connector, pick zones.
- Docs: domains guide, node public IP override and floating IPs, Namecheap
  allowlist.

## Order

D1 first; D2 and D3 in parallel after D1; D4 last. Then a rig pass on fyrmforge.dev (Namecheap, creds in
the repo .env; the only domain tests may touch; snapshot of its records
before tests in the session scratchpad).

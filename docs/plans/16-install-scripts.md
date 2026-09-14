# Install, deploy and wipe scripts

Status: `deploy-test.sh` and `wipe-test.sh` implemented and exercised on the
test box 2026-09-03. `install.sh` written the same day but **never run** — the
images it pulls are not published yet.

## Where this came from

QA on the test box, 2026-09-03. Three things surfaced:

1. The old `scripts/install.sh` came in with the first commit and had never run
   anywhere. It installed a bare binary under systemd at `/usr/local/bin/stackr`
   — which is the CLI, not the daemon (`cmd/stackrd`). It asked for nothing, so
   the panel had no `BASE_URL` and no `ACME_EMAIL`.
2. `scripts/dev/deploy-test.sh` could not run on a wiped box: the panel creates the
   `stackr` network at boot but has to join it to start.
3. There was no real wipe script. Wiping by hand left Traefik serving a config
   from before the wipe (the 2026-09-01 incident, docs/plans/13-proxy-probe.md), and
   a half wipe left 20 orphan networks, 129 images and four old databases behind.

## Decisions

### The panel runs as a container, not a host binary

Traefik's `stackr-auth` middleware forwards auth to `http://stackr-panel:8080`
and the route probe dials `http://stackr-traefik/`. Both are docker-network
names. A systemd binary on the host can reach neither.

### The deployment definition is a compose file, not `docker run` flags

`$DATA_DIR/docker-compose.yml`, mode 600, written by the installer and owned by
the user afterwards.

The alternative was a long `docker run` in the install script plus a `.env`
beside it. Rejected because it makes upgrading depend on re-running the
installer with byte-identical flags — and silently reverts anything the user
changed. With a compose file, upgrade is `docker compose pull && up -d` and the
user can read their own deployment.

Cost: needs the compose plugin. Ships with docker-ce; the installer checks.

### Config splits into two tiers, and always will

The rule: **config needed to reach the store can never live in the store.**

- **Compose `environment:`** — `DATA_DIR`, `DATABASE_PATH`, and the startup
  values the daemon reads before anything else. When stackrd grows a Postgres
  backend, its DSN joins this tier by necessity, and it is one more line in a
  file that already exists.
- **The `settings` table** — everything the panel can meaningfully edit.
  Already used this way for `dns_provider`, `traefik_static_override`,
  `proxy_custom_dynamic` (`internal/stackrd/handlers/api/v1/proxycfg.go`).

Migrations run automatically at daemon start, so the installer never creates the
database or runs anything against it.

### The installer asks; the panel could ask later

`BASE_URL`, `ACME_EMAIL` and `STACKR_TLS` go in the compose file's
`environment:`, which is exactly what `cmd/stackrd/main.go` already reads. Zero
daemon changes.

The better end state is the panel asking, in a first-run wizard step, stored in
`settings` — real form validation, editable forever, no "typo'd it at install
time, now edit a file and restart". Not done because it needs settings-table
read paths with env fallback, first-boot seeding, and live reconfiguration
(rewrite `panel.yml`, restart Traefik) when the hostname changes. Both functions
exist (`proxy.writePanel`, `proxy.restartTraefik`), so it is wiring, but it is a
feature and not part of shipping an installer. The compose file means moving to
it later strands nobody.

### No DNS resolve check at install time

Considered: `dig` the hostname and compare against the box's public IP, since
DNS-not-pointed is the most common reason a first install ends with no cert.
Rejected:

- You cannot reliably know "this box's IP". A cloud VM sees a private address,
  so you would need an outbound call to something like `ifconfig.me`.
- Correct setups fail the check. Cloudflare-proxied DNS points at Cloudflare by
  design; so does any load balancer or NAT forward. IPv6-only AAAA too.
- `dig` is missing on minimal images.

Instead the installer prints the two records that must exist and moves on. The
panel is the better place to notice a certificate that never issued.

### TLS branches on two questions, not one

`proxy.routablePanel` already refuses to write a route for a bare IP or
`localhost`, so those installs get `STACKR_TLS=off` with no email prompt.

That is not enough on its own. A tailnet name like `box.tail1234.ts.net` is
dotted, so it looks public and takes the ACME path, then sits there failing the
http challenge because Let's Encrypt cannot reach a tailnet. So after the
hostname the installer asks outright: "Is `<host>` reachable from the public
internet?" — no, and TLS goes off. One prompt, correct for tailnets, LANs and
split-horizon DNS, none of which a suffix list would ever fully cover.

### Wildcard DNS is required, not optional

`BASE_URL`'s host is both the panel's own name and the suffix new org domains
default to — a panel at `panel.example.com` gives orgs
`<org>.panel.example.com` and tiles a layer under that. Depth is
unrestricted; each name gets its own certificate via the http challenge, no
wildcard *certificate* involved. But every generated name has to resolve, so
`*.<host>` must have an A record alongside `<host>`. The installer prints both.

### The wipe returns the box to stock + Docker + Tailscale

Every container, network, volume and image on the box, and the data dir deleted
for real. Not "every stackr container" — everything, because a half wipe is what
caused the incident.

## The three scripts

### `scripts/install.sh`

Prompts, in order, each re-asked until valid:

| Prompt | Default | Validation |
|---|---|---|
| Panel hostname | none | strips a pasted `https://` and any path, lowercases, rejects bad characters, empty labels, leading/trailing dots and dashes, and a single label with no dot |
| Data directory | `/var/lib/stackr` | absolute path |
| Reachable from the internet? | yes | only asked when the hostname is not an IP or `localhost` |
| ACME email | none | only asked when TLS is on; `x@y.z`, no spaces |
| HTTP / HTTPS ports | 80 / 443 | numeric, 1-65535, and **not already listening** — Traefik failing on a taken port an hour later is the failure this prevents |

Then: root/docker/compose checks, write the compose file, pull, `up -d`, print
the panel URL, the config path, the upgrade command, and the two DNS records.

Re-running with an existing compose file asks before overwriting and otherwise
tells you the upgrade command.

**Known wart:** `runtime.ProxyRelayImage` is the hardcoded local tag
`stackr-proxyrelay:local`, so the installer pulls the published relay image and
`docker tag`s it to match. That should become an env var on the daemon.

### `scripts/dev/deploy-test.sh`

Local dev deploy: build locally, rsync, build the image on the box, swap the
container. `docker network create stackr` before the run, and `$HOME`-relative
defaults so it is not personal to one machine.

### `scripts/dev/wipe-test.sh`

Refuses unless the operator retypes the hostname — it removes every Docker
object on the box, not just stackr's, so a typo must not survive. Then: stop all
containers, delete the data dir, prune everything, `docker network rm stackr`.

The data dir is deleted from inside a throwaway `alpine` container with the
parent bind-mounted. It is root-owned (Traefik writes `acme.json` 600 root) and
ssh has no TTY for sudo. The prune runs last so it also takes the alpine image
those steps pulled.

`--keep-certs` copies `acme*.json` out and back. Off by default (true stock).
On, because Let's Encrypt caps duplicate certificates at 5 per week per name set
and a day of repeated wipes burns that.

## Not done

- Running `install.sh`. It needs published images; there is nothing to pull.
- A `curl | sh` bootstrap. It is the shape the world expects and also the shape
  that runs an unreviewed script as root. Undecided.
- Moving `BASE_URL`/`ACME_EMAIL` into the `settings` table and the setup wizard.
- Keeping a systemd/binary install path.
- Uninstalling Docker or Tailscale in the wipe. Those are the floor.

## Noted while deciding, out of scope here

Tile secrets are delivered as plain `KEY=VALUE` in `ContainerSpec.Env`
(`internal/stackrd/infra/runtime/runtime.go:444`), so they are readable via
`docker inspect` and `/proc/1/environ`. The storage layer above (encrypted
`variables` rows, varref resolution, plan/apply, secret links) is unaffected by
how delivery works, and a Swarm backend would change only the runtime package.
Dokploy avoids the exposure with Docker secrets and the `*_FILE` convention;
plain compose supports `secrets:` too, bind-mounting to the same
`/run/secrets/<name>` path. Not actionable for arbitrary variables, since most
images do not read `_FILE`.

# Host setup

Prerequisites a host needs before running the stackr control plane. This is the
spec the real installer should implement; today `scripts/dev/deploy-test.sh` only
ships the app, not the host prep. `scripts/dev/wipe-test.sh` takes the same box
back to a fresh install — containers, networks and data dir, not just the DB;
a partial wipe leaves Traefik serving a config from before it.

## Installing

On a Docker host, as root:

```
curl -fsSL https://github.com/FyrmForge/stackr/releases/latest/download/install.sh | sudo bash
```

To read it first:

```
curl -fsSLO https://github.com/FyrmForge/stackr/releases/latest/download/install.sh
less install.sh
sudo bash install.sh
```

The script downloads `stackr-install` for your CPU from the same release,
checks it against the release's `checksums.txt`, and runs it. The installer
is a form: arrow keys or tab move between fields, and bad answers show under
the field.

A domain is required. Apps are reached by name through Traefik and so is the
panel, so an install on a bare IP address is not offered.

It asks for the root domain apps are named under (`example.com` gives
`app.stack.org.example.com`), the panel hostname (default `stkr.example.com`),
whether the box is behind Cloudflare, the IP address of any other proxy in
front of it (`192.168.1.100` or a range like `10.0.0.0/8`), whether HTTPS is
on, the ports, and with HTTPS the Let's Encrypt email.

HTTPS off is for a name the Let's Encrypt challenge cannot reach (a LAN or
tailnet name), or when something in front already terminates HTTPS. The panel
and apps then serve plain HTTP and Traefik takes no port 443.

DNS needs the panel host and `*.<root>`. Before installing, the summary warns
when either does not resolve yet; you can install anyway and add them after.
The root and proxy answers are copied into the panel on first boot; change them
there afterwards (the server page for the domain, Admin, Proxy for the rest).

`--dry-run` shows the form and prints what it would run without changing
anything; it needs no root and no docker:

```
curl -fsSL https://github.com/FyrmForge/stackr/releases/latest/download/install.sh | bash -s -- --dry-run
```

Every question has a flag, and `--yes` installs from the flags with no form
(`--help` lists them):

```
curl -fsSL .../install.sh | sudo bash -s -- --yes \
  --domain example.com --proxy 192.168.1.100 --email ops@example.com
```

Data lives in `/var/lib/stackr`; `STACKR_DATA_DIR=/srv/stackr` (through
`sudo STACKR_DATA_DIR=... bash`) puts it elsewhere.

`--version X.Y.Z` pins a release (`| sudo bash -s -- --version X.Y.Z` when
piped). Without `--yes` the form needs a terminal, so run it from an
interactive shell (`ssh -t` for a remote one).

To build the installer yourself: `make installer`, then
`sudo bin/stackr-install --version X.Y.Z`.

## Traffic between nodes is not encrypted

The swarm overlay stackr creates is plain VXLAN. Everything one node sends
another crosses the wire in the clear: app to database, tile to tile, the
agent's own API. Docker can encrypt an overlay, but the flag is fixed when the
network is created and cannot be changed afterwards, so turning it on later
would mean tearing down every service and network on the box and rebuilding
them, panel included.

**So: join nodes over a VPN or a private network you trust.** Tailscale,
WireGuard, or a datacenter VLAN nobody else is on. Never join two nodes across
the open internet.

Ports to open between nodes: 2377/tcp, 7946/tcp and udp, 4789/udp. ESP is not
needed, there is no IPsec.

## Behind Cloudflare

Tick "Trust Cloudflare" on the admin Proxy page. Traefik then takes
X-Forwarded-For from Cloudflare's ranges, so apps see the real visitor IP.

Also firewall 80/443 to Cloudflare's ranges. Otherwise the origin is still
open on its own IP, and anyone can skip Cloudflare.

## conntrack byte accounting — required for the graph traffic overlay

The live traffic overlay on the environment graph (animated edges + Bps labels)
is driven by `internal/stackrd/infra/metrics` reading the **host** conntrack table and
aggregating per-connection byte deltas into tile-to-tile rates. Those `bytes=`
columns only exist when **conntrack byte accounting is enabled** on the host.

Without it: the graph draws static dependency edges but **no traffic pulses or
rates** — every pair aggregates to 0 Bps. The rest of stackr is unaffected.

### Enable it (runtime, no reboot)

Accounting is a per-netns sysctl and takes effect immediately for **new**
connections:

```bash
sysctl -w net.netfilter.nf_conntrack_acct=1
```

No module reload or reboot needed. Only connections **created after** this get
counters — long-lived pooled connections (e.g. between API tiles) stay uncounted
until they reopen, so bounce the workload if you enabled it while it was running.

### Persist across reboots

```bash
echo 'net.netfilter.nf_conntrack_acct=1' > /etc/sysctl.d/99-conntrack-acct.conf
# applied automatically on boot by systemd-sysctl
```

A setup script should write that file and also run the `sysctl -w` once so it's
live in the current boot without waiting for a reboot.

### Verifying it's active — read the HOST netns, not the container's

This is the trap that cost real time: `/proc/net` symlinks to `/proc/self/net`,
so inside the stackr container `/host/proc/net/nf_conntrack` shows the
**container's** netns (empty), not the host's. The sampler correctly reads pid
1's netns — `hostNetPath()` returns `$HOST_PROC/1/net/<file>`, i.e.
`/host/proc/1/net/nf_conntrack`. Check the same place:

```bash
# on the host directly:
grep -c bytes= /proc/net/nf_conntrack            # >0 once traffic flows

# from inside the stackr container — MUST use /1/net (pid 1 = host netns):
docker exec stackr grep -c bytes= /host/proc/1/net/nf_conntrack   # >0
# /host/proc/net/nf_conntrack is the container's own netns — always ~0, ignore it
```

With traffic flowing and accounting on, `bytes=` rows appear and the graph shows
rates within a sample interval (~5s). Note the counts are bursty: short-lived
request connections carry counters only while alive, so a single-shot grep can
miss them — sample a few times.

## Restoring the panel

A panel backup is `stackr.tar.gz`: the database, the master key that
decrypts every secret in it, and the build version that wrote it. The key
is in the archive on purpose. Without it the database is ciphertext no
other host can read, so a restore would come back with every password,
deploy key and app secret gone. That also makes the archive as sensitive
as the panel itself, so keep it somewhere private and delete your local
copy when you are done.

Restore runs on the host, not from the panel, because the files have to be
in place before stackr starts:

```
scripts/restore.sh /path/to/stackr.tar.gz [data-dir]
```

Data dir defaults to `/var/lib/stackr`, which is what `install.sh` offers.
The script prints the archive's version next to the running image, asks
for confirmation, scales the `stackr` service to zero, keeps a copy of the
files it is replacing (suffixed `.before-<timestamp>`), unpacks, and
scales back up. When the archive was written by a different release than
the running image, the service moves to the archive's release first, in
either direction. Restoring an older backup after an upgrade also moves the
panel back to that build; upgrade again from the panel afterwards.

Deployed services are not touched. Stackr reconciles them as it starts.

Archives written before the panel backup carried the key are missing
`keys/master.key`, and the script refuses them rather than restoring a
database it cannot decrypt.

## Upgrading the panel

Upgrades run from the panel: Admin, Update. The rail shows an arrow when a
newer release is out. `install.sh` installs once and refuses a host that
already runs stackr.

The button pulls the new panel and relay images, writes
`<data-dir>/backups/pre-upgrade-<version>.tar.gz`, then points the `stackr`
service at the new image. The new panel migrates the database and rolls the
node agents to the same build. A new version that does not stay up for a
minute is rolled back by swarm; the database is not, so the way back from a
bad upgrade is `restore.sh` with that archive, which also moves the service
back to the previous release. The update page prints the command.

Supported path is one release to the next. Skipping versions is not tested.

If the panel will not start at all, move it by hand on the manager:

```
docker pull ghcr.io/fyrmforge/stackr-proxyrelay:<version>
docker tag ghcr.io/fyrmforge/stackr-proxyrelay:<version> stkr-proxyrelay:local
docker service update --image ghcr.io/fyrmforge/stackr:<version> \
  --env-add STACKR_IMAGE=ghcr.io/fyrmforge/stackr:<version> stackr
```

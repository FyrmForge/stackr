# stackr

A Go web application built with the [HAMR framework](https://github.com/FyrmForge/hamr).

## Using stackr

Step-by-step guides, in the order you need them:

1. [Install a panel](docs/guide/install.md), first login and onboarding
2. [Nodes](docs/guide/nodes.md): add a worker, pin envs, ingress and egress, rejoin, break glass
3. [Upgrade](docs/guide/upgrade.md): the panel and every worker
4. [Backups](docs/guide/backups.md), including Back up node
5. [Remove a node](docs/guide/remove.md) and `stackr-install uninstall`

## System requirements

What a box needs to run stackr, as a controller (`stackr-install`) or a
worker (`stackr-install join`). The installer checks all of it up front and
names whatever is missing.

- **Linux, run as root.**
- **Docker Engine 26.0 or newer**, with its default iptables firewall
  backend (not `firewall-backend: nftables`). 26.0 is the first whose volume
  mounts take a subpath, which config files use. When Docker is missing
  the installer offers Docker's install script (get.docker.com, needs curl);
  `--yes` runs it without asking. An installed Docker is never upgraded.
- **iptables 1.8 or newer.** A controller needs the nf_tables variant
  (`iptables-nft`): the panel image writes nf_tables rules, which Docker's
  chains only see in the same backend. A worker uses the host's own.
- **WireGuard in the kernel:** Linux 5.6 or newer, or the `wireguard`
  module. A single controller runs without it and warns; a worker refuses.
  In a container (LXC) the module must be loaded on the host.
- **Worker only:** systemd (the worker is a systemd service) and iproute2's
  `ip`.
- **Free ports**, checked on a first install:
  - UDP 51871, the mesh's WireGuard (`--wg-port` to pick another; stackr's
    interface is `stackr0`, so a VPN on `wg0`/51820 is left alone).
  - Controller: TCP 80 and 443 (`--http-port`, `--https-port`), TCP
    127.0.0.1:5000 (the image registry).
  - Worker: TCP 7443 (worker API, tunnel only), TCP 7444 (liveness, public),
    TCP 127.0.0.1:5000 (registry front). An ingress worker also needs TCP 80
    and 443; tagging it ingress refuses while something else holds them.
  - Exit links use UDP 52000 to 52255 and a relay node UDP 52300 and up,
    one port per pair, only while one is set up.
- **Address ranges stackr reserves.** A box whose networks overlap them
  cannot install or join:
  - 10.207.255.0/24, the mesh's tunnel addresses
  - 10.208.0.0/12, the cluster's network pool (every env, shared and ingress network is a /24 of it)
  - 10.251.0.0/22, the exit links

## Prerequisites

- Go 1.27.1+
- [templ](https://templ.guide) CLI (`go install github.com/a-h/templ/cmd/templ@latest`)
- [hamr](https://github.com/FyrmForge/hamr) CLI

## Quick Start

```bash
# Install dev tools (templ)
make install

# Run the dev server (builds, live reload)
hamr dev
```

The server starts at [http://localhost:3000](http://localhost:3000) (proxied from `:8080`). If those ports are busy on your machine, hamr walks +1 (3001/8081 etc.) and prints the actual URL in its startup banner.

## Development

```bash
hamr dev            # Run dev server (live reload)
make build          # Build the panel to bin/stackrd
make installcli     # Install the stackr CLI
make installer      # Build bin/stackr-install
make test           # Run tests
make lint           # Run golangci-lint
make templint       # Lint .templ files
make db-sh          # Open sqlite3 shell to local dev DB
```

> **Note:** Migrations run automatically when the server starts.

## Project Structure

```
cmd/stackrd/             Panel (web UI + API), later also `stackrd proxy`
cmd/stackr/              CLI
cmd/stackr-install/      Installer
internal/
  config/                Environment configuration
  db/                    Database + migrations
  repo/                  Data access layer
  web/                   HTTP handlers + components
ui/                      Static assets, CSS source, npm config, dist/
docs/                    Documentation
```

## Stack

- **Go** 1.27.1 + Echo v4
- **Templ** + HTMX
- **SQLite**
- **HAMR Framework** — server, middleware, validation, responses

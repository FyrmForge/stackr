# stackr

[![CI](https://github.com/FyrmForge/stackr/actions/workflows/ci.yml/badge.svg)](https://github.com/FyrmForge/stackr/actions/workflows/ci.yml)

stackr is a self-hosted deployment platform for Docker Swarm. It builds and
runs applications, provisions shared infrastructure, manages routing and
backups, and presents the whole system as a live dependency canvas.

## Features

- Git and image deployments with immutable registry artifacts.
- Multiple environments, promotion, rollback and pull-request previews.
- Reviewable stack and organization configuration as code.
- Managed PostgreSQL, MariaDB, MongoDB, Redis and S3-compatible storage.
- Logical database and bucket slices from shared instances.
- Traefik routing, automatic TLS and environment-aware domains.
- Scheduled jobs, one-shot functions and durable background work.
- Multi-node placement, replicas, node agents and two-pass volume moves.
- Backups for databases, volumes and the stackr control plane.
- REST API, OpenAPI document and the `stackr` CLI.

## Development

Requires Go 1.26 or newer, Docker, Node.js and the HAMR CLI.

```bash
make install
hamr dev
```

The development server watches Go, templ, CSS and static assets. Database
migrations run automatically at startup.

Before submitting a change:

```bash
make test
make lint
make templint
```

## Repository layout

| Path | Purpose |
|---|---|
| `cmd/stackrd` | Control-plane server and node agent |
| `cmd/stackr` | Command-line client |
| `cmd/proxyrelay` | Local port-forward relay |
| `internal/stackrd/handlers` | Web and API transport |
| `internal/stackrd/config` | Config parsing, planning and apply |
| `internal/stackrd/infra/cluster` | Boundary for Docker access across nodes |
| `internal/stackrd/infra` | Deploy, proxy, backup, registry and node services |
| `internal/stackrd/store` | SQLite persistence and audit records |
| `internal/cli` | CLI commands and API client |
| `frontend` | Tailwind source and browser assets |
| `docs/features` | Feature contracts and design notes |
| `docs/plans` | Active plans and compact implementation records |

See [AGENTS.md](AGENTS.md) for project conventions and
[docs/host-setup.md](docs/host-setup.md) for host requirements.

## Status

Pre-release beta. The schema and operational contract may still change without
a compatibility path.

## License

Stackr is licensed under the [Apache License 2.0](LICENSE).

# stackr

A Go web application built with the [HAMR framework](https://github.com/FyrmForge/hamr).

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
frontend/                Static assets, CSS source, npm config, dist/
docs/                    Documentation
```

## Stack

- **Go** 1.27.1 + Echo v4
- **Templ** + HTMX
- **SQLite**
- **HAMR Framework** — server, middleware, validation, responses

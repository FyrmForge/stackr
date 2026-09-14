# stackr — Claude Guidelines

## Build & Test

```bash
make install        # Install dev tools (templ)
# Migrations run automatically when the server starts
hamr dev            # Run dev server (file watching, builds, live reload)
make build          # Build binary (generates templ first)
make test           # Run tests
make lint           # Run linters
make templint       # Lint .templ files for silent failures and a11y issues
```

## Project Conventions

See AGENTS.md for all project conventions, patterns, and guides.

## Framework Reference

See the [HAMR repository](https://github.com/FyrmForge/hamr) for framework
documentation matching the version in `go.mod`.

## hamr MCP

`hamr dev` exposes these tools over MCP. Prefer them over doing the same
thing by hand — they read the live dev server, so their answers are current
and cost the developer nothing.

- Never ask the developer to paste logs, and never tail a log file — `logs.read` (app + build output), `console.read` (browser console, uncaught errors, CSP violations), `http.read` (request log).
- The dev server is already running. Never run `make build`, `go build`, or start a second server — `rule.run` rebuilds one watch rule, `rebuild.all` rebuilds everything, `make.run` runs a Makefile target.
- Check dependency containers with `docker.status` / `docker.logs` before assuming a connection error is app-side.
- `docker.restart` restarts a service; `docker.wipe` resets its volumes.
- Never ask what an email said — `mail.list` and `mail.get` read the dev inbox.
- `mail.clear` empties it; `mail.ingest` injects a message.
- Never ask what an SMS said — `sms.list` and `sms.get` read the dev inbox.
- `sms.clear` empties it; `sms.ingest` injects a message.
- Never guess at payment state — `stripe.list` reads the mock's objects.
- `stripe.complete` / `stripe.expire` / `stripe.refund` drive a payment to an outcome.
- `dev.info` reports the running rules, ports (including walked ones), and versions — read it before assuming a port.

If a call fails with "dev not running / gateway off", say so instead of
falling back to manual steps — the developer needs to start `hamr dev`.

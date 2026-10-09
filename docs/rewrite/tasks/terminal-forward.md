# Terminal and port forward

Status: built 2026-10-08 (commit d3f0f93), verified on the rig as
v0.6.0-dev.21: `stackr ssh` (TTY, exit code), the drawer Terminal tab in
a browser, `stackr forward` to bridge and host-network tiles. Planned
from the v0.5.0 code (darthvader: "it worked
well") and a Fable survey of the rewrite. Wanted in `leftovers.md`
("Port forwarding", "Interactive shell"). Terminal first; forward reuses
its websocket plumbing.

## Agreed
- Transport: `github.com/coder/websocket`, as v0 (Origin check and pings
  built in). New direct require in `go.mod`.
- One backend per feature, on the API router, used by the CLI (bearer
  key) and the browser (session cookie; `Access.Load` falls back to the
  session, `APICSRF` only checks unsafe methods, so a GET upgrade passes).
- **No relay.** v0 needed a relay container because its panel sat on the
  `stackr` bridge. The rewrite panel runs with host networking
  (`internal/installspec/installspec.go` `HostNetwork: true`), and the host
  reaches every env bridge: checked on the rig 2026-10-08, the host and
  the panel container both open 172.22.0.2:5432 and 10.213.48.4:80. The
  panel dials the replica IP directly. `hamr dev` runs on the host too.
- Forward presence cards on the canvas (v0 avatars) stay parked
  (`ui-plan.md`); the graph poll can show them later without a presence
  socket.
- Audit: a `slog.Info` pair per session (opened, closed with duration:
  user, tile, container, shell or port), as v0. No table.
- Verb for both routes, and for `tile exec`: `tile.write` (org owners),
  was `container.admin` (darthvader 2026-10-08: owners already deploy any
  code into the tile; elevated access stays admin only).

## Gotchas every piece must keep (all from v0)
- Exec-create or dial **before** `websocket.Accept`: after the upgrade
  only a close code reaches the client, so "not running", "no port" and
  "connection refused" must be HTTP errors.
- Context: `context.WithCancel(context.WithoutCancel(req ctx))`. The hamr
  30s request timeout killed v0 sessions at exactly 30s.
- Ping every 30s, `CloseNow` on failure, for both features: a closed
  laptop must not leave an exec or a dial open forever.
- Forward: `websocket.NetConn` (lifts the 32 KiB read limit) and
  half-close: copy ws->tcp, then `CloseWrite`, keep copying tcp->ws until
  the tile closes. The CLI waits on the reply direction. Without it psql
  and curl lose the reply.
- TTY streams are raw: no `stdcopy` demux. A new method, not a flag on
  `ExecStream`.

## Wave 1: terminal backend
- `internal/service/internal/docker/exec.go`: `ExecTTY(ctx, id, cmd)`
  returning the attach conn, `resize(cols, rows)`, `close`, and the exit
  code after the stream ends (`ContainerExecInspect` under
  `WithoutCancel`, as `execResult`). `Tty: true` on create and attach.
- Shell argv: `sh -c 'command -v bash >/dev/null && exec bash || exec sh'`;
  `?shell=` replaces it with that one binary.
- `internal/service/docker.go` interface, `dockerfake/fake.go` (`ExecTTY`
  on a `net.Pipe`, records resizes).
- `leaf/tile/world.go`: a TTY verb behind the existing `guard` (system
  containers refused).
- `internal/service/tile.go`: `Orchestrator.Shell(ctx, tileID, container,
  shell)` via `o.replica()` (first running replica unless `?container=`),
  slog open and close. `Terminal` stays for `tile exec`.
- `internal/api/handler/v1/streams.go`: `Terminal()` as
  `Streamed("websocket", ...)` so the OpenAPI dump stays valid. Binary
  frames are bytes both ways; text frame `{"type":"resize","cols","rows"}`
  in; a last text frame `{"type":"exit","code":n}` out.
- `internal/api/routes.go`: `{GET, tile+"/terminal", "tile.terminal",
  `tile.write`, h.Terminal()}` with `.Q("container","shell")`.
- `internal/api/server.go`: add `/terminal` and `/forward` to the gzip
  skipper.
- Tests: orchestrator picks the running replica and refuses a system
  container; API upgrade with a bearer key on `httptest` (pattern in
  `cmd/stackr/cli_test.go`), bytes round-trip, resize reaches the fake,
  401 without a key, 409 before the upgrade with no replica.

## Wave 2a: web Terminal tab
- `hamr.vendor.json`: `@xterm/xterm` js and css plus `@xterm/addon-fit`,
  like htmx, into `ui/static/js/vendor` and `ui/static/css`.
- `ui/ts/term-pane.ts`: a `<term-pane url=...>` element modelled on
  `log-pane.ts`; fit addon, resize frames, "[disconnected]" on close,
  black background in both themes (v0). Built by `npm run ts:build`; the
  `.js` output is generated, never edited.
- `internal/ui/drawer/tile/view.go` (`Tabs()` "Terminal", `TerminalView`),
  `tile.templ` (replica select as the Logs tab, xterm css, `<term-pane>`),
  `internal/web/handler/tile/handler.go` + `views.go`: the tab shows only
  when `can(..., `tile.write`)`.
- CSP (`internal/web/server.go`): `default-src 'self'` covers same-origin
  `wss:`; add `connect-src 'self'` only if the rig console shows a
  violation.
- Test: tab renders for a role with `tile.write`, absent below it.

## Wave 2b: `stackr ssh [tile]`
- New `cmd/stackr/ssh.go`, registered top level next to `logs` and under
  `tile` (`cmd/stackr/cmds.go`). Flags `--container`, `--shell`; tile ref
  through `a.path(c, atTile, tile)` and `scoped(c, true)`.
- `cmd/stackr/app.go`: a websocket dial helper beside `request` (same
  server URL and bearer key, `http.Client{}` without a timeout). A refused
  upgrade prints the response body as the error.
- Raw mode with `golang.org/x/term` (`MakeRaw`, deferred `Restore`), the
  initial size from `term.GetSize`, SIGWINCH in `winch_unix.go` with a
  no-op `winch_windows.go`. Exit with the code from the exit frame.
- Refuse without a terminal on stdin ("needs a terminal; use tile exec"),
  refuse `--json`.
- Tests: the coverage test names `tile.terminal`; no TTY refuses.

## Wave 3: forward backend
- New `internal/service/forward.go`: `Orchestrator.DialTile(ctx, tileID,
  container, port) (net.Conn, int, error)`: `o.replica()`, the replica's
  IP on `env.Network` from the docker inspect, port = `?port=`, else
  `store.Tile.ContainerPort`, else the managed engine's default port (the
  same source the drawer status `Port` uses), else 400 "tile declares no
  port; pass --port". `net.DialTimeout` 5s; slog open and close. A
  redeploy changes the IP; each new connection re-resolves.
- `streams.go`: `Forward()`: dial first, set `X-Stackr-Port`, Accept,
  NetConn bridge with half-close, ping loop. Route `{GET,
  tile+"/forward", "tile.forward", `tile.write`, h.Forward()}` with `.Q("container",
  "port")`.
- Tests: a local listener stands in for the tile; bytes both ways; a
  client half-close still gets the full reply; no port is a 400 before the
  upgrade.

## Wave 4: `stackr forward [tile]`
- New `cmd/stackr/forward.go`, port of v0 `newForwardCmd`: `--port
  [local:]remote` (bare number = local only), `--address` default
  127.0.0.1, `--container`; one websocket per accepted connection; prints
  "Forwarding 127.0.0.1:L -> tile:R (Ctrl-C to stop)" with R from
  `X-Stackr-Port`. A startup probe connection fails fast on a bad tile or
  port. Refuse `--json`.
- Tests: v0's `parsePorts` cases; one local connection tunnels through
  the fake server; coverage names `tile.forward`.

## Wave 5: docs and rig
- `leftovers.md` (both sections built, the no-relay note), `v0-gaps.md`
  (strike "now wanted back"), PROGRESS.md DECIDE 1 and 45 get "reversed,
  see terminal-forward.md", `docs/openapi.json` regenerated.
- Rig: `make release RELEASE=v0.6.0-dev.N`, upgrade; `stackr ssh` into a
  smoke tile (bash and sh images, resize, ctrl-c, exit code); the drawer
  tab in a headed browser; `stackr forward` to a smoke postgres, psql
  through it, idle past 30s, redeploy the tile and reconnect.
- `make test`, `make lint`, `make templint` green.

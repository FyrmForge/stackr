# Blitz fix round: Opus review findings

Status: built, committed in ddaa87e. Was agreed 2026-10-07 (darthvader "go"). Five workers in parallel, then
a verification pass, then rig v0.6.0-dev.15.

Rules for every worker: same as `migration-blitz.md` (no git writes, no
hand edits to generated files, no em dashes, match neighbouring style,
`ponytail:` for ceilings). Touch only the files you own below; a need in
someone else's file goes into `blitz-seams.md` under "Fix round needs".

**Test first.** For every finding, write a test that FAILS on the current
code using the failure scenario given, run it and see it fail, then fix.
Goldens and `caddy.Load` passing proved nothing last time.

Only the host-access worker runs `templ generate`. Nobody regenerates
`docs/openapi.json` except the host-access worker (it changes an API body).

## F1 Forward auth
Owns: `leaf/domain/caddy_auth.go`, `caddy_auth_test.go`, `testdata/
forward_auth*.json`, `leaf/domain/domain.go` (`checkExtras` and a shared
validator), `leaf/domain/caddy.go` (only where `proxy.headers` values and
paths reach Caddy), `domain_test.go`.

1. HIGH. Copy headers the provider omits reach the app as the literal
   `{http.reverse_proxy.header.X}` (Caddy leaves unknown placeholders). Do
   what Caddy's `forward_auth` directive does: in the 2xx handle_response
   route, first DELETE every copy header unconditionally, then SET each one
   only when its placeholder is non-empty (a `vars` / expression matcher per
   header, as `modules/caddyhttp/reverseproxy/forwardauth/caddyfile.go`
   in the Go module cache does; read it). Never skip the delete: a client
   sending `Remote-User: admin` while the provider returns 200 with no
   header must reach the app with NO Remote-User.
2. HIGH. A URL with no path (or "/") leaves the client's path and query on
   the auth request, so `GET /health` asks the provider's health endpoint
   and gets a 200. Always set `rewrite.uri` (`/` when empty). Also fixes the
   body not being dropped (the rewrite's change is what drops it); remove
   the dead Content-Length delete.
3. HIGH. Caddy expands `{...}` placeholders in user strings: a URL query
   `?t={env.DNS_API_TOKEN}` or a copy-header name `{file./etc/x}` exfiltrates
   the proxy's env and files (Cloudflare token, other orgs' TLS keys) and a
   `domain.write` user can set them. One shared validator that refuses `{`
   and `}` applied to every user string landing in Caddy JSON: forward-auth
   URL, copy-header names, `proxy.headers` keys and values, domain path (if
   no regex already restricts it; grep). Route targets get the same check in
   F2 (export the validator).
4. Copy-header names: header-token regex plus `http.CanonicalHeaderKey`.
5. A URL that fails to parse at Build must fail closed (error into Build's
   `bad` path, route left out), not serve the route open.
6. Delete `X-Original-URL` and `X-Original-Method` from the auth request.
7. Order: basic auth BEFORE forward auth (otherwise a stack writer's
   provider harvests visitors' protect credentials from `Authorization`).
8. Dial with `net.JoinHostPort`.
Tests: port the reviewer's harness at
`/tmp/claude-1000/-home-darthvader-FyrmForge-stackr/a5e3e10d-036f-4138-a78a-2104ba405d5a/scratchpad/fa/main.go`
(cases A2 no-path bypass, B placeholder exfil, D client-spoofed Remote-User
with provider omitting it, E lowercase copy header, I X-Original-URL) into
a request-level test that runs a real in-process Caddy with `Build` output,
a mock provider and a mock app (httptest servers). Each case fails before
the fix.

## F2 Routes and host ownership
Owns: `internal/service/domain.go`, `internal/service/route.go`,
`internal/service/domainres.go`, `internal/service/admin.go` (panel_domain
check only), `internal/service/orgconfig.go`, `leaf/route/*`,
`flow/promote/plan.go`, `flow/orgconfig/diff.go`, their tests.

1. HIGH. `AttachDomain` skips `checkRouteHost` for `s.Auto` hosts, and
   `refreshAutoHosts` writes renamed hosts with no check. An org makes tile
   `pve` with an auto host under its own resource and takes the admin's
   `pve.example.com` route. Check the generated host too, on attach and on
   every rename.
2. MEDIUM. A pass-through route on the panel host (`panel_domain`, exact or
   covered by a wildcard) locks the panel out (layer4 wins before HTTP).
   Refuse at route create; refuse a later `panel_domain` change a route
   covers.
3. MEDIUM. `overlap("*.res.x", "res.x")` is false, so a wildcard route sits
   one label above a domain resource and silently takes every auto host
   under it. A resource host refuses, and is refused by, `*.<resource>`.
4. Stack-file resources (plan.go Prepare ~522) and org-file resources
   (`orgconfig/diff.go` ~339, live hosts via `service/orgconfig.go`
   `orgLive`) get the reverse route check as plan blockers.
5. Trailing dot: normalise with the same `CleanHost` before `checkRouteHost`
   in `CreateDomainResource`.
6. Route targets refuse `{` and `}` (F1's exported validator; if F1 has not
   landed it yet, write the same two-character check locally with a
   `ponytail:` to dedupe).
7. Fix `TestSquat`'s expectation that a wildcard covers two labels down
   (Caddy and l4 match one label); fix the `routes_wrapper_order` golden to
   a combination the squat check allows.
Tests: each scenario above, failing first.

## F3 Shares
Owns: `leaf/volume/share.go`, `share_test.go`, `flow/deploy/mount_share.go`
and test, `flow/promote/promote.go`, `internal/service/share.go`,
`docker/volumes.go`, `docker/containers.go` (labelFilter only),
`dockerfake/fake.go`.

1. MEDIUM. `SweepShares` removes any share volume no container holds right
   now, server-wide; a deploy's freshly made share volume sits unheld during
   the image pull, gets removed, and Docker silently recreates it as a plain
   local dir (no opts, no label): the app writes to local disk forever.
   Decide from rows instead: compute the volume names every tile row still
   references (needs the name to be computable from rows, see 3) and remove
   only share volumes NOT in that set. Run it after tile removal (scoped to
   the shares the removed tiles named is fine as long as the keep-set is
   from rows) and in `UpdateShare`. No cross-org names in job logs.
2. Only Docker's in-use / conflict error means "held"; other errors
   (daemon down, ctx cancelled) return as errors. Add a typed check in
   `docker/volumes.go` (`cerrdefs.IsConflict` or the in-use text).
3. `ShareVolumeName` hashes `PasswordRef` (the reference), never the
   expanded password. Rotation of the secret value does not need a new name
   (ponytail note).
4. `shareBind` looks a share up by slug only (a UUID passes slug.Valid and
   dodges the delete-while-mounted guard).
5. Revert the empty-value `labelFilter` overload in the wrapper and the
   fake (a Teardown with an empty id would match every container). Add an
   explicit key-only filter helper if the sweep still needs one.
6. Docs line in `migration-blitz.md` W2: `docker volume inspect` shows the
   SMB password to anyone with Docker access on the host (local driver).
Tests: the concurrent race as a unit (deploy-made volume of a live tile row
survives a sweep), error typing, slug-only lookup.

## F4 Config files
Owns: `flow/deploy/mount_files.go` and test, `internal/git/git.go`,
`internal/service/orchestrator.go` (DataDir only).

1. HIGH. The rendered folder is keyed on (tile, commit) only, so editing
   `files:` lines in the drawer or API (same commit) reuses the old folder:
   swapped content, a new line binds a missing path that Docker creates as
   an empty root-owned dir, a new `:template` stays raw. Key the folder on
   commit + a hash of the tile's `files:` lines + a NON-SECRET params
   version (the max `updated_at` of the params the templates can see; the
   params store has `updated_at`). Never hash secret values (the path shows
   in `docker inspect`).
2. MEDIUM. `IsDir` swallows git errors as "not a dir"; on the reuse path a
   folder line then binds a missing path. Return the error from `IsDir`;
   on the reuse path take file-or-folder from `os.Stat` of the written slot.
3. MEDIUM. When the commit is not in the clone, say so ("the config repo
   clone lacks <sha>; run a promote to fetch it"), not "not in the repo".
4. Local dev: `filepath.Abs` on `DataDir` once in `service.New`; a relative
   bind source is read by Docker as a volume name.
5. Defence in depth: refuse a tree entry whose `rel` is not
   `filepath.IsLocal`.
6. No fsync before rename: add `Sync` on files and the dir, cheap.
Tests: drawer-style edit at the same commit remounts the new content; a
missing commit errors with the clone message; a `..` tree entry refused.

## F5 Host access
Owns: `internal/service/hostgrant.go`, `leaf/hostgrant/*`,
`api/handler/v1/hostgrant.go`, `internal/api/hostgrant_test.go`,
`cmd/stackr/hostgrant.go` and test, `ui/drawer/stack/*`,
`web/handler/canvas/stack.go`, `leaf/tile/check.go`, `docs/openapi.json`
(regenerate once at the end), `templ generate`.

1. MEDIUM. Approve grants the union of every waiting ask at POST time, not
   what the admin saw: an owner can slip `host:/:/h` or privileged in
   between. The drawer and CLI send the exact set shown; the server refuses
   with 409 "the ask changed; review it again" if its pending set differs.
2. LOW. `Parse` splits on ", " so a device line `/dev/null, privileged`
   smuggles `privileged` into the shown and approved set. Refuse `,` in
   device and host mount lines (`ParseDevice`, `ParseMount`).
Tests: the TOCTOU (second ask parked after the GET, POST with the old set is
409, nothing granted); the comma line refused at save.

## Recorded, not fixed (append to leftovers.md section H)
- Promote or rollback that parks at deploy time after `SetRelease` moved the
  env pointer finishes Done on resume without redeploying (also an older
  `errs.Unset` gap).
- `ParamSet` never auto-requeues a covered host-access park; `Requeue` can
  resurrect a superseded job.
- Slice provisioning and earlier volume lines run before the host-access gate
  inside `resolve`.
- Cron, function and one-shot runs fail instead of parking on host access.
- A grant covers the whole stack, PR envs included; a stopped privileged
  container restarts without a check.
- Rendered template folders of deleted tiles and of cron tiles are never
  pruned; template refs are invisible to plan and dependency ordering; no
  `files:` dst collision check at save time.
- Route create check-then-insert race; route vs tile attach race; browser
  connection reuse with broad pass-through certs (docs).

## Verification pass (one worker)
Replay every finding's scenario above against the fixed tree (not just
`make test && make lint && make templint`, which must also be green), then
rig `v0.6.0-dev.15` with `scripts/qa/blitz.sh` plus: push a config reload
(any proxy sync) while a pass-through route exists and re-check the cert
serial; an SMB share with a wrong password and read the job log for a leaked
`password=`.

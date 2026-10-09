# Param redeploy scope: build plan

Status: built 2026-10-09, on the rig as v0.6.0-dev.28, QA passed.

## Problem
Setting or deleting one param redeploys every running tile under the scope
(`SetParams`, `DeleteParam` in `internal/service/params.go` call
`scopeTiles` and redeploy all of it). An org param restarts every tile in
the org. The org file apply (`orgconfig.go`) re-sends every org param on
each apply, so an unrelated org file change restarts everything too.

## Fix
1. Changed keys only. Read the scope's values before and after the write;
   the changed set is the `collection.name` keys whose value or kind moved
   (added, removed, edited). Nothing changed: no redeploy.
2. Readers only. Of the scope's running tiles, redeploy those that read a
   changed key:
   - a ref in env values, volume lines or the command:
     `params.c.n` for an env or stack scope, `org.params.c.n` for org;
   - a share mount whose share user or password refs `org.params.c.n`;
   - a `tile.<slug>.*` ref to a slice whose `provision_from` reads the key;
   - any `:template` files line: the template lives in the config repo, so
     the tile counts as a reader of every key (conservative).
3. One place: a `reads(tile, key)` check beside `scopeTiles`; `SetParams`
   and `DeleteParam` filter through it. Org settings changes keep
   redeploying the whole scope (they change limits).

## Not doing
- Env override shadowing: a stack param a tile reads but its env overrides
  still redeploys that tile (harmless restart, rare).
- Reading template files from the repo to find their refs.

## Files
- `internal/service/params.go` (changed keys, filter)
- `internal/service/internal/leaf/params/ref.go` (a `Reads(s, key)` helper
  over `Refs`, if `Refs` alone is not enough)
- `internal/service/params_test.go` or `orchestrator_test.go` (one test:
  two tiles, one reads the param; set it; only that one redeploys; set it
  again to the same value; nothing redeploys)

## QA
Rig: two tiles in one env, one reads `params.app.x`. Set `app.x` in the web:
only the reader restarts. Set an unread param: nothing restarts.

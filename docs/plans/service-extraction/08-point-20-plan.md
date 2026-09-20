# Point 20 — plan, and why it is much smaller than specced

Written 2026-09-19 after measuring. **The premise in 06-points-18-20.md is
stale.** It was written before points 7-16 extracted the services, and the
extraction already fixed most of what point 20 was for.

## What the spec assumed

> repo.Tile is one struct holding both (GitURL/ImageRef/Volumes next to
> Status/ImageDigest/HomeNode/LatestDigest). Model split, not a read refactor.

The worry behind it: anything that can write a tile can write config AND
state, so the config apply can stomp observed state and the image watcher can
stomp declared config. Nothing structural stops either.

## What is actually true today

**The store already splits the write paths.** `UpdateTile` writes 40-odd
config columns and NOT ONE state column — no `status`, no `image_digest`, no
`latest_digest`, no `home_node`, no `last_run_at`/`last_status`/`last_output`
(sqlite/tiles.go). State has five narrow setters of its own:

    UpdateTileStatus      status
    SetTileHomeNode       home_node
    SetTileImageDigest    image_digest
    SetTileLatestDigest   latest_digest
    (cron run recorder)   last_run_at, last_status, last_output

So config-as-code, which writes through `UpdateTile`, **already cannot reach
the state path**. That is the guarantee point 20 was asking for, and point 13's
managed gate is already structural in the direction that matters.

**And every state write goes through the narrow setter.** Audited all seven
assignment sites outside the store: `managedinstance.setStatus` and
`tilelifecycle.setStatus` both call `UpdateTileStatus` and then update the
in-memory copy; `imagewatch` calls `SetTileImageDigest`/`SetTileLatestDigest`
the same way. `tile.go` and `envops.go` set state fields on a struct headed for
`CreateTile`, which is an insert and legitimately writes the whole row.

No silent no-op found. Nothing writes state through `UpdateTile` today.

## What is left, and it is one hazard

`repo.Tile` still CARRIES the state fields, so a future caller can write

    t.Status = "running"
    store.UpdateTile(ctx, t)

and it compiles, runs, returns nil, and persists nothing. A write that looks
like it worked. That is the remaining hole, and it is a type problem, not a
model problem.

## Options

**A. Embed (RECOMMENDED).** Declare the fields once, in two structs, and embed
both in `Tile`:

    type TileConfig struct { Name, ImageRef, GitURL, Volumes, ... }  // ~40
    type TileState  struct { Status, ImageDigest, LatestDigest, HomeNode, ... }

    type Tile struct {
        ID      string `db:"id"`
        StackID string `db:"stack_id"`
        TileConfig
        TileState
    }

    UpdateTile(ctx, id string, cfg TileConfig)   // cannot express a status
    SetTileState(ctx, id string, st TileState)   // cannot express an image ref

The write paths become unable to name each other's columns, at COMPILE time,
and no field is written down twice. Reads are unchanged: one `Tile`, spec and
status together, which is what the deferred read-split section already decided
and what k8s and swarm both do.

Field ACCESS is unchanged too — Go promotes embedded fields, so `t.Status` and
`t.ImageRef` still compile. All 317 `repo.Tile` references and all 39 templ
views are untouched.

Verified against the sqlx in go.mod (v1.4.0): named exec, `Get` scanning, and a
config-only `UPDATE` all resolve `db:` tags through embedded structs.

  - **Cost: 258 composite literals**, because Go has no flat literal for an
    embedded field. `repo.Tile{Name: "x"}` becomes
    `repo.Tile{TileConfig: repo.TileConfig{Name: "x"}}`. 237 are in tests, 21
    in production code.
  - That cost is the safe kind: every one is a COMPILE ERROR. The compiler
    finds all of them, none can be missed, and none can silently change
    behaviour.

**B. Guard it.** Keep one flat struct. Make the silent no-op loud: `UpdateTile`
compares the state fields against the stored row and errors if a caller changed
one, naming the setter to use instead. One method, one test, no churn.

  - Closes the actual hazard, and leaves A available later — A is a pure type
    change, so nothing here has to be undone to do it.
  - Runtime error, not compile error, and one extra read per update.

**C. A separate TileConfig with a conversion** (`t.Config()` projecting a flat
`Tile` into a narrow struct). REJECTED: the ~40 config fields end up written
twice, in the struct and in the projection, and they have to stay in step with
the SQL. A field added to one and forgotten in the other silently stops
persisting — the same class of bug this is meant to remove, moved somewhere
harder to see. Embedding gets the same guarantee with the fields declared once.

**D. Two tables.** REJECTED. Storage is not the problem; the SQL already
separates config and state writes. A migration on live data buys nothing.

## Recommendation

**A, sequenced after point 19.** It is the correct-by-construction answer and
the only one with no duplicated field list.

Point 19 rewrites the same handlers the literal churn touches, so doing A
first means editing them twice.

If the 258 rewrites are not worth doing now, **B is the better trade today**:
the structural guarantee point 20 wanted already exists in the store, so what
is left is only that breaking it is quiet, and B makes it loud for one method's
worth of work. A stays open afterwards.

## What this does NOT change

The deferred half of point 20 stays deferred: splitting the READS into
`GetConfig()`/`GetState()` was already superseded, because every orchestrator
(k8s, swarm) returns spec and status in one read. Only the write split was
ever live, and most of it turns out to be built.

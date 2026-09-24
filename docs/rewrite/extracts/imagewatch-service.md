# Extract: `service/imagewatch.go` → `service/internal/flow/imagewatch`

- **Source:** `service/imagewatch.go`, `service/imagewatch_test.go`
- **Commit:** `c2423f0`
- **Taken:** nothing. Reference only — the shape of the sweep, described below.
- **Cut:** the notify/auto policy and the notifier, the three auto-deploy branches, the `cron_runs` check history, the in-memory error edge guard, the local-docker baseline, the ghcr.io-only credential special case.
- **Cuts belong to:** nowhere. The plan replaces them with releases + the promote job; the credential lookup is the only piece that needs a home in `flow/imagewatch`.

## What the old service did, per step

**Scan.** `Run` listed *every* tile in the database and filtered in memory:
opted in (`update_policy` is `notify` or `auto`), not paused, not a volume,
either managed or `source_type == "image"`, and an `ImageRef` the registry
client can parse (digest-pinned refs are refused there, so a pinned tile drops
out of the scan by itself). One flat list, no environments — a tile was a single
thing with one running digest, so there was nothing to fan out over.

**Dedupe.** Two maps per sweep, keyed by the image ref: `digests` caches the
answer so N tiles on `postgres:16` cost one registry call, and `failed` caches
the *error* so a broken ref also costs one call — every later tile on that ref
takes the cached failure without touching the network. The failure half is the
non-obvious one and is worth carrying over; the plan's step 1 only spells out
the success half.

**Interval.** Two settings rows: the cadence in minutes and the RFC3339 stamp of
the last sweep. `Run` was a janitor tick called on the janitor's own schedule,
and it returned `0, nil` immediately unless the cadence had elapsed since the
stamp — a "should I run" check, not a timer. Empty cadence read as the default
(5 minutes); `"0"` meant the watch is off, and was distinct from empty. Writing
the cadence refused anything that was not a whole non-negative number rather
than coercing it through `Atoi` — that refusal is the only thing the test file
covers (a bad value used to flash on the settings page and silently leave the
old value in place).

**Compare.** Per tile, against two columns. `image_digest` is what is running,
`latest_digest` is what the registry last said. The sweep is edge-triggered on
`latest_digest` changing: same digest → return, nothing written. Changed →
write `latest_digest`, then if it now equals `image_digest` return anyway (the
registry caught up with what is already running; the chip clears, this is not a
new version). Only a digest that differs from both is an update. There was one
bootstrap wrinkle: a tile first seen with no `image_digest` at all seeded that
column from the *local docker image*, falling back to the registry's answer,
and stayed silent that round — so a tile that had been running something stale
since before the watch existed still surfaced an update on the next sweep.

**Cache.** On the tile row, both columns, so the UI read the badge from the tile
and never called a registry.

**Apply.** Branched on the policy. `notify` pushed a notification. `auto` had
three genuinely different deploy paths: a managed tile was recreated through the
managed-instance service, a cron or function tile has no long-running container
so it only pulled the image and re-baselined `image_digest`, and everything else
went through a normal deploy trigger. A refusal from the deploy service's
upper-environment gate was recorded as success ("new image available"), not as
an error, because an environment that only takes promotions is supposed to
refuse. Registry errors took a different path: recorded once per distinct
message (an in-memory `lastErr` map, so a restart repeats one notification per
broken tile) and pushed as a "check failed" notification. Both outcomes also
wrote a `cron_runs` row under a `watch:<tileID>` ref, which bought check history
and retention for free by reusing the cron table.

## How the plan's ten steps differ

1. **The scan is env-shaped, the cache is not on the tile.** The plan scans
   image tiles across every env and dedupes by `registry/repo:tag` onto an
   *image row*; the answer and any registry error are cached there (steps 1, 4,
   5). The old two-column-per-tile baseline goes away.
2. **Comparison is against a release, not a column.** The plan compares with
   each env's current release, and every release records a digest — so there is
   no "what is actually running" question and no local-docker bootstrap.
3. **A change writes releases, not deploys.** Step 6 makes one release row per
   env that takes releases directly (branch env or bottom rung): the env's
   current release with that tile's digest swapped. `auto` then lands it through
   the ordinary promote job and `manual` waits for the chip's button (step 8),
   so the three deploy branches collapse into one path that already knows about
   pull-by-digest, the health gate and rollout.
4. **No notify policy, no notifier.** The chip is the whole signal (step 7).
   Both notification kinds and the error edge guard go with it — the last error
   lives on the image row and simply retries next round (step 5).
5. **Mode 2 is entirely new.** The old sweep only ever asked "what is behind
   this tag". Tag policy (list tags, semver sort, pick newest match) has no
   counterpart anywhere in this file.
6. **Check now.** The plan adds on-demand checks per tile and per stack next to
   the timer (step 2); the old code had only the cadence, and the cadence
   default changes from 5 minutes to 60.
7. **The cron-table reuse is gone**, with no replacement asked for: step 5 wants
   the error on the image row, not a run-history row per check.

## Notes for the builder

- Carry the `failed`-map dedupe across: one broken image should cost one
  registry call per sweep, not one per tile using it.
- Keep the "registry now matches what is deployed → not an update" case. With
  releases it is the cheaper test the old code had to fake: compare the polled
  digest with the env's current release, and only a difference is news.
- Keep interval validation as a refusal, not a coercion, and keep empty
  (= default) distinct from `0` (= off). That distinction is the one thing the
  old test asserts, and it is a real setting people set to zero.
- The credential lookup is the only piece of this file with no plan home: the
  old one was `ghcr.io` → GitHub App, else anonymous. v1 needs a real per-tile
  pull credential resolved here and handed to `registry.Digest`.

Size: source 337 lines, extract 103 lines.

# Point 18 — build plan

Agreed with the dev 2026-09-19. Follows the order in `06-points-18-20.md`
("Order of work"); step 1 of that order is already landed
(`routeverb_test.go`). This plan is steps 2 and 3.

## Decisions taken before starting

- **Point 18 proper is green-lit.** The original brief banned it; the dev
  lifted that on 2026-09-19.
- **Personal routes stay out of the verb table.** `/account/*`,
  `/notifications/*`, `/orgs/switch`, `/cli/authorize` act on the caller, not
  on an org. They live in their own Echo groups that never mount the
  middleware, so the exclusion is structural, not a list. The test's
  `personal()` allowlist stays as the mirror of those groups.
- **Pass-through reads move** (point 19), and handlers lose the store. They
  KEEP `repo.X` as view types — banning the import outright is a type
  migration across 150 handler files and 39 templ views, and it is point 20's
  job, not this one.
- **Scopes are already route-level and orthogonal.** `op()` in
  `api/v1/v1.go:210` registers the scope beside the route and feeds the
  OpenAPI spec from the same call. Deleting role gate helpers does not touch
  scope enforcement. `AccessService.RequireScope` has no callers today and
  stays unused unless a panel path ever needs both.

## The risk this plan is shaped around

The route-verb test asserts a route names a KNOWN verb. It cannot tell
`VerbStackWrite` from `VerbOrgWrite`. Put a member-level verb on an
owner-level route, delete the helper, and the test is green, `go vet` is
green, and the endpoint is open one rung.

That is the silent failure the dev banned this work over, and the gate
helpers are the only record of what is enforced today — so capture it as data
BEFORE deleting them.

## Steps

### 2a. Capture the enforced level (new, before any deletion)

For each of the 271 mutating routes, record the level its current gate helper
enforces today: read / write / owner / admin. Lands as a table-driven test,
`routelevel_test.go`, asserting `service.LevelOf(verbFor(route)) == captured`.

Deliberate changes — the "higher level wins" cases already decided in points
7, 10 and 11 — appear as an explicit diff in that table with the point that
decided them, never as a silent move.

### 2b. TenancyOf

One resolver: given a route's addressed thing, which org owns it. Replaces
the org-walk baked into each of the ~35 helpers. Handles both addressing
forms (by id, and by `org:stack:env:tile` path) — the API has
`tileByPath`/`stackByPath`/`orgByPath`/`envByPath` and the panel has 13 slug
lookups of its own.

### 2c. Verbs on routes + requireVerb, mounted ONCE

`Route.Name = string(service.VerbX)` is a string and wires nothing on its
own. `requireVerb` mounts at ONE point covering everything authenticated, not
per-group — ~30 mount points would be ~30 places a route can carry a correct
verb and still be ungated.

Fail-closed: a mutating route whose `Name` is not a known verb is refused.
The two skip buckets (unauthenticated, personal) become the middleware's own
knowledge rather than a test's allowlist.

Surface error policy is NOT flattened: panel answers 404 for tenancy/owner
refusals, api answers 403. `AccessService` already returns `ErrNotFound` vs
`ErrForbidden`; the HTTP mapping stays per surface.

### 3. Delete the gate helpers

~35 helpers, ~200 call sites. Only after 2a is green, because 2a is the thing
that catches a downgrade.

## Not in this plan

- **Point 20 is still blocked.** It needs `repo.Tile` split into config and
  state. Option 1 above keeps `repo.X` as the view type and does not unblock
  it. "Finish everything" means 18 + 19.
- **Point 15's two leftovers stay reserved for the dev**: scope grants at
  mint time, and deactivation not revoking share links. Raise when 18 lands.

## Checks per chunk

`go vet`, `make templint`, full suite, deploy to the VM. `make lint` is
broken by a go1.26/go1.27 golangci-lint mismatch — pre-existing, not chased.
Judgement calls go into `05-assumptions.md` as they happen. Nothing staged.

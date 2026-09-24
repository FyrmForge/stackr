---
name: handler-audit
description: Read-only audit of stackr HTTP handlers (internal/web/handler, internal/api/handler) against the layering rules in AGENTS.md. Reports every store call, Docker call, domain decision and auth check found inside a handler or its .templ, with file:line and the service method that should own it. Use when asked to audit handlers, check a handler package, or before merging a change that touches handlers.
---

# handler-audit

Spot check for AGENTS.md "Layering rules". Handlers bind input, check form
shape, call one `service.Orchestrator` method and render. Anything else in a
handler is a finding. **Read-only: never edit a file.**

## Run

1. Target: the package(s) the user names, default both trees. From the repo
   root:

   ```bash
   .claude/skills/handler-audit/audit.sh                       # both trees
   .claude/skills/handler-audit/audit.sh internal/web/handler/home   # one package
   ```

   It greps `*.go` (skipping `*_templ.go`, `*_test.go`) and `*.templ` and
   prints candidate lines under five headings. It changes nothing.
2. Open each hit and decide: finding or false positive. Drop false
   positives (a word match in a comment, a form-shape check, an
   `if view.CanDeploy` in templ that only reads a bool the service set).
3. Report.

## What counts

| Category | Finding when a handler... | Not a finding |
|---|---|---|
| store | imports `internal/repo` or the store, holds a store field, runs SQL | calling an orchestrator method |
| docker | imports or calls Docker, proxy, git or s3 code | — |
| domain decision | computes a status, picks a default, normalises data the domain cares about (lower-casing an email), applies a rule needing a lookup, generates IDs or timestamps | form shape: required, type, length, two fields of one form agreeing |
| auth | checks role, membership or "may this user", reads the subject to decide, validates or deletes sessions itself | reading the subject ID only to pass it to the orchestrator for audit fields |
| templ decision | `.templ` has `if`/`for`/`switch` whose condition computes (`==`, `len(`, `&&`, comparisons on domain fields) | `if view.X` on a bool/field the service or handler view mapping already set; ranging over a view slice |

Session plumbing (setting/clearing the cookie after the orchestrator says
the login is good) is transport and may stay; deciding validity is auth and
belongs in middleware or the orchestrator.

## Report format

Plain text, one line per finding, grouped by category:

```
store
  internal/api/handler/health/handler.go:22  h.store.Health(...)  -> Orchestrator.Health(ctx)
auth
  internal/web/handler/auth/login/handler.go:88  sessionManager.ValidateSession  -> middleware / Orchestrator.Logout(ctx, token)
templ
  internal/web/handler/x/x.templ:14  if len(v.Items) == 0  -> view field Empty bool set by the handler's view mapping
```

For each finding name the owner: an `Orchestrator` method (existing or the
one that should exist), `authz.can` via middleware, or a view-struct field
the service returns. End with a count per category. Empty findings: say
"no findings" per category; that is a valid result.

# Fixing silent secret declassification

Standalone bug, found during the business-logic sweep. Not part of a wave;
fix it on its own.

## What happens

A write that sets a plain value over an existing **secret** turns that row into
plaintext. Nothing refuses it and nothing logs it as a declassification. The
value then shows unmasked in every listing, to anyone without `secrets:read`.

`VariableService.write` (`service/variable.go:156-203`) builds the row as:

```go
s.store.UpsertVariable(ctx, &repo.Variable{... Secret: w.Secret ...})
```

`w.Secret` comes from the request. The only pre-read it does
(`:170-176`) captures `live[v.Name] = v.Value != ""` — whether a value exists,
never whether the row is a secret. The stored `Secret` flag is never consulted.

### How it is reached

**API** — `handlers/api/v1/variables.go:142-143` passes `Secret: e.Secret`
straight from the JSON body. `varEntry.Secret` is a plain `bool`, so an omitted
field is indistinguishable from `false`. Any ordinary "update this variable's
value" script declassifies the row. Route is `Replace`, so this is the
`stackr vars set` path too.

**Panel** — three of the five call sites read a checkbox, so submitting the
plain-var form for a name already held as a secret does the same:

| site | scope | secret flag |
|------|-------|-------------|
| `handler/org/settings.go:396` | org | `c.FormValue("secret") != ""` |
| `handler/project/handler.go:2100` | stack | `c.FormValue("secret") != ""` |
| `handler/project/handler.go:2347` | env | `c.FormValue("secret") != ""` |
| `handler/project/handler.go:810` | stack | hardcoded `true` |
| `handler/app/handler.go:997` | tile | hardcoded `true` |

The last two are the dedicated "fill a waiting secret" forms and are safe.

## The rule already exists — in the wrong place

`config/stackconf/apply.go:1853-1875`, `applyVars`, with the reasoning written
out:

> applyVars writes the file's vars: onto the stack and env owner rows. Plain
> values only; **a name already stored as a secret is left alone**, the plan
> already errored on it and overwriting would drop a credential into a row the
> file claims is public.

and implemented:

```go
secret := map[string]bool{}
if cur, err := store.ListVariables(ctx, ownerKind, ownerID); err == nil {
    for _, v := range cur { secret[v.Name] = v.Secret }
}
for name, val := range vars {
    if secret[name] { continue }
    ...
}
```

The config applier does **not** route through `VariableService` — it calls
`a.Ops.Vars.Upsert` directly — so its copy protects only config-driven writes.
The panel and the API, which is where a human declassifies a credential by
accident, have no such check.

This is the sweep's thesis in one bug: one rule, correctly reasoned, owned by
whichever caller happened to need it first.

## The fix

One choke point. `Set` (`:90`) and `Replace` (`:100`) both go through
`write` (`:156`), and `Replace`'s delete loop runs only after `write` returns,
so a refusal there aborts cleanly with nothing deleted.

Three edits:

1. **Hoist the existing `ListVariables` read above the validation loop.**
   It currently sits between the two loops (`:170-176`). `write`'s doc comment
   promises *"a set that is going to be refused must not half-apply"*, and that
   promise only holds for checks made in the first loop. Capture
   `wasSecret[v.Name] = v.Secret` alongside the existing `live[v.Name]`. Same
   single query, no new reads.

2. **Refuse the downgrade in the validation loop:**

   ```go
   if wasSecret[w.Name] && !w.Secret {
       return svcerr.Invalidf(w.Name,
           "%s is stored as a secret; tick Secret to update it, or delete it first", w.Name)
   }
   ```

3. **Leave `applyVars` alone for now.** It skips rather than refuses, which is
   right for a file reconciler — an apply must not fail because one name was
   promoted to a secret by hand. Removing that copy belongs to the variables
   wave, when config writes route through the service. Note the divergence in
   a comment on both sides so it reads as deliberate.

### Refuse, not skip

Config skips because a reconciler runs unattended and must be idempotent.
An interactive write is a person who typed a value and pressed save; silently
discarding it is worse than refusing it. Fail-closed and visible.

### No DTO change

Refusing means the existing row stays the source of truth, so `varEntry.Secret
bool` can stay as it is. `Secret *bool` would only be needed to let a caller
declassify *deliberately* over the API — and that would be two surfaces of
work, because the panel has the same absent-vs-false collapse in its checkbox.
Not proposed here.

## Open question for darhvader

At org/stack/env scope the panel shows **one form with a Secret checkbox**, so
unticking it is a deliberate user act. A refusal is therefore visible and
explainable — but it also means there is no way to declassify from the panel at
all any more, short of delete-and-recreate.

Is delete-and-recreate the intended path, or should there be an explicit
"make this public" control? The fix is the same either way; the second just
adds a follow-up.

## Tests

- `write` refuses a plain write over an existing secret, at each of the four
  owner kinds.
- The refusal is raised before any upsert — a three-entry batch whose second
  entry is the offender writes none of the three.
- `Replace` deletes nothing when `write` refuses.
- A write carrying `Secret: true` over an existing secret still succeeds
  (an ordinary value update).
- A write over a name that does not exist yet is unaffected.

## Tracked separately

`varNameRe` (`service/variable.go:28`) accepts `9FOO` and `MY.VAR`, which
`envVarRe` in stackconf can never hold — a variable created over the API can be
unrepresentable in config-as-code. Same file, different bug. Not folded in.

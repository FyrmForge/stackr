# Plan: basic auth review fixes

Status: built 2026-09-17, tests and lint green, not rig-verified.

Working rules: discuss first, one point at a time, no code without a go, no
git writes, no edits to `*_templ.go` or `output.css`, terse UI copy.

## Why

A review of the plan 48 work (basic auth through the settings cascade) found
six problems. Five are in how `internal/stackrd/infra/proxy/protect.go` turns
the cascade into a Traefik basicAuth entry and how the cascade is saved and
read. One is a false "not set" on the stack variables page.

## Points

### 1. Protection fails open on a store error

`settings.Levels` (`internal/stackrd/config/settings/settings.go`) drops every
store error. A cancelled request or a DB hiccup returns fewer levels, `Protect`
resolves false, and `proxy.WriteApp` writes the route with no auth. It stays
public on disk until the next Resync.

Fix: `Levels` returns `([]Level, error)`. `Chain` and the `For*` helpers keep
their signatures and keep swallowing (a limit falling back to a built-in on a
DB error is not a security problem). One new `settings.TryTile(ctx, store, t)
(Resolved, error)` for the proxy. `basicAuthEntry` returns `(string, error)`;
`WriteApp` returns the error and leaves the existing route file untouched.

Direct `Levels` callers that gain an error return: `api/v1/settings.go`
(one helper), `project/handler.go` (two page handlers).

Files: settings.go, protect.go, proxy.go, api/v1/settings.go,
project/handler.go.

### 2. `hashPassword` returned `"!"` when bcrypt failed

Traefik treats an entry that is not a recognised hash as plain text, so
`admin:!` means the password is `!`. Reachable with a `${{ }}` secret over 72
bytes.

Fix: route to the lock hash from point 5. No sentinel strings.

### 5. The lock path mints a new random password on every render

Every settings save rewrites the locked route with new bytes, Traefik reloads,
and the `hashes` map grows for ever.

Fix: one package-level lock hash, bcrypt of 24 random bytes computed once
(`sync.Once`). Same bytes every render, matches nothing anyone knows. Points 2
and 5 share it.

Files: protect.go only.

### 3. The API returns inherited passwords

`api/v1/settings.go` `protect_password` uses the resolved getter, so reading
stack settings returns a password set at the org. The templ deliberately
shows `inherit` instead.

Fix: the resolved getter for `protect_password` returns `""` when the
level's own override is nil; `source` still says where it comes from.

Files: api/v1/settings.go.

### 4. protect on with a user and no password saves fine

`settings.Merge` validates nothing. Typing a user and leaving the password
blank at any cascade level resolves to an empty password and locks every URL
under it (after point 5: locked with a stable unknown password, one log
line). The tile form already refuses this with a 400.

Decision: `Settings.Check() error` ("protect needs a user and a password"),
called by the five save handlers (api/v1, org, server, stack, env) before the
store write, 400 like the tile. Rejected: Merge silently dropping the user
(edits what the operator typed), and leaving it (a lock nobody asked for
with only a log line to explain it).

Files: settings.go plus the five handlers.

### 6. A per-env secret still shows as "not set"

`unsetSecrets` in `project/handler.go` subtracts stack and org variables. The
resolver (`varref.scopeVar`) reads the consumer's env row first, so a secret
set only per env is fine at deploy time but red on the page.

Fix: pass the env variable lists too and report a name only if some env is
missing it; the row says which envs. Needs `ListVariables` per env on the
stack variables page.

Decision: build it. `unsetSecrets(cp, stackVars, orgVars, envVars
map[string][]repo.Variable)`; a name is unset when no stack or org row has it
and at least one env lacks it. `stackconf.Input` gets no new field: the row
text stays "not set", the drawer already links to the env pages. Rejected:
parking it, a red "Will not deploy" that is wrong is worse than none.

Files: project/handler.go (one `ListVariables` per env on the variables
page).

## Tests

Every point leaves one test that fails without the fix.

- 1: `TestWriteAppFailsClosedOnStoreError` in `proxy/writeapp_test.go`. A
  `testdb.New` store with the tile's stack settings `protect: true`, write
  once, then write again with a cancelled context: `WriteApp` returns an
  error and the file on disk still carries `basicAuth`. Plus
  `TestLevelsReturnsStoreError` in `settings_test.go` (cancelled ctx).
- 2+5: extend `TestWriteAppBasicAuth`: a 100-byte password locks (the hash
  does not verify against it, and is a bcrypt hash, not `!`); two lock
  renders produce identical bytes.
- 3: `TestSettingsPasswordNotInherited` in `api/v1/patch_test.go`: org sets
  user+password, GET stack settings returns `protect_user` resolved and
  `protect_password` empty with source `org`.
- 4: `TestCheckProtectPair` in `settings_test.go` (user only, password
  only, both, neither, off with user only). One handler test in
  `api/v1/patch_test.go` expecting 400.
- 6: `TestUnsetSecretsPerEnv` in `project/vars_test.go` (new):
  declared secret set in one of two envs is unset; set in both is
  not; set at stack is not.

## Order

1, 2+5, 3, 4, 6. Each point is one change; 2+5 are one edit. Lint and the
full suite after each.

## How it was built

Matches the decisions above, with these details worth knowing:

- `Chain` keeps its signature and swallows; `TryChain` is the erroring one
  underneath it, and `TryTile` is what the proxy calls. Everything reading
  limits, node groups or concurrency is untouched.
- `WriteApp` returns the cascade error before it touches the file, so the
  route already on disk keeps its `basicAuth` middleware.
- `lockHash` is a `sync.OnceValue` bcrypt of 24 random bytes. Both lock paths
  (no user or password, and a password bcrypt refuses) return it, so a locked
  route renders identical bytes every time.
- `Settings.Check` refuses half a pair only. Protect on with the credentials
  inherited from a level above is legal and still saves.
- Config-as-code needed the same gate. `defaults:` reaches the store through
  `DefaultsConf.SettingsJSON()`, which never goes near `settings.Merge`, so
  an applied file could mint the locked-out state the five handlers now
  refuse. `DefaultsConf.Check` is called from `stackconf.Parse` (stack level
  and each environment) and from the org file's parse.
- `Resync` logs and continues past a tile whose cascade will not read, and
  reports the failures at the end. Returning on the first one would stop
  every later tile from syncing, at boot most of all.
- `unsetSecrets` takes `map[envID][]Variable`. A name is covered when every
  environment has it; an environment with no variables still gets an entry,
  since that is exactly the one that leaves a secret unresolved.

## Verified

- `go test ./...` and `make lint` (0 issues).
- Points 1 and 2 checked against the old behaviour: reverting
  `basicAuthEntry` to `settings.ForTile` and `hashPassword` to `"!"` makes
  `TestWriteAppFailsClosedOnStoreError` and `TestWriteAppBasicAuthLock` fail.
- Not run on the rig. Nothing here changes the install path.

### 7. The password was rendered back into the form (added 2026-09-17)

Not one of the six. `components/form/protect.templ` set the input's `value`
to the level's own password, so anyone who could open an org, stack, env or
server settings page read it in the HTML, and the API's `own` value handed
it to any read-scope key.

Decision: the password never leaves the store.

- The input is `type="password"` with no value; the placeholder says
  `unchanged` when the level has one, `inherit` when it does not.
- `Merge` therefore reads a blank `protect_password` as "leave it alone".
  Clearing the *user* clears both, which is the only way a form that cannot
  show the stored value can give the pair back to the level above.
- The API returns `"set"` as `own`, never the password, so a client can still
  tell a level that sets one from a level that inherits.

Rejected: a hidden "the password is unchanged" marker field (a second source
of truth, and a client that forgets it silently wipes the password), and
leaving the value rendered behind a write-access check (the page is readable
by anyone who can view settings).

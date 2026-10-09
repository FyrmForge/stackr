# Roles and scoped keys: build plan

Status: built 2026-10-09, on the rig as v0.6.0-dev.24.

## Decided with darthvader (2026-10-09)
- Three org roles on today's verb table (`internal/authz/authz.go`
  `verbLevels`, unchanged): viewer (read), member (write: deploy, edit stacks,
  envs, tiles, params incl. secret reveal, terminal and forward, backups,
  connectors), owner (member plus people, org settings, registry
  credentials, domains, shares). The stackr admin stays above, separate.
- An API key has a ceiling (viewer, member, owner; admin only for admins)
  and optionally one stack. It does the lower of its ceiling and its user's
  live rights. A ceiling above the minter's role is refused. A drop below
  member (to viewer, removed, disabled) still deletes the user's keys and
  sessions in that org, the older rule B16 (`authz.StandingChanged`). A stack key
  never passes an org-wide verb (no `StackID` on the resource).
- A CLI login (`/cli-codes` then `/auth/exchange`) mints a key with no
  ceiling and no stack: the user's full rights, as today.
- **decision taken:** members deploy to every env, production included;
  per-env protection is later.
- **decision taken:** any role mints keys, capped at its own.

## Contract between the lanes
- `api_keys` gains `level TEXT NOT NULL DEFAULT ''` ('' = no ceiling,
  else `viewer|member|owner|admin`) and `stack_id TEXT REFERENCES stacks (id)
  ON DELETE CASCADE` (migration `009_key_scope`).
- API `POST /orgs/:org/keys` body: `name`, `level` (optional), `stack`
  (optional stack slug). `GET /me/keys` rows add `level` and `stack`.
- Web mint form (org drawer keys section) posts `name`, `level`, `stack`.

## R1. Core (one worker)
Files: `internal/authz/authz.go` (+test), `internal/middleware/access.go`,
`internal/service/access.go`, `internal/service/admin.go`,
`internal/service/clilogin.go`, `leaf/user/user.go` (+test),
`leaf/org/org.go` (`roles` = owner, member, viewer),
`store/` api key store, migration 009, `internal/api/handler/v1/orgs.go`,
`internal/web/handler/canvas/org.go` (read the new form fields),
`docs/openapi.json` (regenerated).
- `authz.User` gains `KeyLevel Level` (or a bool for "no ceiling") and
  `KeyStack string`; `authz.Resource` gains `StackID`. `Can`: effective
  level = min(member level, key ceiling); a stack key refuses (NotFound) a
  resource of another stack and any resource with no `StackID`.
- The middleware fills `Resource.StackID` from the resolved scope.
- `MintKey` takes the ceiling and stack; refuses a ceiling above the
  minter's role in that org, an admin ceiling from a non-admin, a stack not
  in the org.
- Tests: `Can` table (ceiling below role, role demoted below ceiling, stack
  key on its stack, other stack, org verb); mint refusals; an API test where
  a viewer key gets 403 on a deploy and a stack key 404s another stack.

## R2. Surfaces (one worker, in parallel with R1)
Files: `internal/ui/drawer/org/*.templ` (keys section and role pickers),
`internal/ui/pages/setup/*.templ` role copy, `cmd/stackr/cmds.go` (+test).
- Org drawer: the key form gets a role select (roles up to the viewer's
  own) and a stack select (all stacks, or one); the key list shows both.
  The member list role picker offers owner, member, viewer with one short
  line each.
- CLI: `stackr key create --role viewer|member|owner --stack <slug>`;
  `stackr invite --role` help lists the three; `stackr key ls` shows role
  and stack.
- Viewer UI: every write button already sits behind a verb check
  (`can`/`canWrite` helpers in the web handlers); spot-check the drawers as
  a viewer and hide any that are not.

## Ship and QA
With the review fixes: dev.24. Rig: invite a viewer and a member to `smoke`;
viewer sees the canvas and cannot deploy, open the terminal or reveal a
secret; member can; a member's owner-ceiling key is refused; a stack key on
`shop` lists `shop` and 404s `web`; demote the member to viewer and their
key is deleted (B16), so the next request is a 401.

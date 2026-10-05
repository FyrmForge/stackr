# UX pass: CLI and web fixes

Status: S, C1, C2, W1, W2, W3 built 2026-10-05, on the rig as v0.6.0-dev.3.
D and the follow-ups below are open.

Source: the design-knowledge audits of the CLI and the web UI (run against
v0.0.52), rechecked against the working tree on 2026-10-05. Only items still
open are listed. Env sync is separate (`docs/features/env-sync.md`).

Done already, not in this pass: public sign-up closed, login rate limit,
wrong-password error, invite role text, bare `config-repo`, promote and
rollback plan and confirm.

## How the work runs

- One worker per package. A package owns its files; no two packages that run
  at the same time touch the same file.
- Every package ends green on `make test`, `make lint` and `make templint`
  (web packages), and leaves one test per behaviour change.
- No commits by workers. Changes stay unstaged for darthvader.
- After all packages land: one rig build (v0.6.0-dev.3), then QA on the rig.

## Packages

### S. Server bits (Sonnet). Runs first; C1 depends on it.
Files: `cmd/stackrd/main.go`, `internal/service/internal/store/jobs.go`,
`internal/service` (jobs), the jobs API handler, the org leaf and API routes.
- tzdata: `import _ "time/tzdata"` in `cmd/stackrd/main.go`, so `CRON_TZ`
  and `--tz` work beyond UTC.
- Job list: newest first, with a limit (`store/jobs.go:43-52` has no
  ORDER BY). Expose `?limit=` on the API.
- Org create in one call: name given at create, so a failure never leaves an
  "Untitled organization" draft (`cmds.go:365-386` makes three calls today).

### C1. CLI verbs (Opus)
Files: `cmd/stackr/stack.go`, `cmd/stackr/cmds.go`, `cmd/stackr/orgconfig.go`,
`cmd/stackr/cli_test.go`.
- `tile image-check` (and the stack and admin forms): help says it writes a
  release; output says which release and which envs it promoted into.
- `tile env-pairs`: read the current map, print the diff across all envs,
  confirm. `tile allow --set` the same. `slice on-remove drop` confirms.
- `org approve`: print the plan, then confirm, like `move` does for promote.
- Look up before confirming: `volume rm`, `dest rm`, `creds rm`,
  `members rm`; the prompt names the object, not a UUID.
- Copy that lies: `params get` masks secrets (name plus `****`) instead of
  omitting them; `stack rm` drops "and everything in it"; `dest add` drops
  "the bucket is dialled first".
- Next handle after writes: `rename` prints the new slug, `key add` prints
  the key id, `invites add` prints the full invite URL.
- `login --with-key` also reads `STACKR_KEY`, so the key can stay off argv.
- `job ls --limit`, using S. `org create` uses S's one call.
- `env create --from-kind` default: `promote` when the stack has an env,
  else required.

### C2. CLI plumbing (Sonnet), in parallel with C1
Files: `cmd/stackr/app.go`, `cmd/stackr/main.go`, `cmd/stackr/scope.go`.
- Errors: a 404 names what was asked for (`no tile "x" in shop/dev`); API
  field names map to flag names (`limits.memory_mb` reads `--memory`); the
  map already lives at `stack.go:73,110`, move it here.
- HTTP client with a timeout; a dial error reads "can't reach <server>".
- `(none)` splits into empty, not applicable and bad filter.
- Tables fit the terminal width: truncate long cells, drop UUID columns.
- Did-you-mean on unknown verbs; `help <unknown>` exits 2.
- Scope flags only on verbs that use them (`scope.go:25-28`).

### W1. Web visual (Sonnet)
Files: `ui/css/input.css`, `ui/tailwind.config.js`,
`internal/ui/components/table.templ`.
- Dark mode: lift muted, faint and accentHi text to APCA Lc 60+ (muted 75);
  input borders to 3:1; one hairline token for dividers.
- Focus: solid 2px ring with a 2px offset on `.btn`; `focus-visible` on tabs,
  settings nav, graph controls, rail links, `.link`, cards.
- Native radios, checkboxes and selects styled as one component.
- `.btn` pressed state, instant.
- No text under 12px.
- `tabular-nums` on tables, times, digests, counts.
- Motion: drawer reduced-motion rule, toast fade 200ms, no `transition-all`
  or hover glow on `.card`, drawer in 250-300ms with a faster exit.
- Mobile: 44px rail and small buttons.

### W2. Web behaviour (Opus), in parallel with W1
Files: `internal/ui/drawer/org/org.templ`, `internal/ui/pages/setup/setup.templ`,
`internal/ui/drawer/tile/tile.templ`, `internal/web/handler/canvas/org.go`,
`ui/ts/side-drawer.ts`, `ui/ts/flash-toast.ts` (compiled to `ui/static/js/elements/`),
`internal/ui/components/error.templ`, `internal/ui/components/confirm.templ`.
- Remove member and Revoke key go through the confirm dialog, naming the
  person or key. Setup "Discard this organization" becomes a typed confirm.
  Restart loses its dialog (a restart is safe to repeat).
- A drawer with unsaved edits asks before it closes (backdrop, Esc, x).
- Request errors show in the drawer, not as a "Request failed: 500" toast.
  The 404 and 500 pages keep the nav and show a request id.
- Minted key and invite link: full URL, Copy button, "not shown again".
  Invite rows get Copy and Revoke.
- `novalidate` on the raw forms in its files.

### W3. Web forms and a11y (Sonnet), after W2
Files: `internal/ui/components/form.templ`, `internal/ui/components/layout.templ`,
`internal/ui/components/drawer.templ`, `internal/ui/components/drawer_view.templ`,
`internal/web/handler/auth/login/login.templ`, the invite and account
templates, the handlers' form rules.
- `novalidate` on the Form component; fix-first messages instead of hamr's
  defaults ("Enter your email address, like name@example.com").
- Password field with show and hide; `autocomplete` on login, invite, account.
- Skip link; current crumb is plain text; drawer named by its title
  (`aria-labelledby`); one h1 on stack and env pages.
- `color-scheme` and `theme-color` meta.
- Drawer tab strip becomes a select on phones.

### D. Docs and boards (after the rig build)
- CLI docs reworked around user journeys, plus a CLI UX audit doc.
- fraedi design boards recaptured from the rig (login screen, no register).

## Follow-ups found while building
Each needs a file no package owned, mostly server side.
- `params get` still omits secrets: the `/params` handler
  (`internal/api/handler/v1/data.go`) should return secret rows with the
  value blanked; then the CLI masks them and the help is true.
- `image-check` cannot say which release it wrote: `flow/imagewatch`
  `release()` and the `kindImageWatch` runner should log the release number
  and each env promoted into.
- `members rm` prompt shows a user id: member JSON needs the email.
- Invite rows have Copy but no Revoke: no revoke-invite op exists.
- `MintKey` accepts a blank name and setup's config form 400s on a blank
  repo, so those two forms keep browser validation.
- Error pages: Log out answers 403 (no CSRF token on the page) and no
  request id is shown; `render.Theme` should put both on the context.
- Phone tab select does not update the URL: the drawer handler should send
  `HX-Push-Url`.
- `dialog.Restart` (`internal/ui/dialog/confirm.go`) is dead code.
- `stackr <unknown> <verb> --help` prints root help and exits 0.
- A stack, env or tile rename leaves directory links on the old slug.

## Round 3: from the CLI journey docs (2026-10-05)
Evidence: fraedi `stackr/design/cli-journeys`, local copy in the session
scratchpad `cli/journeys/`.
- A tile that crashes on start hides its error; `logs` refuses to show it (J5).
- An unresolved param ref leaves a deploy waiting with no output; `params set`
  redeploys tiles without saying so (J3).
- Promote into an env that lacks the tiles applies nothing but marks the env
  as on the release (J4). Refuse an empty promote and say why.
- `STACKR_KEY` alone does not authenticate; CI must `login`, which writes the
  key to disk (J8).
- Invites cannot be revoked from the API or CLI (J6).

## Not in this pass
- More org roles than owner (v1 has one role).
- A UI typeface (needs a pick and a self-hosted font).
- Undo (no undo model exists; confirms cover it for now).
- Resource-name shell completion, `--json` contract cleanup, examples in help.

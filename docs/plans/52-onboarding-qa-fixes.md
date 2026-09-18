# Plan: onboarding QA fixes

Status: steps 1 to 9 built 2026-09-18, step 10 investigated and accepted.
`make test`, `make lint`, `make templint` green. Verified on the rig the same
day, round doc `docs/qa/rounds/2026-09-18-onboarding.md`: steps 1, 2, 3, 6, 7,
8, 9 held first time; steps 4 and 5 were incomplete (the profile form's hidden
username field and the org cookie), both fixed and re-verified after a
redeploy. Three findings from that round stay open and are not in this plan:
a required secret that does not block the apply, a config path that saves a
binding to a file that does not exist, and the real length of the boot
certificate window (a minute, not ten seconds).

Working rules: discuss first, one point at a time, no code without a go, no
git writes, no edits to `*_templ.go` or `output.css`, terse UI copy.

## Why

The first full run of `docs/qa/onboarding.md` on the rig (round doc:
`docs/qa/rounds/2026-09-17-onboarding.md`) took a wiped box to a live HTTPS
app, and found ten things on the way. Two of them strand a new user on their
first attempt at creating an organization, which is the first thing a beta
tester does. This plan is those ten, in priority order, plus the two parts of
the runbook that could not run.

Nothing here is a new feature. Steps 1 and 2 are the beta blockers; the rest
are cheap while the code is open.

## Blockers

### 1. An unnamed draft on the by-hand branch is a dead end

`setupDonePage` (`handlers/web/handler/org/setup.templ:405`) decides its copy
from `o.Name == setupDraftName` alone and then assumes the config branch: it
says the config file will supply the name, and its one button is "Back to the
config file" → `/orgs/<slug>/setup/config`. That step does not exist on the
by-hand branch, so `Setup` (`handler/org/setup.go:104`) bounces it to
`/setup/done`, the page it came from. The button loops.

Reached with no URL typing: `/setup` → "Fill it in here" → navigate away →
the root canvas lists "Untitled organization — Setup unfinished" → click it →
gated → the summary.

Fix, decided 2026-09-17: branch on `o.SetupConfigBranch()` as well as the
name. A by-hand draft with the placeholder name gets "This organization has
no name yet." and a button "Name it" → `/orgs/<slug>/setup/name`; the config
branch keeps the copy and button it has today. One template, plus a test per
branch in `setup_test.go`.

### 2. Discard never works from the wizard

`setupDiscard` (`setup.templ:279`) posts `/orgs/:slug/delete` with a CSRF
field and an `hx-confirm` dialog. `Delete`
(`handler/web/handler/org/members.go:174`) calls
`components.RequireConfirm(c, o.Slug)`, which wants a form field `confirm`
holding the slug (`components/confirm.go:18`). The form never sends one, so
every Discard is a 400, the org stays, and the page says nothing. Verified on
the rig: `POST /orgs/org-6447f2/delete` → 400, row still there.

This hits every draft on both branches. With step 1 it means a draft can be
neither finished nor deleted, and the rig still carries the one this round
made.

**The guard: decided 2026-09-17.** `Delete` skips `RequireConfirm` when the
org's setup is unfinished; `hx-confirm` stays as the only prompt. Typing a
generated slug to throw away an empty draft is friction with nothing behind
it, and the stacks check above already refuses a draft whose config file
built something. A finished org still requires the typed name, so the guard
is untouched everywhere it protects data.

**The silence: decided 2026-09-17, no code.** With the typed confirm gone the
only refusal left on this path is a config-branch draft whose apply already
built stacks, and that one already sets a flash and redirects to
`/setup/done` over htmx, the same pair every other wizard step uses. Verify
it on the rig during the verification round rather than changing anything;
the 400 this round hit never reached that branch.

## The rest

### 3. A tile reads "Crashed" while it is serving traffic

Root cause is not the deploy path, it is the reconciler.
`reconcileStatus` (`infra/metrics/reconcile.go:24`) arbitrates only
running↔stopped↔unhealthy; there is no `case "error"`, so a tile written to
`error` by a failed deploy (`infra/deploy/engine.go:498`) stays red for ever
even while its container runs. Verified: a bad image tag failed the pull,
Swarm kept the old task (`1/1`, `nginx:alpine`, HTTP 200), and the canvas
said Crashed.

Fix, decided 2026-09-17: one case in `reconcileStatus`, error → running when
the container is up, plus a case in `reconcile_test.go`. That covers every
path that leaves a tile red with a healthy container, not just this one.

The card then reads Online again within a tick and the canvas carries no
trace of the failure. Accepted: the deployments row and its log still name
the exact cause, three clicks away. A "last deploy failed" mark on the card
is **out of scope, decided 2026-09-17** — it needs a new field on the card
and a rule for when it clears.

### 4. Login, registration and profile forms have no autocomplete attributes

Chrome logs it on `/register` (wants `username`, `new-password`) and on
`/account/profile` ("password forms should have a username field"). Password
managers fill them badly. Fix: the attributes, on the three forms.

### 5. The active organization can be an unfinished draft

`OrgContext` (`handlers/middleware/orgctx.go:76`) defaults to `orgs[0]`, so a
draft can win over finished orgs; after logging back in the switcher showed
"Untitled organization" as active with two finished orgs on the account. Fix:
prefer the first org with `SetupDoneAt != nil`, fall back to `orgs[0]` when
every org is a draft. The cookie keeps winning when it names one, which is
what a user mid-wizard needs. Mostly evaporates once 1 and 2 land, so it goes
after them.

### 6. "No tiles in this environment yet" right after creating a tile

A created tile is staged, and the canvas keeps its empty state
(`handler/project/graph.templ:127`) with "1 pending change, review & apply"
underneath, so the page contradicts the click that just happened.

Fix, decided 2026-09-18: when `stagedCount > 0` the empty state reads
"Nothing applied yet. Review the pending change." instead. A ghost card for a
staged create is the nicer answer and the bigger one; out of scope here.

### 7. The plan preview lists a change that changes nothing — root cause was elsewhere

Applying the managed-database create also listed
`~ production web domain web.demo...vulpe.dev → web.demo...vulpe.dev (auto)`,
same value both sides. Source is `applier.StagedPlan` →
`config/stackconf/plan.go`.

Identical rows were already dropped (`diffDomains`, `if want != have`), so the
planned fix would not have fired. Read out on 2026-09-18: the two sides were
not identical, they differed by " (https, no redirect)", and the reason is that
three of the four places that create a domain row left `ForceHTTPS` at its zero
value while every reader treats "absent" as on (`DomainConf.ForceHTTPSOn`, the
API's explicit `force := true`, `apply.go`). So a panel-created domain both
diffed against its own serialization for ever and served plain HTTP without the
bounce.

Fixed at the three creation sites instead: `config/envops/envops.go`
(`EnsureAutoDomain`), `handlers/web/handler/app/handler.go` and
`handlers/web/handler/db/handler.go` now set `ForceHTTPS` alongside `HTTPS`.
Rows already in a database keep the phantom until their next apply, which on
the rig means after a wipe. Left alone: `domainsToConf` drops the https flags
for `auto:`/`apex:` rows, so an operator who deliberately turns the bounce off
on a generated domain still gets a phantom row. Nothing creates that today.

### 8. A created API key's token lives only in a toast

It is shown once in a flash that disappears on its own; miss it and the key
is dead. Intended ("the token is shown once, at creation") but harsh.

Fix, decided 2026-09-18: replace the flash with a panel on the page above the
key list, the token in a mono box, a copy button, and one line saying it is
not shown again. It stays until the next page load. Not a sticky flash: that
wedges "must not disappear" into a component whose whole job is to
disappear.

### 9. The branch question has no way out

`/setup` renders no shell: no sidebar, no logout, no link to the organization
list. Every later step at least has Discard.

Fix, decided 2026-09-18: one link under the two branch buttons, back to the
organization list. Not the full app shell: the wizard is chromeless on
purpose, and a sidebar here invites clicks into orgs the gate bounces back.
The link shows unconditionally; on a fresh install it lands on the
organizations empty state, which carries its own create action, so there is
no dead end to guard against.

### 10. A minute-long window after boot serves a cert that fails verification

Right after the panel and Traefik started, the panel host served a
certificate that failed hostname verification; moments later it was the real
Let's Encrypt one. It is Traefik's built-in default certificate before the
stored ones are in play.

Measured on 2026-09-18 (round 2): 60 to 90 seconds, not the ten this was
written with. The traefik log says why: it registers the ACME account against
Let's Encrypt at boot before it serves anything from `le.json`.

Investigated 2026-09-18, **accepted**. Both inputs are already on the manager's
disk before the container starts: `writeStatic` writes `traefik.yml` and the
dynamic router files (`writePanel`) ahead of the service create, and the
certificate store is the bind-mounted `le.json`. Traefik binds :443 as it boots
and propagates the file provider and the acme store after, so the window is
inside Traefik and there is nothing on this side to order differently. Waiting
for the store before publishing the router is not available either: Traefik
owns the listener, not the panel. `infra/proxy/probe.go` is the readiness answer
we do have, and it is per-route, after the fact.

## Not code: the runbook's gaps

Two parts of `docs/qa/onboarding.md` could not run, and they stay unrun until
someone does these once:

- **Parts D and 15, the config branch and the GitHub App. Done 2026-09-18,
  the branch is now testable.** Creating an App needs an interactive GitHub
  login, which an agent cannot do, so the developer made one on the rig
  through `beta-qa`'s connector step and installed it on the FyrmForge org.
  `scripts/dev/connector-keep.sh save` has it (private key included) beside
  the data dir; `restore <org-slug>` puts it back after every wipe, so this
  is a one-time cost.
  - The file the config step binds is `FyrmForge/stackr-test@master`, path
    `stackr-org-min.yml` (added 2026-09-18, commit `b01e00f`): `org: Config
    Branch QA`, one required secret with no value, one whoami tile. Small on
    purpose, so an apply is seconds, the plan has an input to ask for, and a
    forced failure for check 27 is unambiguous. The repo's full
    `stackr-org.yml` builds five images and ten tiles, which is the wrong
    shape for this pass.
  - Sequence in a round: wipe, deploy, register, start the wizard's config
    branch to get a draft slug, `connector-keep.sh restore <draft-slug>`,
    then carry on from the connector step with the App already there.
- **Part G, the published-release install path.** Needs its own wipe and a
  real `install.sh` run. Worth folding into the next release's verification
  rather than this plan.

## Order (done)

1 and 2 together, they are the same screen and the same user. Then 3, then 4.
5 to 9 in any order once those are in. 10 is an investigation.

## Verification

Wipe the rig, deploy, and run `docs/qa/onboarding.md` from step 1. It has to
reach a live app again, and specifically:

- check 21b, a by-hand draft reached from the root canvas offers a name step
- check 39, Discard removes a draft, and says so if it refuses
- a failed pull leaves the card Online with a failed deployment row
- the switcher does not show a draft as active while a finished org exists
- **a hostname that has never held a certificate still gets one.** Step 7 turns
  the plain-HTTP bounce on for every panel-created domain, and the http-01
  challenge rides the same entrypoint: Traefik's own challenge router is meant
  to outrank the file provider's, but this is the one change that could break
  check 46 as "the cert never issues", not as an error. A fresh tile with a
  fresh auto domain, not the kept `web` host.
- the keys page shows a new token in its panel and Copy puts it on the
  clipboard (the listener is global, frontend/static/js/main.js:175)
- "All organizations" on `/setup` lands on the root canvas
- the canvas reads "Nothing applied yet. Review the pending change." between a
  tile create and its apply
- the register and login forms offer a password manager a username, and the
  account password form no longer warns in the console
- applying twice in a row leaves the second plan empty: no domain row that
  changes nothing

`make test`, `make lint`, `make templint` before the rig round, not after.

## State when this plan was written

Everything below is fact on the rig as of 2026-09-18, not a plan.

- **Nothing in this plan is built.** Steps 1, 2, 3, 6, 8, 9, 10 have their
  decisions recorded above; steps 4, 5, 7 are one obvious fix each and were
  not discussed further.
- **Rig** (`stackr-test`, manager only, `skrt2` not joined): wiped and
  deployed from the working tree this round, so it runs `stkr:local`, not a
  release image. It carries `beta-qa` (stack `demo`, env `production`, tile
  `web`, managed postgres `db`), `skip-domain-org`, and the stuck draft
  `org-6447f2` that findings 1 and 2 make undeletable.
- `web`'s image is still `nginx:thistagdoesnotexist` from the forced
  failure. The service serves the old `nginx:alpine` task, so the site is up
  and the card says Crashed, which is finding 3 sitting there to look at.
- Accounts: `admin@test.com` / `Test1234!` (admin), `viewer@test.com` /
  `Viewer1234!` (member of beta-qa). An API key named `qa2` exists with every
  scope; its token was recorded in this session only, so make a new one if it
  is needed again.
- **The GitHub App is saved.** `connector-keep.sh save` holds one connector
  for `beta-qa` beside the data dir on the box. Restore it after a wipe with
  `connector-keep.sh restore <org-slug>`.
- Related work: `docs/plans/51-pre-cut.md` (another session, same day) freezes
  `001_initial` and adds a destructive-migration guard as its last act. No
  item here touches the schema, so the two do not collide, but 51's own
  reasoning wants a green first-boot round before the freeze, which is this
  plan. 51 also caught the wrong service name in `docs/qa/onboarding.md`
  (fixed 2026-09-18) and still claims the onboarding chain has never been
  run, which this round settles; that paragraph is 51's to update.

## What was built, 2026-09-18

One commit's worth of changes, nothing staged or committed (they sit unstaged).

- **1**: `handler/org/setup.templ`, the unnamed-draft branch of
  `setupDonePage` now splits on `o.SetupConfigBranch()`; a by-hand draft gets
  "This organization has no name yet." and "Name it" → `/setup/name`. Test:
  `TestSetupDonePageUnnamedDraftPerBranch` (org/people_render_test.go), both
  branches rendered.
- **2**: `handler/org/members.go`, `Delete` runs `RequireConfirm` only when
  setup is finished. `TestJourneyDiscardDraft` was asserting the old contract
  (a 400 on a wrong typed slug) and now posts an empty form;
  `TestJourneyDeleteFinishedOrgNeedsTypedName` keeps the guard covered where it
  protects data.
- **3**: `infra/metrics/reconcile.go`, `case "error"` → running (or unhealthy)
  when the container is up, three cases in `reconcile_test.go`.
- **4**: `autocomplete` on the register (name, username, new-password) and
  login (username, current-password) forms, and a hidden `username` field in
  the account password form, which is the one Chrome was warning about.
- **5**: `handlers/middleware/orgctx.go`, the active-org pick prefers an org
  with `SetupDoneAt != nil` before falling back to `orgs[0]`; the cookie still
  wins when it names one.
- **6**: `handler/project/graph.templ`, `emptyCanvasText(stagedCount)`.
- **7**: the three `ForceHTTPS` defaults above.
- **8**: `handler/account/handler.go` + `account.templ`, the new token rides a
  one-shot `stackr_new_key` cookie (HttpOnly, 60s, cleared on read) to the keys
  page, which renders it in a mono box with a copy button; the flash is now
  just "API key created."
- **9**: `handler/org/setup.templ`, an "All organizations" link under the two
  branch buttons.
- **10**: investigation only, see above.

Not done, and not code: plan 51's paragraph claiming the onboarding chain has
never been run green, and a row for this plan in `docs/plans/README.md` (that
file has another session's uncommitted edits in it).

## Aftermath, 2026-09-18

The verification round (`docs/qa/rounds/2026-09-18-onboarding.md`) found the
two incomplete fixes and three things this plan did not ask about. What was
done with them:

- **Step 4 was incomplete.** Chrome does not count a `type=hidden` field as
  the username field a password form needs. It is now a `type=text` field
  hidden with `sr-only`, `tabindex="-1"`, `readonly`, `aria-hidden`. Console
  silent on `/account/profile` after a redeploy.
- **Step 5 was incomplete.** The fallback was fixed but the cause was the
  cookie: creating a draft calls `setActive`, and `stackr_org` outlives the
  session. `OrgContext` now ignores a cookie naming an unfinished draft while
  the account has a finished org, on both the admin and member-scoped lists.
  Test `TestOrgContextPrefersAFinishedOrg`.
- **A required secret does not block an apply (round finding R2-3).** Read
  out: this is deliberate and the plan row already says "Apply goes ahead
  without it." The defect was a stale comment on `SecretConf.Required`
  claiming the apply refuses. Comment corrected, no behaviour changed. What is
  left is a question, not a bug: `required: true` currently only prefixes that
  note, so it has no teeth beyond wording. Deciding whether it should is not
  in this plan.
- **A config path that does not exist (R2-4).** The binding is still saved,
  which is right for a file about to be committed, but the message now names
  the path, repo and branch it looked in and says to check the path or commit
  the file. `handler/org/config.go`.
- **The boot certificate window (R2-5).** Number corrected above, still
  accepted.

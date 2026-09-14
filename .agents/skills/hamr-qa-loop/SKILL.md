---
name: hamr-qa-loop
description: The QA testing-and-fixing loop for a HAMR app. Use whenever asked to QA, test, or verify a feature/branch — or after finishing a feature worth verifying. Drives the app with Playwright MCP as the target user(s) across device profiles, checks functional + UI/UX quality against the watch-lists, fixes findings, and loops (3 rounds default). Uses hamr MCP for everything server-side.
---

# QA loop

Iterative test-and-fix loop. Goal: the best UI/UX an agent can deliver, not a
box-ticking pass. Judge, don't just verify.

## Ground rules

- `hamr dev` is already running. **Never** run `templ generate`, `go build`,
  or start/stop the site yourself. All server-side actions go through hamr
  MCP: `dev.info` (resolve ports EVERY time — hamr port-walks, never assume),
  `make.run` for one-off make targets, `logs.read` / `docker.logs` for server
  errors, `stripe.complete` for mock checkout, `mail.list`/`mail.get` for
  outbound email, `sms.list`/`sms.get` for outbound SMS. If `dev.info` fails,
  stop and ask the developer to start `hamr dev`. (Dotted names are hamr's;
  your client may surface them underscored, e.g. `mcp__hamr__dev_info`.)
- Drive the browser with Playwright MCP (Chromium-only — note as a limit,
  don't claim cross-browser coverage).
- Fixes follow root-cause-not-symptom: grep callers, check the blast radius
  of shared components, re-run the exact broken flow after the fix.
- Questions from the user during their pass are questions — answer, don't act.

## The loop

### 1. Scope
Unless the user names a scope: analyse unstaged changes, else the diff vs
the default branch. From the diff, work out:
- what the changed features actually are
- **who the target user is** — work out the app's roles (admin, customer,
  visitor, …) from its auth/handler code; usually a list of several
- how to drive each feature end to end (entry point → actions → outcomes)
Write this up as a short QA plan before driving.

### 2. Drive
As each target user, on each device profile:
- **Profiles:** mobile 375×812 · tablet 768×1024 · desktop 1440×900
  (`browser_resize`)
- Walk every lifecycle state the feature has (use seeded states; see step 4
  when data is missing). Seeded logins usually live in the project's seed SQL
  (look under `internal/db/`).
- **Screenshot every state and actually look at it** — critique like a
  designer (spacing, alignment, hierarchy) before moving on. DOM snapshots
  alone miss visual jank.
- **Watch console + network on every page** (`browser_console_messages`,
  `browser_network_requests`): zero JS errors, no 4xx/5xx, no 404 assets.
  hamr MCP `console.read` and `http.read` cover the same from the server side.
- **Check server errors each round** via hamr MCP `logs.read` (app) and
  `docker.logs` (postgres/minio/etc.).
- Check functional issues (watch-list A) and UI/UX issues (watch-list B).

### 3. Fix
Fix findings (root cause). Re-drive the exact flow that was broken.
Anything deliberately deferred goes to the report's follow-ups, not silence.

### 4. Data
Identify missing DB states the drive needs and populate them (seed or
targeted inserts). Apply the **0 / 1 / many rule**: every list/widget gets
seen empty, with one item, and with many items + longest-realistic strings.
Realistic files only — real images/videos/PDFs, never 1×1 pixels or junk.

### 5. Grade
End each round with a scorecard, not a vibe. Score each area 1–10 with a
one-line why:
correctness & safety (watch-list A) · visibility of status · match to real
world · user control & error prevention · error recovery · consistency ·
responsive/touch · feedback & motion · accessibility.

- **Overall = the lowest area score** (a 4 in recovery is a 4 product). Show
  the mean beside it only to track movement between rounds.
- Caps: any watch-list-A miss → correctness ≤ 5. Irreversible action without
  confirm/undo → control ≤ 6. Error message that says "retry" for something
  that can't succeed → recovery ≤ 7. Full-page reload on a primary action →
  motion ≤ 7 unless the redirect lands on visibly changed state.
- 9+ only if a first-time user could not get it wrong and a mistake is
  recoverable.
- Every area < 9 gets a **"to 9" line**: the concrete change, rough cost,
  in-scope or elsewhere. In-scope ones are the next round's fix list; the
  rest get noted as follow-ups.

### 6. Loop
Back to step 2. Default **3 rounds**, unless the user specifies otherwise.
Stop early when every area ≥ 8 and no in-scope "to 9" remains; add a round
when any area is still < 8.

### 7. Report
Write a small doc under `docs/qa/` (bullets, nested where needed):
issues found → fixes applied, round by round, the scorecard per round so
movement is visible, a one-paragraph verdict, plus open follow-ups.
Include which user/profile combinations were driven.

## Watch-list A — functional

- Double-submit / back-button / resubmit → exactly one effect (idempotency).
- Invalid input, max lengths, required fields — inline validation, not
  color-only.
- Async (HTMX) actions: what happens when the request fails? Button
  disabled/spinner while in flight (no double-click window)?
- Auth/role: always know which account you're driving as; verify each state
  from **both** sides where a flow has two sides (e.g. sender and recipient).
- Deep links: URLs of detail pages work cold (fresh session → login → land).
- Perf smells from `browser_network_requests`: oversized images (original
  served where a thumb belongs), request storms (N+1 as many identical XHRs),
  slow endpoints.
- Email/SMS surface: flows that send — `mail.list`/`mail.get` (or
  `sms.list`/`sms.get`) → fired, renders, links land on the right page.
- Payment surface: flows that charge — `stripe.list` to find sessions, drive
  outcomes with `stripe.complete` / `stripe.expire` / `stripe.refund`, verify
  the app reacts to each.
- Expired session mid-flow: log out elsewhere, then click an HTMX action —
  must navigate to login, not swap the login page into a fragment.
- User-content injection: messages / display names with `<script>`, quotes,
  emoji — escaped everywhere they render (templ escapes text; URL and
  attribute sinks are the usual leaks).

## Watch-list B — UI/UX

Layout / responsive
- No horizontal scrollbar at any width; nothing overflows or overlaps.
- Long text, names, filenames, prices truncate — stress with LONG values.
- Touch targets ≥ 44px on mobile; hamburger nav works.

Data & content
- Realistic test data only.
- Attached/uploaded files always show **type + name + view + download** —
  never placeholder copy.
- Microcopy pass: read every string — consistent tone, no dev-speak,
  currency and dates formatted correctly for the app's locale.

Navigation & affordance
- Every count/badge/link clicks through to somewhere real — no dead ends.
- Automated/system messages link the thing they mention; link target resolved
  per viewer role.
- Interaction states on every interactive element: hover, focus, disabled,
  in-flight.

States (the less-happy paths)
- Empty, loading/processing, and error states all exist and explain *why +
  what to do next* — no bare "something went wrong".

Motion (via `browser_evaluate` / `browser_run_code_unsafe`)
- Transitions exist where expected — modals, menus, HTMX swaps; no hard
  content "pop", no first-paint flash.
- `document.getAnimations()`: assert the transition ran, duration sane
  (~150–300ms), and it animates **transform/opacity only** (not width/top —
  layout-thrash smell).
- Deterministic mid-frames: pause + pin `anim.currentTime` to ~50%, then
  screenshot.
- Jank probe: 1s `requestAnimationFrame` sampler while triggering — flag
  dropped-frame spikes.
- `prefers-reduced-motion` respected.
- Limit: no video via MCP; smoothness "feel" is approximated by numbers.

Consistency & a11y
- Spacing/buttons/fonts/colors match the rest of the site (compare against an
  existing good page).
- Contrast, visible focus rings, image alt text.
- Keyboard-only walkthrough: tab order sane, Esc closes modals, focus returns
  after close, Enter submits.

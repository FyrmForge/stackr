# Onboarding QA runbook

An agent runs this on a disposable rig, start to finish, without asking the
developer anything. It covers first boot to a deployed app: wipe, install,
account, organization, stack, tile, reachable app.

`docs/qa/loop.md` is the broad surface sweep. This one is a single linear
chain, so it has its own rules: **report the whole chain first, fix nothing
mid-run**, then fix, then wipe and run it again from step 1. A fix applied
halfway through cannot be trusted, because everything behind it was created by
the old code.

## Rules

- Headed Playwright, never headless. Screenshots to `.playwright-mcp` only,
  cleaned up at the end of the round.
- Server-side answers come from hamr MCP, never from a pasted log: `logs.read`,
  `console.read`, `http.read`, `mail.list` / `mail.get`, `docker.status`.
  Those read the local `hamr dev` server, so on the rig use `ssh` for the
  panel's own logs (`docker service logs stackr`; the service is `stackr`,
  not `stackr_panel`).
- A step where you had to guess what to do next is a finding even when it
  worked. Record the guess.
- Record pass / fail / not testable per numbered check. Nothing else counts as
  having run it.
- Findings go in a round doc at `docs/qa/rounds/<date>-onboarding.md`
  (gitignored). Fixed ones end in code, open ones in `docs/notes.md`.

## The rig

Not recorded in the repository. The manager comes from
`STACKR_DEPLOY_HOST`; workers are passed to `--nodes`.

```bash
export STACKR_DEPLOY_HOST=manager.example.test
scripts/dev/connector-keep.sh save          # before the wipe, keeps the GitHub App
scripts/dev/wipe-test.sh --no-keep-connectors --no-keep-certs
scripts/dev/deploy-test.sh
```

Certificates and connectors are kept by default. This pass is specifically
about the first-boot path, so opt out of both, then put the connector back
after the org exists (step 4 below). Re-issuing certificates burns Let's
Encrypt's duplicate limit of 5 per name set per week; if the round is not
about TLS issuance, drop `--no-keep-certs`.

The rig is disposable, so wipe it without asking. There is no dry run.

## Part A — first boot

`deploy-test.sh` builds the current tree and swaps the service; it is the
default install path for this pass. Part G runs the published-release path
separately.

1. **Nothing is up before install.** After the wipe, the panel host answers
   nothing. A page that still loads means the wipe was partial, which is the
   2026-09-01 Traefik incident: stop and finish the wipe before continuing.
2. **Deploy succeeds and the panel answers.** `docker service ls` on the
   manager shows the panel converged, and its logs show migrations running
   forward on an empty database with no error.
3. **The first visit lands on registration.** `GET /` with no session and no
   accounts reaches `/register`, and so does `/login`: with no accounts there
   is nothing to log into (`firstBootRegister`, web/server.go:622).
4. **Registration validates per field.** Name, email, password,
   confirm password. Blur each with a bad value: the field's own message
   appears (posts to `/register/validate/:field`), not a page-level error.
   Weak password refused, mismatched confirmation refused.
5. **Registration creates the account and signs it in.** One admin account,
   a welcome flash, and it lands on `/setup` (register/handler.go:99).
6. **Registration closes behind you.** `GET /register` now redirects to
   `/login` (`firstBootOnly`). This is the only way an account is created
   without an invite; if it stays open, anyone who finds the host gets an
   account.
7. **Log out and back in.** The session survives, and the login page is the
   one that answers now.

## Part B — the wizard, branch question

The wizard is `/setup` then `/orgs/:slug/setup/:step`. Step 1 is the branch
question and writes nothing: a GET that created an org would mean a crawler or
a link prefetch makes organizations.

8. **`/setup` is admin only.** A non-admin account (invite one in part E, or
   check after) gets refused, not the branch question
   (web/server.go:230, `adminOnly`).
9. **The branch question offers both branches** and explains the difference
   well enough to choose without reading docs.
10. **Choosing by hand** creates the draft and lands on step 2, the name step.
    The stepper says "Step 2 of 6".
11. **The draft is called "Untitled organization"** in the org switcher until
    it is named. Ugly but deliberate; a nameless org is what this wizard was
    rebuilt to remove.
12. **The branch is not a one-way door.** Switch to config and back
    (`POST /setup/mode`). Switching to the by-hand branch drops the config
    binding and rejects any plan waiting on it. The step numbers change with
    the branch: 5 steps by hand, 4 on config, plus the branch question.

## Part C — the by-hand branch

Steps: name, connector, domain, team, done.

13. **Name.** Empty refused. The field starts empty rather than prefilled with
    the placeholder. Saving renames the org and its slug, and the URL you are
    on afterwards uses the new slug.
14. **Connector.** With no GitHub App yet, the step renders inline and offers
    the flow. Skip is offered and works.
15. **Connector, real flow.** Create the App through the step, install it on at
    least one repository, and come back. The step lists the repositories. An
    App that exists but was installed nowhere must show as having no
    repositories: if it looks connected here, step 3 binds a repo the server
    cannot read.
16. **Domain.** The field is prefilled `<slug>.<root>` from the root domain the
    installer seeded. A LAN install with no root domain gets an empty field,
    not a guess.
17. **Domain, saved.** Save it, and check it shows on the summary.
18. **Domain, skipped.** On a second org, skip it, finish, and confirm the org
    still got a domain under the server's own host
    (`ensureDefaultDomain`, handler/org/setup.go). An org that finishes with no
    domain of any kind gets tiles that come up unreachable with nothing on
    screen saying why.
19. **Team.** Add an existing user; invite a new email. With mail configured,
    the invite arrives (read it, never ask what it said); without, the UI says
    the link has to be copied by hand.
20. **Every step is skippable** and the summary says which are done.
21. **The summary does not finish the wizard.** Load step 6, press back,
    reload: `setup_done_at` is still null. Only `POST /setup/done` closes it.
21b. **An unnamed draft on this branch is not a dead end.** Create the draft,
    leave the wizard, then reach it again from the root canvas ("Untitled
    organization"). The summary must offer a way to name it, not the config
    branch's copy, and its one button must not lead back to the page it is
    on.
22. **Finish.** It lands on the org canvas, and the org is usable.

## Part D — the config branch

Steps: connector, config, plan, team, done. Needs the GitHub App from
`connector-keep.sh restore <org-slug>`, or a fresh App flow.

23. **Connector first**, same checks as 15.
24. **Bind a config file**: connector, repository, branch, path. A path that
    does not exist is refused with something readable, not a 500.
25. **Binding produces a plan** and the wizard shows it before moving on
    (`/setup/config/plan`). There is no way past the plan screen: on this
    branch the org is named and built by the apply.
26. **Approve.** The apply names the org, so the next step's URL uses the slug
    it has now, not the one the request arrived on.
27. **A failed apply stays on the plan** with the error on it, and does not
    move to the next step.
28. **Reject** goes back to the binding form, not on to the next step.
29. **Plan inputs**, where the file declares any, are asked for before apply.
30. **No name step and no domain step** on this branch, and the summary shows
    the config row rather than a domain row.
31. **A hand-typed URL for the other branch's step** (e.g. `/setup/name` here)
    redirects to the summary rather than rendering a page that cannot apply.
32. **Finish**, then confirm the org's stacks and tiles came from the file.

## Part E — the setup gate

An unfinished org exposes only its own wizard. Every check here is a way out
of the flow that used to exist.

33. **The canvas is closed** while setup is open: `/orgs/:slug` sends the owner
    to `/setup/done`.
34. **Stack and plan pages under the org** are closed the same way, id-keyed
    URLs included.
35. **Settings tabs are closed**: each redirects to the summary, not to the
    tab. An htmx form post gets `HX-Redirect`, so the wizard must not appear
    swapped inside a form's target.
36. **A member, not the owner**, gets the holding page on both the canvas and
    settings, not a 404 and not the wizard.
37. **A stranger** (signed in, not a member, not admin) gets not-found.
38. **The API refuses with 409**, not a redirect: a 303 to HTML is a parse
    error to the CLI. `stackr` against an unfinished org says setup is not
    finished and names the URL (api/v1/auth.go:262).
39. **Discard works.** `POST /orgs/:slug/delete` is open during setup, so an
    abandoned draft can be thrown away. Without it the only way out of a draft
    is to finish it.
40. **`/servers` still works for the owning account with no finished org**:
    add node, drain, save a volume. The middleware used to answer "read-only
    access" here.
41. **Finished orgs do not reopen.** Load a wizard step URL on a finished org:
    it flashes "Setup is already finished." and goes to the settings tab that
    owns that setting, not to a random page.

## Part F — first app online

Time this part and count the clicks. This is the journey a beta user does
first, and the one they abandon.

42. **Create a stack** from the canvas.
43. **Create an environment** if the stack does not come with one.
44. **Create a tile** from a repository, or from an image if no connector.
45. **Deploy it.** The run page says what is happening at every stage; no stage
    sits silent. A failure names the step that failed and shows its log.
46. **The app is reachable** on its hostname over the scheme the install chose,
    from a browser that is not on the rig.
47. **A managed database tile**: provision, and a tile that consumes it comes
    up connected.
48. **Break it on purpose** and find out why: from the canvas to the log line
    that explains it, counting clicks.
49. **Server-side clean.** `docker service logs` for the panel across the whole
    run: no panics, no repeated errors. Browser console: zero JS errors, no
    4xx or 5xx, no missing assets, no CSP violations.

## Part G — the published install path

Run once per release, not every round. Same box, wiped again.

50. **`install.sh --dry-run`** shows the form and prints what it would run,
    with no root and no docker.
51. **`install.sh` with `--yes` and flags** installs unattended and refuses a
    live host.
52. **The installer form** validates bad answers under the field, and the DNS
    summary warns when the panel host or `*.<root>` does not resolve yet.
53. **Parts A to C again on that install**, at least through first boot and a
    finished org, so the release image's own first-run path is exercised and
    not just the local build's.
54. **The recovery CLI**: `stackr` from the host shell. Today it needs a login
    it has no way to perform, which is a known open item in `docs/notes.md`;
    confirm whether the install script still promises it.

## Known open items

Do not file these again. Confirm they still behave as described, and note it
if one got worse.

- The recovery CLI cannot act as admin (check 54).
- The join script needs an interactive terminal for sudo, so a headless
  install cannot add a node.
- `files:` only work on the manager.
- An org built by hand cannot be exported back to config on any surface.

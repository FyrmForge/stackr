Source: internal/stackrd/handlers/web/ — 68 .templ files under handler/*/ and components/
Commit: c2423f0
Taken: what each screen shows, the decisions it only displays, and which parts update themselves
Cut: every line of templ, all Tailwind, all htmx attributes, all `_templ.go`
Cuts belong to: the new web layer. The verdicts listed here belong in services; the panel only renders them.

Reference only. This says **what a screen shows**, never how. The rewrite
does the UI after the API and CLI, so this is a checklist of what has to be
answerable, not a layout to reproduce.

## Shared components

- **shell / layout** — chrome: org and stack switcher, the nav, the flash
  slot, the update badge. Everything renders inside it.
- **panel** — the repeated page frame: header with title and location
  breadcrumb, a tab strip, a body. Carries the "which environment am I
  looking at" context through the page (`UpperEnv`, tile location) so tabs
  and forms do not each re-derive it.
- **badge** — one status word rendered consistently. It translates status
  spellings for humans (`waiting_ci` → "waiting for CI"). The *set* of
  statuses is a service answer; the badge only colours it.
- **envcolor** — a stable colour per environment, so prod looks like prod
  everywhere.
- **confirm** — the destructive-action guard. Two strengths: a plain
  confirm, and the type-the-name variant that takes the post URL, the word
  that must be typed, a warning line, and an explicit list of **what is
  kept**. Keep both, and keep "what survives" as a required field, not an
  optional nicety.
- **logview** — a log pane: SSE stream, search box, level filter, and
  toggles for timestamps, wrapping and follow. Used by tiles, deployments
  and containers.
- **varsedit** — the param editor. Rows show key, level that decided it,
  and whether this level overrides. Secret plaintext is **never in the
  DOM**: Copy fetches the value through an audited route; Generate mints
  server-side; unset rows read "not set" and warn "Will not deploy: …" when
  something references them.
- **form/protect** — the "who can see this" control. It is a *select*, not
  a checkbox, because a checkbox cannot say `inherit`. The password is
  never rendered back; the field placeholder says "unchanged".
- **form/fields, form/settings** — labelled inputs and the settings-section
  wrapper with its scope label (instance / org / stack / env).
- **flash** — one-shot messages after a post.
- **modal, loading, error, avatar** — plumbing.
- Later-only components: canvas (graph nodes and icons), chart and metrics,
  search, auditlist, plan, setupholding.

## v1 screens

### Login, register, invite accept

Email and password; the register and invite-accept pages differ only in
what they are joining. Invite accept shows the org being joined and the
role being granted before the button. Verdict displayed, not decided: is
registration open on this instance.

### CLI authorize

The page the `stackr login` browser flow lands on. Shows the client name,
the scopes being asked for, and the account granting them; Approve / Deny.
The write scopes must be greyed out when the granting account may not grant
them — that is `CanGrantWrite`, a service answer arriving as a field.

### Tile panel — the main screen

The tab strip over one tile: Overview, Logs, Deployments, Config, Domains,
Volumes, Provisions, Backups.

Shows: name, current state badge, image or git source with branch and
commit, the environment it lives in, its hostnames, resource limits,
replicas, restart policy, health, the live deployment if there is one.
Actions: Deploy, Stop, Restart, Rollback, Delete.

Verdicts it only displays:

- **Config-managed banner.** When the tile comes from `stackr-compose.yml`
  the page shows a "this is owned by the file" banner **and** disables the
  inputs. Both, not one: the banner alone still lets someone type into a
  field that will be overwritten. The boolean comes from the service.
- **Deploy disabled / Cancel offered** — from the deploy state, never from
  reading a status string in the template.
- **Rollback offered** only when there is something to roll back to.
- **Replicas disabled** when the tile is pinned to a home node.
- **Volume delete** offered only when the volume is detached.
- **Editable at all** — `canWrite` / `CanEdit`, an argument, never a check
  in the view.

Live parts: the state badge and the deployment strip poll today; the log
tab is already an SSE stream. All of it becomes one SSE channel.

### Deployment page

One deployment: status, trigger (who or what started it), commit, image
tag, duration, the step list, and the log stream. Cancel button, shown only
while the deployment is live — gated on the deploy-state predicate, which
is the single source for "live". Polls every 2s today; SSE tomorrow.

### Container page

The running container behind a tile: state, uptime, image digest, ports,
the log stream, restart. A stopped-containers filter, carried in the URL so
a refresh keeps it. System containers are carved out of the normal list and
of the stop actions. A websocket room today plus a slow (120s) poll as a
backstop.

### Releases and promote

The release list for a stack: commit, message, author, tag, which
environments currently run it. Promote opens a form choosing the target
environment and showing the diff before applying.

- The promote form **used to post `force: true` unconditionally**, which
  meant the panel overrode a per-environment apply policy that the API and
  the CLI respected. An override that is always on is not an override. In
  the new panel, force is a deliberate, separately-presented choice, and
  the server must refuse it from a caller without the right to it.
- Whether promote is allowed at all, and whether it needs approval, is an
  environment policy verdict. Display it; do not compute it.

### Environment compare

Two environments side by side: which release each runs, which params differ
(names and levels, never secret values), which tiles exist in one and not
the other. This is the screen that makes promote legible — keep it.

### Stack settings

Config-as-code binding (repo, branch, file path), PR-environment settings,
stack defaults, the environment list with its ladder order, danger zone.
A helper in the old template computed "which level above decided this" in
the view; that answer belongs in the settings service.

### Backups panel

Schedules for a tile or a managed instance: destination, cron, keep count,
enabled. Run history with status, trigger, size, and either the object key
or the error. Actions: run now, restore, edit, delete.

Restore is the type-the-name confirm, and the warning states that nothing
snapshots what it replaces. The history table polls every 2s **only while a
run is active** — the active check is a service answer; keep the
"only while active" part, it is the difference between a live table and a
server hammered by idle tabs.

### Managed instance panel

An instance of a managed engine: engine and version, image (with an
"overridden" marker when it is not the engine default), connection details
with the password masked, slices, consumers, storage, backups tab.

- The **file-owned banner and disabled inputs** appear here too.
- Which tabs exist at all is the engine registry's answer: data browser,
  public slices, forkable, speaks HTTP, provisionable, and what a "unit"
  and a "slice" are called for this engine. The panel must not hard-code
  Postgres words.
- The masking and the "image differs from default" test were template
  helpers; both are service answers.

### Managed provisions (slices)

Slices of an instance and who holds them. Deleting a slice warns about the
consumers that hold it. `Orphaned` on a slice means *the owning record is
gone*, not *nobody uses it* — the old template's wording conflated the two
and the warning was wrong. Name it precisely or the confirm lies.

### Managed volume

The instance's volume: size, mount, attach state, snapshot/backup links.
Delete gated on detached.

### Org settings, org defaults, org setup

- **Settings**: name, slug, members with roles, invites (link, expiry,
  whether it is address-restricted), config-as-code binding, danger zone.
  The member roster is visible only to the right role — a service verdict.
- **Defaults**: the cascade editor at org level. One knob per row with what
  applies, which level decided it, and whether this level overrides. The
  same shape at server, stack and env.
- **Setup**: a six-step wizard for a new org. Worth keeping the shape: each
  step is skippable, and the panel shows what is still unconfigured rather
  than blocking.

### Account

Profile, password, sessions, and **API keys** — mint, name, scope
checkboxes, revoke. Write scopes are disabled when the account may not
grant them. The key is shown once with that said plainly.

### Server settings

Instance-level knobs (the same cascade UI at server level), admin nav,
backup destinations section, registries, domains. Admin-only, and the nav
entry itself is gated on the verdict, not on a role string read in the
view.

### Update

Current version, an on-load check for a newer release, and the upgrade
button with a confirm saying the panel restarts. After the upgrade it shows
a waiting state that reloads itself when the new build answers, with a
"still not back — a version that fails to start rolls back on its own"
line. Keep that second line: it is the difference between waiting and
panicking. Plus the small update badge in the shell.

### About

Version, build, links. Trivial.

## Later, one line each

- **Canvas / graph** — the org, stack and project graph views and their
  node/icon components.
- **Data browse** — the managed-instance table and row browser.
- **File browse** — the volume and instance file explorers.
- **Metrics** — CPU/memory charts and the chart component.
- **Notifications** — the notification centre and its feed.
- **Search** — global search box and results page.
- **Share links** — public share pages for a resource.
- **Audit** — the audit log page and its list component.
- **Terminal** — in-browser exec into a container.
- **Staging** — the staging-changes view.
- **Plans / org config** — the config-plan pages and the plan component
  (stack plans are v1 in the API, but the *panel* comes after).
- **Nodes / server** — multi-node pages; v1 is single-node.
- **Commit log, env logs** — per-stack commit history and env-wide log
  aggregation.
- **Registry** — image and tag browser; tag delete is hidden while a live
  deployment references it.
- **Proxy** — the Traefik static-override and custom-entry editors; the v1
  equivalent is a smaller admin-only extra-Caddy-config page.
- **Tile storage** — mounting declared shares.

## The verdicts, collected

Every one of these is a boolean or a small value the old templates received
and merely rendered. They are the service surface the new panel needs, and
the layering rule says they arrive as arguments — no template may derive
them:

config-managed / file-owned; is this deployment live; is this deployment
cancellable; is there an active backup run; is this tag in use by a live
deployment; is this the last environment; is this slice orphaned; is this
container a system container; what is the live deployment; may replicas be
changed (home-node pinning); may this volume be deleted (detached); may
this user grant write scopes; is this user an owner; may this user write
here; which engine features exist and what are they called.

Five helpers in the old templates computed answers in the view — link
state, secret masking, "image differs from engine default", "which level
above decided this", and the slice-drop warning. Each is a service method
in the new code. A helper in a template is a decision with no tests.

## Notes for the builder

- Two rules produce most of the panel's correctness: **a screen never
  decides, it renders**, and **a disabled control always says why**. The
  config-managed case is the proof — banner *and* disabled inputs, because
  either alone is a trap.
- Live regions are: tile state, deployment status and steps, container
  state, backup run history, log streams. Today that is a mix of 2s polls,
  a 120s backstop poll, a websocket room and SSE. One SSE channel per page
  replaces all of it; keep the "only poll while something is actually
  happening" gate, whatever the transport.
- Filters that change what you are looking at (stopped containers, log
  level) belong in the URL so a refresh and a shared link keep them.
- Secret values never reach the DOM. Reveal and copy go through an audited
  single-value fetch. Carry that rule into the new panel unchanged — it is
  the same rule `vars get` follows in the CLI.
- Destructive confirms state what is kept, not only what is lost: "archives
  already in the bucket are kept", "data on the share is untouched". Users
  agree faster to a deletion they understand the edges of.
- DECIDE: the old panel has 68 templates for a v1 scope that needs roughly
  20 screens. The later list is not a backlog — most of it (canvas, search,
  notifications, share links, terminal) was built before anyone asked. Bin
  it until someone does.

Size: source 12958 lines, extract 276 lines

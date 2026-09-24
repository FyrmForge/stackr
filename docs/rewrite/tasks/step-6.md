# Step 6: UI

Read first: `docs/rewrite/PROGRESS.md`, then REWRITE.md "UI stack", "Build
order" row 6, "Auth" (nested URLs), "Volumes" (the card rule). Extract:
`docs/rewrite/extracts/templ-ref.md` (what each old screen shows, never
how). `docs/rewrite/verbs.md` lists the verbs the screens call.

templ + htmx, server-rendered, the URL is the state, SSE for live data,
view structs only. Web handlers are as dumb as the API's. Shared components
first; every screen is built from them.

## Custom-element whitelist (the only JS that may exist)

One file, one class, one tag each, under `ui/static/js/`, TypeScript
checked by `tsc --noEmit` in `make lint`, no imports, no fetch, DOM events
up, htmx attributes only in the templ wrapper, no shadow DOM, ~150 lines.
Adding a tag is a plan item, not a build decision.

| Tag | Does | Wrapper |
|-----|------|---------|
| `<log-pane>` | follow-scroll, level filter, search, timestamps toggle, wrap toggle over lines the SSE swap appends | `@LogPane(view)` |
| `<confirm-dialog>` | native `<dialog>` open/close, returns the answer as a DOM event the parent's `hx-trigger` listens to | `@Confirm(view)` |
| `<flash-toast>` | shows htmx error responses and flashes, auto-dismiss | `@Flash()` in the layout |
| `<theme-toggle>` | light/dark, `localStorage` only | `@ThemeToggle()` |

**Superseded 2026-09-24 for tasks 5 to 13:** the graph is v1. Read
`docs/rewrite/ui-plan.md`; it adds `<graph-canvas>`, `<graph-node>` and
`<side-drawer>` to this whitelist and replaces the page lists below with
four canvases plus drawers and dialogs. Tasks 1 to 4 (session A) stand.

Not in v1 (old JS dropped): metrics charts, terminal (xterm + websocket),
the YAML code editor with autocomplete (raw Caddy snippets and any file
view use a plain `<textarea>` / `<pre>`).

## Tasks

1. **Vendor htmx + SSE extension**, pinned, under `ui/static/js/vendor/`.
   `tsc` config, `make lint` runs `tsc --noEmit`, `templint` allowlist gets
   the four tags.
   Done when: lint passes with an empty element file for each tag.

2. **Layout + helper.** `ui/components/layout.templ` (shell, nav by URL,
   flash slot), one helper that reads `HX-Request` and renders full page or
   fragment from the same handler, `hx-push-url` on navigation. Web router
   mounts the step 1 middleware on `/:org/:stack/:env/:tile`.
   Done when: one page renders both ways in a test.

3. **Component set** in `ui/components/`, one templ file each: form (field,
   error, submit), table, status badge (tile state, job state, env colour),
   card (tile card, volume card stacked under its mounting tile or alone),
   dialog wrapper, log pane wrapper, job status (SSE), release diff/plan
   view, param editor (masked secret, merge semantics), settings form built
   from the catalogue, empty state, pagination.
   Done when: a components gallery page renders every one.

4. **Custom elements**, the four above, TypeScript.
   Done when: each under budget, no fetch, no import, events only.

5. **Auth pages:** login, setup (first admin), invite accept, CLI authorize,
   account (password change, keys).

6. **Org pages** `ui/pages/org/`: list/switch, settings (rename/delete with
   rule verdicts), members + invites, API keys, connector (GitHub App
   install), org params, backup destinations.

7. **Stack pages** `ui/pages/stack/`: overview (envs across, tiles down),
   releases (list, diff, promote button with dry-run plan, rollback),
   settings (`from`/`auto` per env, defaults), stack params, config file
   pointer.

8. **Env pages** `ui/pages/env/`: tile + volume cards, env params, logs,
   settings, PR env badge.

9. **Tile pages** `ui/pages/tile/`: state + last job (SSE), logs, domains
   (+ named extras, admin raw snippet), env block editor, settings,
   jobs, image-watch chip + "check now" + apply, managed instance tab
   (provisions/slices, bindings), volumes mounted, backups (schedules,
   runs, restore with target picker), restart/stop/deploy actions.

10. **Admin pages:** server settings from the catalogue, users, update
    (check/run/badge), raw Caddy, panel backup.

11. **Handler audit.** Run the skill over `ui/`; empty report. templint
    clean.

12. **Playwright smoke** (see memory `verify-ui-with-playwright`): login →
    create org → stack → env → deploy an image tile → promote dry run →
    promote → rollback, on the VM.

13. **Done gate.** build/lint/test/templint pass; smoke green on the VM;
    PROGRESS.md ticked; stacked PR.

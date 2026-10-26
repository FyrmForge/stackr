# Admin setup wizard (plan, 2026-10-22)

Status: draft for darthvader's review. One helper, one branch off master.

## Why

The v0.8.3/0.8.4 run-throughs showed an admin has to know where things live:
observability is off until found in Admin, a backup destination added at
setup can't be used by orgs until shared elsewhere, a GitHub App can't be
brought back without database surgery. The fix is a guided first run that
explains each server-level choice: what it is, what you get with it, what
you get without it, and a default.

## Today

- `internal/service/serversetup.go`: wizard state in settings
  (`server_setup_done_at`, `server_setup_mode`), branches `ui` and `config`.
- `internal/web/handler/setup/server.go`, `internal/ui/pages/setup/server.templ`:
  the `ui` branch is one page with three sections (Domains, Backup
  destination, Placements). `ServerPending` sends admins here until done.
- The org wizard (`internal/ui/pages/setup/setup.templ`) has the stepper to reuse.
- The config-file branch stays as it is.

## Steps (by-hand branch)

One screen each under the stepper, `/-/setup/server/ui/<step>`. Every screen
but Domains has Skip. Next saves and moves on. No stored progress: the URL is
the position; `ServerPending` still sends an unfinished admin to the start.

1. **Domains.** Panel domain, root domain, ACME email (today's form).
2. **Certificates.** Explains: each domain gets its own certificate
   automatically. Wildcard certificates need a DNS provider; one line that
   DNS providers are coming. Info only until DNS connectors land.
3. **GitHub.** Create a server GitHub App, shared with every org (the
   existing manifest flow in `internal/web/handler/admin/connectors.go`,
   returning to the wizard), or use an existing App: a form for app id,
   slug, private key, webhook secret, client id and secret, saved as a
   server connector shared with every org, showing the webhook URL the App
   must point at. Skip = each org adds its own later.
4. **Observability.** Two short columns: with the `stackr-observe` container
   (30 days of graphs, log search, access logs; uses memory and disk, the
   caps) vs without (24 hours of live graphs, no log search). One switch,
   on by default; retention and caps prefilled. Catalogue default for
   `observe_enabled` becomes true, so installs that skip the wizard get it too.
5. **Backups.** Add an S3 destination (today's form) with "Let orgs use it"
   ticked by default, then the panel backup switch, schedule and that
   destination. Without: says what is lost if the box dies.
6. **Nodes.** One box works alone. How to add a worker (the panel's Add
   node with its one-line command), what placements are, the current
   placements list. Link to Admin, Nodes.
7. **Summary.** Each step: on, off or skipped, with a link to where it lives
   in Admin. Finish stamps `server_setup_done_at`.

## After setup

Admin gets "Setup guide" that reopens the same screens at any time (the
done stamp stays; steps no longer redirect away when done).

## Copy

Plain, terse, no em dashes. Panel first; the CLI mirror is the existing
settings and connector commands, no new CLI.

## Check

- Handler tests per step (save, skip, validation errors), `ServerPending`
  still gates, revisit works when done.
- The existing-App form refuses a bad private key and stores the connector
  shared.
- Observe default true in a fresh settings read.
- Then a clean rig install from the next dev build, walked through in a
  browser.

## Out of scope

DNS providers (step 2 gets them with DNS connectors), email/SMTP, the
config-file branch.

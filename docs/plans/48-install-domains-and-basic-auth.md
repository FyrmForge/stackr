# Plan: root domain, trusted proxies at install, basic-auth URL protection

Status: built and rig-verified 2026-09-17.

Working rules: discuss first, one point at a time, no code without a go, no
git writes, no edits to `*_templ.go` or `output.css`, terse UI copy.

## Why

Installing on `fyrmforge.dev` with the panel on `panel.fyrmforge.dev` should
let stackr hand out names under `fyrmforge.dev`, not under the panel's name.
Today every app name hangs off the panel host, and the panel's session cookie
is scoped to that host, so every tenant app under it receives the admin's
session cookie. That cookie scope exists only for "protect preview URLs",
which checks a stackr session through Traefik `forwardAuth`.

## Decisions (2026-09-17)

1. **Root domain prompt.** `install.sh` asks for the panel hostname and a root
   domain (default: the panel host's parent). Passed as `ROOT_DOMAIN`. On
   first boot the panel creates it as the instance-level domain resource, the
   setup wizard suggests `<org>.<root>`, and the installer's DNS text asks for
   `*.<root>`.
2. **Protect URLs becomes basic auth.** No stackr accounts involved. It covers
   every domain a tile has, not only generated ones. Switched on through the
   settings cascade (server, org, stack, env, tile). The session `forwardAuth`
   (`/_stackr/authcheck`, `stackr-auth@file`) is deleted.
3. **The session cookie is host-only.** No Domain attribute; no app hostname
   ever receives it.
4. **Credentials sit with the toggle.** Each level can set user and password;
   the nearest level that sets them wins. The tile's existing basic auth is
   the tile level of the same thing.
5. **Password is a plain string** in the panel and the config file, and may
   be a `${{ }}` reference (e.g. `${{ stack.secrets.PREVIEW_PASS }}`). Stackr
   bcrypt-hashes it when it writes the Traefik config. The hash field goes.
6. **Trusted proxies at install.** Two prompts: `Behind Cloudflare? [y/N]` and
   `Other trusted proxy CIDRs`. Passed as `TRUST_CLOUDFLARE` and
   `TRUSTED_PROXY_CIDRS`, copied into the plan 47 settings on first boot only;
   the Proxy page owns them after. The existing `TRUSTED_PROXIES` (panel
   trusts Traefik) is unrelated and stays.

No backwards compatibility: config keys and columns are renamed or removed
outright.

## Build

### A. Installer (`scripts/install.sh`)

- After `Panel hostname`: `Root domain [<parent of host>]`. Skipped for
  localhost / an IP. Must equal the panel host or be a parent of it
  (`panel.fyrmforge.dev` under `fyrmforge.dev`); re-asks otherwise.
- `Behind Cloudflare? [y/N]`, then `Other trusted proxy CIDRs` (comma
  separated, blank for none, each checked as a CIDR).
- `--env ROOT_DOMAIN=`, `TRUST_CLOUDFLARE=`, `TRUSTED_PROXY_CIDRS=` on the
  service.
- DNS text: `panel host A`, `*.<root> A`.
- `docs/host-setup.md` install section lists the new questions.

### B. First-boot seeding (`cmd/stackrd/main.go` + a small seed func)

- `ROOT_DOMAIN`: when no instance-level domain resource exists, create one
  (same row `handler/server/handler.go` creates by hand, `Level: "instance"`,
  owner = local server). Uses `envops.HostTaken` first.
- `TRUST_CLOUDFLARE` / `TRUSTED_PROXY_CIDRS`: write `trust_cloudflare` /
  `trusted_proxies` only when the setting row does not exist yet. Cloudflare
  fetch failure at boot: log, leave the box off (plan 47 rule: never silently
  empty).
- `handler/org/setup.go` `setupDomainPrefill`: prefer the instance domain
  resource over the BASE_URL host.

### C. Cookie

- `cmd/stackrd/main.go`: drop `auth.WithCookieDomain(baseDomain)` and the
  `CookieDomain` passed to handlers. `handler/auth/cookies.go` keeps reading
  `sm.CookieDomain()`, which is now empty (host-only).

### D. Remove session protection

- `infra/proxy/proxy.go`: `stackr-auth` middleware, `authCheckPath`,
  `protectAuto` branch in `WriteApp`.
- `handlers/web/server.go`: `/_stackr/authcheck` route and `authCheck`;
  `authcheck_test.go`.
- `handlers/web/handler/server/handler.go`: `cookieDomain`, `CookieCovers`,
  `cookie_test.go`, and the lock-out warning in `server.templ`.

### E. Basic-auth protection

- `config/settings/settings.go`: `ProtectAutoDomains` becomes `Protect *bool`,
  plus `ProtectUser *string`, `ProtectPassword *string`. Resolved as one unit:
  user and password come from the nearest level that sets either. Tests in
  `settings_test.go`.
- Tile level: `repo.Tile` / migration `001_initial.up.sql`
  `basic_auth_hash` becomes `basic_auth_password` (plain). Everything reading
  the hash follows: `sqlite/tiles.go`, `api/v1/apps.go`, `handler/app`
  (form + `app.templ`), `stackconf` (`stackconf.go`, `plan.go`, `apply.go`,
  `serialize.go`), `orgconf/export.go`.
- `infra/proxy/proxy.go` `WriteApp`: resolve protection for the tile (tile
  fields over cascade). When on, resolve `${{ }}` in the password with
  `varref`, bcrypt it, emit one `basicAuth` middleware first in the chain for
  every router of the tile.
- **Fail closed:** protection on but no user, or a password reference that
  does not resolve, writes a `basicAuth` with a random throwaway password and
  logs why. The URL is locked, never left open.
- UI: server settings toggle becomes toggle + user + password. Org, stack and
  env settings pages get the same three fields where their cascade forms
  live. Tile form keeps its user/password fields, password now plain.
- Config file: `protect`, `protect_user`, `protect_password` at the levels
  that take settings today; tile `basic_auth_user` / `basic_auth_password`.
- `docs/features/config-as-code.md`: replace `protect_auto_domains`.

Known and accepted: a plain (non-reference) password is stored unencrypted in
the settings JSON. It gates dev sites; use a `${{ }}` secret for anything
more.

## How it was built (differs from above)

- Seeding runs once, marked by the `install_seeded` setting, not "when the row
  is absent": the settings store cannot tell absent from empty, and a domain
  the operator deleted must not come back on restart.
- `protect` is a select (inherit / on / off), not a checkbox: a checkbox
  cannot say inherit, so saving an env form would pin it off.
- Tile level: a tile with its own `basic_auth_user` is protected with its own
  credentials whatever the cascade says; it cannot opt out of protection set
  above.
- bcrypt at cost 5 (Traefik checks every request), cached per password so a
  resync does not rewrite every route.
- Org defaults save, the settings API and config applies now resync routes
  too; before only server, stack and env saves did.
- The installer asks the root first (localhost or an IP skips the rest), then
  the panel host, default `stkr.<root>`, which must be under the root.
- Password inputs are plain text fields showing the stored value (it may be a
  reference).

## Tests

- `settings_test.go`: cascade of the three fields, nearest level wins.
- `writeapp_test.go`: protection on emits basicAuth on every router incl.
  hand-attached domains; unresolvable password fails closed; off emits none.
- Seed: instance resource created once, second boot no-op; trusted proxy
  settings written only when absent.
- `install.sh`: root validation cases in the fake-docker style used for plan 16.

## Verify on the rig

Wipe, install with `panel.stackr-test.vulpe.dev` / root `stackr-test.vulpe.dev`
(the only wildcard the rig has), Cloudflare no. Wizard suggests
`<org>.stackr-test.vulpe.dev`. Deploy a tile, protect at org level with a
secret reference: browser gets a basic-auth prompt, right password gets in,
panel cookie absent from the app request.

Verified 2026-09-17: installed from `install.sh` (pulls stubbed, local images)
with root `stackr-test.vulpe.dev`, panel default `stkr.stackr-test.vulpe.dev`,
CIDR `100.64.0.0/10`. LE cert issued for the panel; instance domain and
`trusted_proxies` seeded, Traefik `trustedIPs` set. Wizard prefilled
`test-org.stackr-test.vulpe.dev`; tile got
`web.demo.test-org.stackr-test.vulpe.dev`. Org-level protect with
`${{ org.secrets.PREVIEW_PASS }}`: 401 without or with a wrong password, 200
with the secret's value, panel untouched. `session_token` is host-only on the
panel host.


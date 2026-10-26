# Connectors UI rework (plan, 2026-10-23)

Agreed with darthvader 2026-10-23 after the DNS design review (screens in
the session scratchpad `dns-design/`).

## Layout

- Connectors tab (Admin and org) is only a list of registered connectors.
  One row each: kind icon/label, name/account, short summary (e.g.
  "2 managed domains", "shared with 3 orgs"), a cogwheel. Empty state plus
  "+ Add connector".
- "+ Add connector" opens a floating guided dialog (modal, stepper):
  1. Pick the kind: GitHub, Namecheap (Cloudflare and custom later; the
     picker is a list so new kinds are one entry).
  2. That kind's steps, one screen each. GitHub: org, then the manifest
     flow to GitHub and back. Namecheap: turn on API access, username and
     key, allowlist the server IPv4, check connection, pick domains.
- Cogwheel opens a dialog with that connector's settings. For a DNS
  connector this holds the domain list (Manage, Take over DNS, Stop
  managing, Available to) and Remove. For GitHub: sharing and Remove.
- Setup wizard Certificates step reuses the same add dialog, opened on
  Namecheap.

## Fixes folded in

- Empty submit names every missing field; messages start with a capital.
- Server IPv4 has no fake placeholder IP; prefill question stays open.

## Not this round

Cloudflare, custom connectors, IP auto-detect behind NAT.

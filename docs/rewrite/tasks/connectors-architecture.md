# Connectors architecture (plan, 2026-10-23)

Agreed with darthvader 2026-10-23, point by point in chat. Goal: one small
pluggable surface; no kind-specific code spread through the repo.

## Agreed shape

1. One connection per account. A connection's kind (github, namecheap,
   later cloudflare, forgejo, ...) offers capabilities; each is switched on
   or off on that connection. Never "Cloudflare DNS" + "Cloudflare Tunnel".
2. Capabilities are small Go interfaces in `internal/connectors/capability`.
   A kind has a capability by implementing it. Features ask by capability
   ("the DNS connection managing fyrmforge.dev", "the Git source for this
   repo"), never by kind. Connectors may use other connections the same way.
3. Kinds are registered in one file, `internal/connectors/all.go`, the only
   place outside a kind's own package that names a kind. A test fails if
   anything outside `internal/connectors/` imports a kind package.
4. UI ownership: the kind owns its setup steps (fields, hints, order); the
   capability owns its section in the connection's settings dialog (DNS owns
   the domain list, so Cloudflare gets it for free).
5. Storage: one connectors table: id, scope (server/org), kind, enabled
   capabilities, sharing, and one encrypted settings blob per kind (no
   per-kind columns).
6. Setup steps are forms or "sign in with X" (OAuth). Core owns OAuth
   plumbing (state, PKCE, one callback route, encrypted token storage,
   refresh). A kind declares endpoints, scopes and which app: its own per
   install (GitHub App), a FyrmForge public client via a bounce page, or
   none (pasted key fallback).

## Capabilities in v1

- DNS: Zones, Get, Append, Set, Delete, TakeOver (today's dnscap.Provider).
- Git source: Repos (picker), Repo URL (owns and normalises a URL),
  clone access (short-lived login for one repo), push events (provider
  webhook in, "repo X, branch Y, commit Z" out). Optional extras later:
  CI status, commit status, PR previews.

## Phases

1. Core + Namecheap: `internal/connectors` (registry, capability
   interfaces, storage), Namecheap moved onto it; dialog, CLI, API and
   wizard drawn from the kind's declared steps. Both step shapes exist from
   here: form, and redirect out and back through one core callback route
   (GitHub's manifest flow uses it in phase 2). All DNS behaviour and tests
   unchanged.
2. GitHub onto the Git source capability: RepoURL, CloneAuth, webhook
   decoding and App fields move into the github kind; core sees only the
   capability. Repo URL checks go through the capability, so GitLab/Forgejo
   URLs are not refused by a GitHub parser.
3. OAuth token plumbing (token storage, refresh, PKCE, FyrmForge bounce
   page): built with its first user (Cloudflare), not before.

## Backlog

Cloudflare (DNS, Tunnel, proxy), Forgejo/GitLab, third-party connectors
(trust model to discuss), private admin access (Tailscale/WireGuard/NetBird
with a break-glass), provider connectors (Hetzner, DO, Proxmox), folding
registry credentials and backup destinations into connectors.

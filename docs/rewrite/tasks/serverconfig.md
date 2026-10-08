# Server config as code (stackr-server.yml)

Status: design agreed with darthvader 2026-10-07; built 2026-10-07 (blitz
waves 0 to 2), rig is wave 3. Seams: `serverconfig-seams.md`.
Grounding: Fable survey of the org file code (this session). Mirrors the
org file: `internal/service/orgconfig.go`,
`internal/service/internal/flow/orgconfig/{file,diff,export}.go`,
`leaf/orgplan`.

## Agreed
1. **One file, one machinery.** `stackr-server.yml`, planned, approved,
   applied and exported like `stackr-org.yml`. Only server admins approve.
   Fed by a bound repo (push plans it) or by the CLI
   (`stackr server plan|apply <file>`) from any machine with an admin key.
2. **Local apply stays allowed** once a repo is bound: the plan is tagged
   "from a local file, not the repo" and needs the same approval. It is
   the bootstrap and the DR path (fresh box, apply, orgs rebind).
3. **Server connectors.** A connector may belong to the server
   (`connectors.org_id` nullable). Shared with none (default), named orgs,
   or all orgs; only an admin or the server file shares. "All orgs" lets
   every org owner clone every repo the App is installed on (warn).
   GitHub App connectors are made in the panel only (browser flow); the
   file names them.
4. **Orgs from the file.** The file creates orgs, binds each to its org
   file (repo, path, connector) and lists the server connectors it may
   use. A file-created org's owner is the approving admin. Org file plans
   are still approved by the org's owners.
5. **Server params.** A `server` params scope with params and secrets.
   The file declares secret names only; items reference
   `${{ server.params.<col>.<name> }}` (as org shares use `org.params`).
   A missing value blocks only that item. Room for later connectors that
   need a token (SMTP2GO).
6. **Panel edits allowed.** No lock, no "managed" tag (the org side has
   none). The next plan shows the drift; approving puts the file back.
7. **Never delete on its own.** Anything live the file no longer names
   (route, global backup dest, instance domain resource, connector share,
   org binding) is a "remove?" row in the plan, unticked by default.
   Approving applies the rest; a ticked row applies its removal, still
   refused while something uses it (a dest a schedule or
   `panel_backup_dest` uses, a domain resource tiles sit under, a
   connector a binding names). Orgs and server params are never removed.
8. **Every setting is allowed**, the risky ones included (panel domain,
   root domain, DNS provider, proxy custom). The plan carries an impact
   line per risky change ("panel moves to X; point DNS first",
   "redeploys N tiles", "wildcard certs need the DNS token on the proxy",
   "moves N hosts, issues N certificates"); approving a plan with impact
   lines asks "are you sure?".
9. **Root domain changes** become real: root domain is only the seed of
   the instance domain resource (`SeedInstance`), so a change is a rename
   of a domain resource. The rename rewrites every auto/apex host it named
   (`domains.resource_id`), leaves literal hosts, re-pushes the proxy, and
   keeps each old host as a 308 redirect to its new one until removed (it
   dies anyway once DNS moves). Org domain resources rename the same way.
10. **Auto-apply.** A switch, off by default; when on, only a plan with no
    impact lines and no removal rows applies by itself.
11. **Export** writes the live server as a file: secrets by name, dests
    with key refs, connectors by name with their shares, orgs with their
    bindings. Export then plan reads clean.

## In the same plan
- Remove the dead `dns_env` knob (nothing reads it).
- Wire `proxy_custom` into the Caddy config (today it saves and does
  nothing, DECIDE 127), since the file may set it.

## Also agreed
- The org file's `shares:` delete becomes an unticked removal row too.
- One plan component and one approve contract for server and org plans
  (impact lines, removal ticks, confirm enforced in the service).
- Creating an org is an impact line (an auto-applied plan has no approver
  to own it).
- An org's connector list (canvas cards, `org connectors ls`, API) shows
  the server connectors shared with it, marked shared; their card is
  read-only and has no install links (2026-10-08).

Builder plan: `serverconfig-blitz.md`.

## Technical notes from the survey
- Locks: `serverconfig` + `serverplan:<id>`; add `orgconfig:<org>` for
  each org the apply touches. Walk order: connector shares before org
  bindings (an org plan needs its connector).
- Webhook (`/hooks/:connector`, `internal/service/stack.go` `Webhook`)
  uses `c.OrgID`: a server connector fans out to the server plan and every
  org it is shared with.
- `SetOrgConfigRepo` and `orgLive.Connectors` must accept shared server
  connectors; connector lookup by host needs a rule (org's own first, then
  shared server ones).
- `Change`, `strictYAML`, `checkParams` are copied per flow with a "third
  user moves it" note; the server file is the third user.
- `SetSettings` writes the row before the proxy push; a push failure must
  fail the apply step (it does, the verb returns it).
- Param refs (`leaf/params/ref.go`) knew `org.params` only: `server.params` is
  now a kind, resolved in the server file's items only (S2, 2026-10-07).

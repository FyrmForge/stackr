# infra/proxy + service/proxy — reference

Source: `infra/proxy/{proxy,protect,trusted,probe,accesslog}.go`, `service/proxy/proxy.go` (tests read: `infra/proxy/{writeapp,protect,trusted}_test.go`, `service/proxy/proxy_test.go`)
Commit: c2423f0
Taken (as prose, reference only): basic-auth cascade and fail-closed rule, trusted-proxy list + Cloudflare ranges, the ACME account model and the DNS-01/wildcard path, how a domain maps to a tile in an environment, redirects, the raw operator snippets, the exact field set a route needs from a tile, the push-serialisation rule
Cut: the Traefik file writer itself — YAML rendering, `atomicWrite`, `dynamic/` layout, orphan pruning, `Resync`'s file sweep, the route probe + restart loop, the access-log tail reader, the Traefik container/service spec
Cuts belong to: `service/internal/proxy` (one Caddy admin push replaces the whole file-provider apparatus); the container spec to the installer; the access log to a later logs wave

Target: `service/internal/leaf/domain` (turns a tile + its domains into Caddy
JSON) and `service/internal/proxy` (the admin-API client that pushes it). No
code is carried over — Caddy's JSON config has no relation to Traefik's YAML.
What is carried over is every decision the old renderer made, below.

---

## 1. Basic auth

**Where the credentials come from.** Two sources, tile wins. The tile row's
own user/password if the user is set; otherwise a settings cascade resolved
for that tile — levels `server → org → stack` and then the tile, nearest
answer winning — giving `Protect`, `ProtectUser`, `ProtectPassword`.
Protection is on when a user exists at whichever level answered.

**The cascade has two readers and only one is safe here.** The settings
package offers a try-variant that returns its error and a for-variant that
swallows it and returns zero values. Protection must use the erroring one,
and the test asserts exactly that, because the swallowing one turns an
unreadable stack row into `Protect == false` — an open URL.

**The password may be a `${{ }}` reference** into the param store, expanded at
render time with system scope.

**Fails closed, three ways, and this is the whole point of the section:**

- Protection on but user or password empty → the route is still written, with
  user `locked` and a bcrypt hash of 24 random bytes nobody has. Never an open
  URL.
- A reference that will not resolve → same lock, plus a log line.
- bcrypt refuses a password over 72 bytes (a `${{ }}` expansion easily
  exceeds it) → same lock. Specifically **not** a sentinel string: Traefik
  compared an unrecognised entry as plain text, so `"!"` would have *become*
  the password. Whatever Caddy's equivalent, the rule is that the stored value
  must always be a real hash.
- A cascade that cannot be read is an **error returned to the caller**, not an
  unprotected tile. The caller then leaves the previously written route
  in place — rewriting it without the auth would publish the URL until the
  next successful sync. The test proves it byte-for-byte: render once with a
  working store, break the store, render again, assert the error *and* that
  the protected config on disk is unchanged.

**Two caches, both there to keep the config byte-stable.** bcrypt salts
differ per call, so a re-render with the same password would produce a
different config and force a reload on every unrelated settings save. So:
the lock hash is computed once per process (`sync.OnceValue`), and real
hashes are memoised by sha256 of the password. Test asserts a rewrite
produces identical bytes.

**Cost 5, deliberately** (`htpasswd -B`'s default). The proxy checks the hash
on *every* request, assets included, and this gates dev sites, not banks.

**Ordering.** Auth is the first handler on the route, so a request about to
get a 401 skips the security-header and operator middlewares. It applies to
**every** route of the tile, hand-attached domains included — the test counts
the occurrences precisely to catch a domain slipping past.

**The other built-in preset** is a security-header toggle on the tile
(`SecHeaders`): HSTS 31536000 with subdomains, nosniff, frame-deny.

## 2. Trusted proxies

Two settings: a free-text CIDR list and a "trust Cloudflare" flag.

- **Parsing is one policy for all callers** (panel form, API, installer seed).
  Split on newlines *and* commas, trim, skip blanks, a bare IP becomes `/32`.
  **One bad line refuses the whole save** — the installer used to skip it with
  a warning, and a dropped CIDR is a trusted proxy that is not trusted, which
  surfaces much later as every client IP being the proxy's, in the rate
  limiter, the access log and the audit trail.
- **Cloudflare ranges** are fetched from `https://www.cloudflare.com/ips-v4`
  and `/ips-v6`, whole fetch capped at 3s. Any line that is not a CIDR fails
  the whole fetch, so a maintenance HTML page never replaces a good cache; an
  empty result also fails. Success replaces a cached copy in settings;
  failure leaves the cache alone and logs.
- **The cached list is sorted before storing.** Cloudflare reordering its own
  list must not read back as a changed config.
- On save with the flag on: refresh; if the refresh fails *and* the cache is
  empty, refuse the save with "could not reach Cloudflare"; if a cache exists,
  warn and keep it.
- At render time the typed list and the cache are merged in that order,
  deduped, order preserved, non-CIDRs dropped silently. Empty list → the key
  is omitted entirely so the config stays byte-identical to a fresh install.

In Caddy this is `servers.trusted_proxies` (a `static` range source), set on
both listeners, and it is what makes `{http.request.remote.host}` the real
client. Same input, same refusal rules.

## 3. ACME

**There is no staging/production switch and there never was.** Only the live
directory, plus a global off switch. Do not invent a staging toggle for
parity; if one is wanted it is new work.

**The off switch** is `STACKR_TLS=off` (opt-in only, never inferred from a
missing ACME email — keying it on that would silently strip HTTPS from every
host that never set the variable, on the next config write). With it: no
resolvers, no certificates, no custom certs, no HTTPS redirect, every route on
the plain-HTTP listener only. Meant for throwaway/LAN boxes no CA can reach.
The renderer also stops publishing 443 at all, because a busy 443 on the box
would otherwise stop the proxy starting.

**Accounts are per email address, not one per install.** The instance address
is always the resolver named `le` (stable so an existing certificate is not
re-issued). Every *domain resource* row may name its own ACME email; each
distinct extra address becomes its own account, named `le-<first 4 bytes of
sha256(email), hex>` with its own storage file. Derived from the address so
the same address always maps to the same store, across restarts and across
resources that share it. Sorted, so the generated config is stable.

Why: a single instance-wide address sent every tenant's expiry notices to the
operator, and a customer bringing their own domain could not bring their own
account.

**Which account a host uses: longest suffix match.** Take every domain
resource that names an email; a resource matches when the host equals its base
or ends in `.` + base (`*.` stripped from both sides first); the longest base
wins; no match → `le`. Resources nest — an org owns `example.com`, a stack
owns `shop.example.com` — and the stack's account is the nearer answer.

**Challenges.** HTTP-01 on the plain-HTTP listener is the default for every
account. DNS-01 exists only as **one** extra instance-wide resolver (`ledns`)
configured with a provider name plus credential env lines, and it is used for
exactly one thing: wildcard hosts. A wildcard domain gets `main: <base>`,
`sans: ["*.<base>"]` on the DNS resolver. An unset provider means wildcards
are off, and that same setting also gates whether the panel accepts a wildcard
domain at all — one read, one answer.

Credentials are passed to the proxy process as environment, so a credential
edit is invisible to a plain config diff; the old code hashed the env lines
into a comment in the static config so an edit still counted as a change.
**In the new shape the proxy is a config sink with a Docker-less container,
so credential env belongs to the container spec, and changing it is a
container recreate, not a config push.** Flag for the builder.

**Custom certificates** (operator-supplied PEM + key on the domain row) beat
ACME entirely: the domain is served from the supplied pair, matched by SNI,
and gets no resolver. Ignored when TLS is off.

## 4. Domains, environments and what a route points at

**The join.** A domain row belongs to a tile; the tile belongs to an
environment. The route's upstream is therefore *the tile in that environment*,
and nothing in the domain row names the environment — the tile does.

**The upstream name must be globally unique, not env-unique.** One proxy sits
in front of every environment, so a plain tile slug (unique only inside its
env) would collide. The old alias was `tile-<first 8 of tile id>`, and the
same function had to exist in the network layer so the container actually
carried that alias. Upstream = `http://tile-<id8>:<domain.ContainerPort>`.
The port is per **domain**, not per tile — the same tile can publish two
hosts on two ports (the test does exactly that).

In the new shape the upstream list is the tile's **replica names on its
ingress network** and Caddy does the health checking, so the alias is no
longer a VIP — but the "stable name, unique across environments, chosen by
stackrd not by the proxy" rule stands.

**Nothing crosses environments.** A domain never resolves to a tile in another
env; there is no env selector in a route. Env-awareness is entirely in *which
tile id the domain row hangs off*.

**Wildcards.** Host `*.example.com` matches exactly one label
(`^[^.]+\.example\.com$`, dots escaped) — not a greedy suffix match. Its
certificate comes from the DNS-01 resolver as described above.

**Path.** A domain may carry a path prefix; `""` and `/` mean no prefix, any
other value narrows the match to that prefix.

**Priority.** An explicit integer on the domain row, emitted only when
non-zero. Needed because a tile can attach two rows for the same host where
one is a narrow rule that must win (the test case is an OIDC discovery path).

**A declared raw matcher** (`domain.Rule`) replaces the *matcher only*. The
host is still what picks the certificate — a rule entry gets its host's
resolver like any other.

**Redirects.** `RedirectTo` turns the row into a permanent 301 from
`^https?://<host>/(.*)` to `https://<target>/${1}`. Three rules worth keeping:

- the target is always `https`, because an http redirect target is rare enough
  to hand-edit through the raw override;
- the source host is regex-escaped;
- **a redirect route carries no auth and no security headers** — the 301 fires
  first anyway, and the test explicitly asserts the auth middlewares are not
  inherited.

**Serving TLS and forcing it are two separate questions.** `HTTPS` means the
host gets a certificate and a TLS listener route. `ForceHTTPS` means the
plain-HTTP route becomes a permanent redirect to https. With `HTTPS` on and
`ForceHTTPS` off, the plain-HTTP route still *serves the tile*, so a legacy
client or a health check that cannot follow a redirect still gets an answer.
Dedicated test.

**Self-routes are generated, never configured.** The panel's own hostname and
the managed registry's hostname get routes emitted from the running
configuration on every boot, not from rows a user edits. Reason: giving a tile
a domain requires the panel, so a panel whose own route lived only in its
database was unreachable after a data wipe — you would need the panel to reach
the panel. The panel route is skipped for a bare IP, `localhost`, and an unset
base URL (and any file an earlier boot wrote is removed), because a LAN box
has no name to route and no certificate to get. The backend is the panel's
*internal* address, not its public base URL — the public one may not resolve
from inside, or may route back through the proxy.

## 5. Raw operator snippets

Four escape hatches, all replace-not-merge:

1. **Per-tile override** — a raw config blob on the tile row that replaces the
   tile's whole generated route, verbatim. The database stays the source of
   truth; the render is just a projection. A tile with an override and no
   domains still gets a config.
2. **Instance-wide static override** — a raw blob that replaces the entire
   generated global config, verbatim. Editing it must count as a change like
   any other, so it is substituted *before* the change comparison.
3. **Named dynamic entries** — a `name → raw yaml` map in settings,
   materialised one file per entry and pruned when an entry is deleted. Name
   is slugified, empty name refused, body must parse as YAML or the save is
   refused. This is the hatch REWRITE names for unprotected routes, WebDAV and
   routes to LAN IPs.
4. **Stack middlewares** — a stack row carries a `proxy.middlewares` map of
   raw handler bodies. Only the *names* are rewritten, to
   `stk-<stack id8>-<name>`. Keyed on the stack **id**, never its slug: slugs
   repeat across orgs and change on rename, and either would silently point a
   route at another org's middleware or at nothing.

   A domain references one as a bare `name` (its own stack) or `stack/name`
   (another stack in the same org). When the other stack is gone, the
   reference is emitted as a deliberately unresolvable `missing-…` name so the
   route **breaks loudly**: a dead route beats a `forwardAuth` guard silently
   dropping off a public one. Operator middlewares are appended *after*
   stackr's own.

All four bodies are operator-authored config. Validation is "does it parse",
nothing more.

## 6. What a route needs from a tile

The complete input set, which is what `leaf/domain` should take as arguments:

- **Tile:** id (the upstream name and the file key), stack id (middleware
  name resolution), kind (`cron` and `function` never serve HTTP and are
  skipped at the service boundary, not at each call site), basic-auth user and
  password, security-header flag, raw override.
- **Per domain:** host, container port, path prefix, raw rule, priority,
  redirect target, https flag, force-https flag, cert PEM + key PEM,
  middleware reference list, and an "auto" marker distinguishing a generated
  hostname from a hand-attached one (it changes nothing in the render — every
  route gets the same treatment, and a test asserts that).
- **Install-wide:** instance ACME email, the domain-resource list (host +
  ACME email) for account selection, the DNS-01 provider, the TLS-off flag,
  the trusted-proxy list, the panel's public host and internal URL.

## 7. Pushing the config

The old code wrote files and hoped. Two things it learned are worth keeping in
the admin client:

**Serialise and coalesce.** Rewriting the global config restarts the proxy, so
two concurrent saves must not race. One run at a time; callers arriving while
a run is in flight collapse into **exactly one** follow-up run, never zero and
never five. Zero is the bug that matters: a save made mid-run was silently
dropped, the panel said "saved" and the setting never reached the proxy until
the next restart. Both halves have dedicated tests.

**Boot returns its error; everything else logs.** Boot goes through the same
slot (it runs in a goroutine while the HTTP server is already accepting, so an
operator saving settings during the image pull is a real window) but hands the
error back, because nothing is serving yet and a proxy that will not come up
is worth reporting. A handler-triggered push must not run on the request
context: a client disconnecting used to cancel the reconfigure half way.

**Everything that can fail runs before the swap** is the same rule the upgrade
flow uses.

---

## Notes for the builder

- **The probe is cut and should stay cut.** The old code could not tell
  whether the proxy had picked up a file, so it sent itself an HTTP request
  with a forged `Host` header and treated a 404 as "config never landed", then
  restarted the container — a real failure (deleting the bind-mounted data dir
  under a running proxy kills the inotify watch permanently, seen live). A
  Caddy admin push either returns 200 or returns an error. That whole class of
  bug is gone; do not port a probe.
- **`Resync` becomes one push, not a sweep.** Everything the old boot path
  reconstructed file by file — every tile's route, stack middlewares, named
  entries, the registry route, the panel route, plus pruning files for tiles
  that no longer exist — is, in Caddy, "build the whole config from the
  database and PUT it". Orphans cannot exist. REWRITE already requires this
  push whenever stackrd sees the proxy container start.
- **Failure policy on a full rebuild:** one tile whose settings will not read
  must not stop the rest, at boot least of all. The old loop logged, kept the
  previously written (protected) route for that tile, collected the slugs and
  returned one error naming them. Worth keeping — but note a single Caddy PUT
  is all-or-nothing, so the equivalent is to drop the failing tile's routes
  from the pushed config, not to abort the push.
- **The access log is not in this target.** The old reader tailed the last 2MB
  of the proxy's shared JSON access log and filtered by route-name prefix to
  get one tile's recent requests (method, path, status, ms, client IP,
  preferring origin status over downstream). It only worked because route
  names encoded the tile id. If the panel keeps that view, Caddy's access log
  needs the tile id in the log fields deliberately; do not rely on route names.
- **Missing from the old code, flagged for the new:** nothing anywhere set an
  ACME staging directory, and nothing set a per-route timeout, request body
  limit or rate limit. The trusted-proxy list was the only client-IP concern.

Size: source 2513 lines, extract 318 lines

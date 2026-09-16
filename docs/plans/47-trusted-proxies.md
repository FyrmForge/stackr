# Plan: trusted proxies

Status: agreed 2026-09-16, not built.

Working rules: discuss first, one point at a time, no code without a go, no
git writes, no edits to `*_templ.go` or `output.css`, terse UI copy.

## Why

Traefik has no `forwardedHeaders.trustedIPs`, so behind Cloudflare every
request looks like it came from Cloudflare. Rate limits, audit logs, the
panel's `clientIP`, and tenant apps using `${{ stackr.PROXY_CIDR }}` all see
one visitor. The panel-side `TRUSTED_PROXIES` env (panel trusts Traefik) is
already right and does not change.

## Decisions (2026-09-16)

- One new section on the admin Proxy page: a textarea of CIDRs, one per
  line, and a tick box "Trust Cloudflare".
- Cloudflare's ranges are fetched from `cloudflare.com/ips-v4` and `ips-v6`,
  not hardcoded. Fetched on every static-config write (startup, proxy saves),
  few-second timeout, last good list cached in the DB.
- Fetch fails with a cache: use the cache, log a warning.
  Fetch fails with no cache: the save is refused with "could not reach
  Cloudflare" and the box stays off. Never silently empty.
- No refresh timer. Restart or re-save refreshes. Add one if stale ranges
  ever bite.
- Empty textarea and box off: Traefik config is byte-identical to today.

## Settings keys

| key | value |
|---|---|
| `trusted_proxies` | CIDRs, newline separated, hand typed |
| `trust_cloudflare` | `1` or empty |
| `cloudflare_cidrs` | cache, newline separated, written only by a successful fetch |

## Exact shapes

Fetch: `GET https://www.cloudflare.com/ips-v4` and `/ips-v6`, plain text,
one CIDR per line. Keep the base URL in a package var so the test can point
it at `httptest`. Reuse the `probeClient` pattern in `probe.go` for the
timeout.

Rendered entrypoint, only when the merged list is non-empty:

```yaml
entryPoints:
  web:
    address: ":80"
    forwardedHeaders:
      trustedIPs: ["173.245.48.0/20", "2400:cb00::/32"]
  websecure:
    address: ":443"
    forwardedHeaders:
      trustedIPs: [...]
```

Merged list is typed lines first, then the cache, deduped, order kept.

`traefik_static_override` replaces the generated file verbatim, so an
override silently drops the trusted list. The page says so in one line
under the new form when an override is set.

UI copy (terse): section title "Trusted proxies". Textarea placeholder
"one CIDR per line". Checkbox label "Trust Cloudflare". Hint under it:
"Fetched from cloudflare.com on save and restart." Errors: "not a CIDR:
<line>", "could not reach Cloudflare".

## Files

- `internal/stackrd/infra/proxy/proxy.go`
  `RefreshCloudflare(ctx) error` fetches both lists, validates each line
  with `net.ParseCIDR`, writes `cloudflare_cidrs`. `EnsureTraefik` calls it
  when `trust_cloudflare` is set (warn on failure), then renders
  `forwardedHeaders.trustedIPs` on `web` and `websecure` from
  `trusted_proxies` plus the cache. Goes in before the change compare so a
  list change recreates the container like any other static change.
- `internal/stackrd/handlers/web/handler/settings/proxy.go`
  `ProxyPage` reads the three keys. New `SaveTrustedProxies`: validates
  every typed line as a CIDR, calls `RefreshCloudflare` synchronously when
  the box is ticked and refuses on failure with no cache, saves, runs
  `EnsureTraefik` in the background like the override handler.
- `internal/stackrd/handlers/web/handler/settings/proxy.templ`
  The new form above the static override block.
- `internal/stackrd/handlers/web/server.go`
  `POST /admin/proxy/trusted`.
- `internal/stackrd/infra/proxy/trusted.go` and `trusted_test.go`
  `EnsureTraefik` needs a cluster, so keep the new logic in two pure pieces
  it calls: `fetchCloudflare(ctx) ([]string, error)` and
  `trustedIPs(typed, cached string) []string` (merge, dedupe, drop bad
  lines). One test file covers both with an `httptest` server. No new
  `EnsureTraefik` test.
- `docs/plans/README.md`
  Index row: `| [47](47-trusted-proxies.md) | Trusted proxies and Cloudflare ranges. |`
- `docs/host-setup.md`
  Short "behind Cloudflare" note: tick the box, firewall 80/443 to
  Cloudflare's ranges or the origin is still open on its IP.

## Out of scope

- Cloudflare tunnel and per-org shield, see plan 40.
- Origin certs. HTTP-01 keeps working through the proxy.
- Reading `CF-Connecting-IP`. Trusting the ranges makes X-Forwarded-For
  correct, which is what everything already reads.

# Scoped Variable References — Implementation Handover

## Status

**Shipped.** `internal/stackrd/config/varref` is the live resolver; this
document records the agreed product behavior and the implementation plan for
Railway-style variables, secrets, resource outputs, and reference
autocomplete. One later change: environment-specific stack values exist now
(`env` owner kind; secrets are per-env by default via `env_versions`) —
see the note below.

## Product model

Applications ultimately receive ordinary environment variables, but their
values may be literals, secrets, or references:

```env
HTTP_PORT=3000
API_URL=${{ tile.api.STACKR_INTERNAL_URL }}
DATABASE_URL=${{ tile.orders-db.DATABASE_URL }}
REDIS_URL=${{ stack.redis.REDIS_URL }}
SMTP_URL=${{ org.smtp.SMTP_URL }}
SENTRY_DSN=${{ stack.secrets.SENTRY_DSN }}
LOG_LEVEL=${{ stack.vars.LOG_LEVEL }}
```

Reference forms:

| Form | Meaning |
|---|---|
| `${{ tile.<slug>.<output> }}` | Tile or managed resource in the current environment |
| `${{ stack.<slug>.<output> }}` | Stack-scoped singleton |
| `${{ org.<slug>.<output> }}` | Organization-scoped singleton |
| `${{ stack.vars.<name> }}` | Stack-wide plain variable |
| `${{ stack.secrets.<name> }}` | Stack-wide secret |
| `${{ org.vars.<name> }}` / `${{ org.secrets.<name> }}` | Organization-wide, same split |
| `${{ stackr.<NAME> }}` | Value the stackr server supplies about itself |

Vars and secrets are separate namespaces in a reference: the bucket must match
the row's `secret` flag, and a mismatch is an error rather than a fallback.
`vars`, `secrets` and `backups` are reserved slugs, so no tile or shared
instance may take one. The old two-part `${{ stack.<name> }}` /
`${{ org.<name> }}` forms are parse errors naming the replacement.

(Since built: environment-specific stack values — the consumer's env row shadows
the stack-wide value at resolution time, same `${{ stack.vars.<name> }}` syntax;
secrets default to per-env via `env_versions`.)

### The `stackr.` scope

Unlike the others, nobody authors these — the server records them, and the set
of names is closed. Two exist today, both describing where Traefik sits on the
consumer's own environment network:

| Name | Value | Lifetime |
|---|---|---|
| `PROXY_IP` | Traefik's address on this env's network, e.g. `172.18.0.4` | Until Traefik is recreated and docker assigns another |
| `PROXY_CIDR` | That network's subnet, e.g. `172.18.0.0/16` | The life of the network |

The intended use is the trusted-proxy setting an app needs before it will
believe `X-Forwarded-For`:

```yaml
env:
  TRUSTED_PROXIES: ${{ stackr.PROXY_CIDR }}
```

**Prefer `PROXY_CIDR`.** Environment variables are baked when a container is
created, so an app deployed before Traefik was last recreated keeps a stale
`PROXY_IP` — and a wrong trusted-proxy value does not fail loudly. The app just
stops trusting the forwarded header and silently attributes every request to the
proxy's address. `PROXY_CIDR` covers whatever address Traefik holds, at the cost
of trusting every container on that network rather than Traefik alone. Those
containers already share a network and can reach each other directly, so the
widening is small.

Both are per-environment despite the scope being named for the server: Traefik
joins every environment network and holds a different address on each. They are
not secrets, so external readers (`Scoped` mode) see them. A name outside the
closed set is a parse error, and a name that the proxy has not recorded yet is a
resolve error — neither resolves to an empty string, because a blank here is
exactly the failure that would go unnoticed.

Recorded by `envnet.RecordProxyAddr` at both moments Traefik attaches to an
environment network — `envnet.Ensure` on deploy (which is how a new or PR
environment gets one) and `Proxy.reattach` at startup (which refreshes every
existing env after Traefik is recreated). Stored on the environment row
(`proxy_ip`, `proxy_cidr`, migration 033).

Compose tiles are the one gap: they return early in the deploy pipeline, before
the env network is ensured, so a compose tile in a brand-new environment sees
the not-recorded-yet error until some service tile in that env has deployed.

All variables on an accessible tile are referenceable. Secret values remain
masked in normal responses and suggestions, but browser users with content
write access and API keys with `secrets:read` may retrieve them.

## Resources and endpoints

### Application endpoints

Each service initially has one primary endpoint:

- protocol: `http`, `https`, or `tcp`;
- container port;
- optional variable name through which the application receives that port
  (`PORT`, `HTTP_PORT`, a custom name, or none).

Stackr publishes:

- `STACKR_PRIVATE_DOMAIN`;
- `STACKR_INTERNAL_PORT`;
- `STACKR_INTERNAL_URL` when the protocol has URL semantics;
- `STACKR_PUBLIC_URL` when a public domain exists.

The endpoint is Stackr's source of truth. The application's chosen port
variable is only an injection mechanism.

### Managed provisioned resources

A logical database or bucket provisioned from shared infrastructure is a
first-class resource tile even though it does not own a container.

Example:

```text
web -> orders-db -> org/shared-postgres
```

`orders-db` publishes the standard PostgreSQL outputs:

- `DATABASE_URL`;
- `PGHOST`;
- `PGPORT`;
- `PGDATABASE`;
- `PGUSER`;
- `PGPASSWORD`.

An S3 resource publishes:

- `S3_ENDPOINT`;
- `S3_BUCKET`;
- `S3_REGION`;
- `S3_ACCESS_KEY`;
- `S3_SECRET_KEY`;
- `S3_PUBLIC_URL` when public.

Provisioned resources have explicit consumer bindings. Referencing a resource
does not grant access to it.

Provisioning providers must not publish administrator credentials. A true
stack/org singleton that is consumed directly may publish its normal
connection outputs.

## Authorization

Validation happens when a reference is saved and again before deployment:

- `tile.*` resolves only within the consumer's environment;
- `stack.*` resources must have `scope_kind=stack` and belong to the
  consumer's stack;
- `org.*` resources must have `scope_kind=org` and belong to the consumer's
  organization;
- provisioned resources require an active binding to the consumer;
- names must resolve exactly once; there is no fallback across scopes;
- cross-organization references are always rejected.

Secret names and metadata may be listed when the source is accessible. Secret
values require browser content-write access or the privileged API scope
`secrets:read`.

## Storage changes

Add normalized variable storage:

```text
variables
  owner_kind   tile | stack | org
  owner_id
  name
  value        encrypted
  secret       boolean
  created_at
  updated_at
  UNIQUE(owner_kind, owner_id, name)
```

All values are encrypted at rest, including non-secret values and reference
expressions. The secret flag controls disclosure.

Add managed resources and bindings:

```text
managed_resources
  id
  environment_id
  provider_tile_id
  name
  slug
  kind
  status
  public
  created_at
  updated_at
  UNIQUE(environment_id, slug)

resource_outputs
  resource_id
  name
  value              encrypted
  secret
  requires_network
  UNIQUE(resource_id, name)

resource_bindings
  resource_id
  consumer_tile_id
  created_at
  UNIQUE(resource_id, consumer_tile_id)
```

Add the endpoint protocol and optional port-variable name to service tiles.
Keep `container_port` as the canonical port.

After backfilling ordinary `Tile.Env` entries, normalized variables become the
runtime source of truth. Keep the old column and legacy tables untouched for
rollback, but stop using them in new flows.

## Reference resolver

Create one resolver used by deployments, compose runs, databases, cron tiles,
API previews, and CLI export.

The resolver must:

1. Parse reference expressions embedded anywhere in a value.
2. Resolve stack/org values, tile variables, virtual endpoint outputs, and
   managed-resource outputs.
3. Recursively resolve referenced variables.
4. Track dependencies and required shared networks.
5. Reject missing, malformed, ambiguous, inaccessible, or cyclic references.
6. Return both resolved variables and dependency metadata.

Never leave an unresolved reference in a container environment. A resolution
error blocks deployment or execution.

Changing a referenced value/output redeploys running service consumers. Cron
and scheduled-job consumers use the new value on their next run.

The dependency metadata replaces substring-based graph inference and drives
canvas edges and shared-network attachment.

## Web UI and autocomplete

Structured variable rows are the primary editor. Retain the raw `.env` editor
for bulk import.

Typing `${{` or choosing **Add reference** opens an autocomplete grouped by:

1. current-environment tiles and provisioned resources;
2. stack variables and singletons;
3. organization variables and singletons.

Each suggestion shows:

- source and output name;
- description;
- scope badge;
- system/user origin;
- lock icon for secrets;
- resolved preview only for non-secret values.

Interaction requirements:

- fuzzy filtering;
- Arrow Up/Down navigation;
- Enter or Tab to insert;
- Escape to close;
- mouse/touch selection;
- insertion at the current caret without replacing surrounding text;
- accessible combobox/listbox roles and active-option announcements;
- reinitialization after HTMX swaps.

Extend `static/js/codearea.js` so the raw editor receives the same
autocomplete. View mode renders references as chips linking to their source
and shows missing/inaccessible/cyclic status inline.

Autocomplete data comes from a server reference-catalogue endpoint. It returns
only sources and output metadata currently accessible to the consumer; it
never returns secret values. Server-side save/deploy validation remains
authoritative.

## API

Replace map-only variable payloads with structured entries:

```json
{
  "variables": [
    {
      "name": "DATABASE_URL",
      "value": "${{ tile.orders-db.DATABASE_URL }}",
      "secret": false
    }
  ]
}
```

Required surfaces:

- list/replace tile variables;
- list/upsert/delete stack variables;
- list/upsert/delete organization variables;
- retrieve the reference catalogue for a consumer tile;
- retrieve unresolved variables;
- retrieve fully resolved variables;
- list managed-resource outputs and bindings;
- provision, attach, detach, and delete managed resources.

Add `secrets:read` as a privileged API-key scope. Resolved retrieval fails
rather than silently omitting required secret-derived variables when the key
lacks that scope.

Provision responses return the resource identity and published output names,
not legacy env-secret names.

## CLI

Keep `stackr vars set` and add explicit secret input support.

Add:

```text
stackr vars pull [app] [--output PATH] [--force]
```

Behavior:

- resolves all references;
- emits dotenv to stdout by default;
- requires `secrets:read` when any result contains secret-derived data;
- correctly quotes multiline and special-character values;
- writes output files with mode `0600`;
- refuses to overwrite an existing file unless `--force` is supplied.

## Config, cloning, and migration

- Config-as-code env maps store reference expressions unchanged.
- Stack/org values remain server-managed and outside repository config.
- Environment cloning copies tile-variable definitions.
- Cloning a provisioned resource creates a fresh logical resource and
  credentials using the same provider; stack/org singleton references remain
  shared.
- This is a clean break for `${secret.*}` and legacy provision references:
  they are not migrated or resolved.
- Existing ordinary tile variables are backfilled into normalized storage.
- Legacy secret/provision records remain untouched for rollback but disappear
  from the new UI.

## Test and acceptance checklist

- Literal, embedded, nested, multiline, and secret references resolve.
- Missing, malformed, cyclic, ambiguous, deleted, and unauthorized references
  fail clearly before a container starts.
- Tile, stack, and org boundaries cannot be bypassed through API requests or
  hand-written expressions.
- Provisioned PostgreSQL and S3 resources publish the documented outputs and
  enforce consumer bindings.
- Endpoint outputs reflect protocol, port, internal hostname, and public
  domain changes.
- Reference changes produce dependency edges, required network attachment,
  and consumer redeployment.
- Autocomplete works with keyboard, mouse, touch, raw editor, structured
  editor, and HTMX swaps; secret values never enter its payload.
- Browser reveal and CLI/API export enforce secret-read permissions.
- CLI dotenv output round-trips multiline and special-character values and
  applies safe file permissions.
- Config-managed stacks and environment cloning preserve references correctly.

## Deferred

- Multiple named endpoints per service.
- Build-argument references.
- Implicit cross-scope fallback.

(Since done: environment-specific stack values shipped; the legacy
columns/tables went with the migration squash.)

# 002_api_key_org.up.sql

- **Source:** `internal/stackrd/store/db/migrations/002_api_key_org.up.sql` (14 lines)
- **Commit:** `c2423f0`
- **Taken:** the whole migration — one column, its FK, the reason keys are org-bound. No backfill exists.
- **Cut:** the pointer to `docs/plans/service-extraction/06-points-18-20.md`; that plan is dead.
- **Cuts belong to:** nowhere. The source is 11 lines of comment over one statement, so the header alone outweighs it; this is the one row where a bigger extract is not a copy failure.

```sql
ALTER TABLE api_keys ADD COLUMN org_id TEXT REFERENCES orgs (id) ON DELETE SET NULL;
```

**Why.** Scopes are granted against whichever org the minting browser's cookie
pointed at, but the key carried no org of its own — so a write scope minted on
org A travelled to org B, and only the live role check there stopped it.
Binding the key makes the scope and its justification travel together: the key
must carry scope X *and* be acting in the org that granted it.

**Why NULL is allowed.** Unbound covers keys minted by a server admin (whose
write scopes come from the admin badge, not any org) and keys minted with no
active org. Silently narrowing a live credential breaks running CI with no
signal to its owner, so unbound kept the old behaviour.

**Why SET NULL, not CASCADE.** The key belongs to the user (`api_keys.user_id`
cascades from `users`), not the org; deleting an org unbinds rather than
deletes. It is the only `ON DELETE SET NULL` in the schema.

## Notes for the builder

- v1 has no installs, so the "every key that exists today" case is empty. Only
  the admin path needs `NULL`; a key minted inside an org can require `org_id`.
- Two checks, not one. The second — acting in the bound org — is the whole
  point of the migration and is the easy one to drop when rewriting auth.

Size: source 14 lines, extract 35 lines.

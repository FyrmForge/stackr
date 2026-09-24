Source: service/access.go, service/revoke.go (+ access_test.go, revoke_test.go)
Commit: c2423f0
Taken: the verb/level rule table and the one `can()`; "standing changed → close what a live check cannot reach"
Cut: every store read inside the check, API key scopes, share-link mint/spend, the wizard gate, tenancy resolution, HTTP status mapping
Cuts belong to: middleware (facts, tenancy, status codes), leaf/user (revoke), leaf/secretlink (mint/spend), flow/orgsetup (wizard gate)

## The shape

Facts in, verdict out. `can()` never touches the store: middleware already
resolved `/:org/:stack/:env/:tile`, so the org is known and the user's roles
were loaded once for the request.

```go
package authz

// User is the request's principal. Middleware loads it once and reuses it —
// a list view's per-row verdicts come from this same value.
type User struct {
	ID    string
	Admin bool              // stackr admin: owner in every org
	Roles map[string]string // org id -> role
	// extract: fact "roles" now loaded once by middleware (old code:
	// AccessService.Principal ran ListOrgsForUser + GetOrgMember per request)

	// KeyOrg is the org an API key is bound to, "" for a browser session.
	// The key carries no rights of its own: the Roles above are the user's
	// live ones, so a demotion is felt by the key at the next request.
	KeyOrg string
}

// Resource is what the URL resolved to. Today only the org matters; the
// documented ceiling (per-asset grants) adds fields here and calls the same
// function from the orchestrator with the row in hand.
type Resource struct {
	OrgID string
	// extract: fact "orgID" now loaded once by middleware from /:org/...
	// (old code: AccessService.TenancyOf re-walked ids per route)
}

// can is the only place in the product that decides this.
func can(u User, v Verb, r Resource) error {
	need := LevelOf(v)
	if need == LevelAdmin { // not org content at all: nodes, proxy, users
		if !u.Admin {
			return ErrForbidden
		}
		return nil
	}
	// B36. A key is bound to the org it was minted in and does not travel:
	// its abilities were granted on the strength of one org's role.
	if u.KeyOrg != "" && u.KeyOrg != r.OrgID {
		return ErrNotFound
	}
	have, member := u.level(r.OrgID)
	if !member {
		// Outside the org is NotFound, not Forbidden: a 403 there confirms
		// the id exists to someone who should not know it.
		return ErrNotFound
	}
	if have < need {
		return ErrForbidden
	}
	return nil
}

func (u User) level(orgID string) (Level, bool) {
	if u.ID == "" {
		return LevelRead, false
	}
	if u.Admin {
		return LevelAdmin, true
	}
	return RoleLevel(u.Roles[orgID])
}

// RoleLevel is exported because revocation compares the rung somebody had
// against the one they have now, and "did they drop" must be the same
// comparison the gate makes.
func RoleLevel(role string) (Level, bool) {
	switch role {
	case "owner":
		return LevelOwner, true
	case "member":
		return LevelWrite, true
	case "viewer":
		return LevelRead, true
	}
	return LevelRead, false
}

// LevelOf: an unregistered verb needs admin, so a new operation that forgets
// to name itself fails closed instead of open.
func LevelOf(v Verb) Level {
	if l, ok := verbLevels[v]; ok {
		return l
	}
	return LevelAdmin
}

// KnownVerb backs the route test: walk the registered routes, assert every
// mutating one names a verb. An unknown verb fails closed at request time,
// which tells a test nothing — so "is this a verb at all" is a separate
// question.
func KnownVerb(v Verb) bool { _, ok := verbLevels[v]; return ok }
```

## verb → who may

`verbLevels` is this table, one entry per row. The ladder is
read < write < owner < admin; a check is a comparison. Where the panel and the
API disagreed on a verb the **higher** level is the entry — the lower one was
never a considered grant, it was a forgotten guard.

| verb | level | who may, v1 |
|---|---|---|
| `org.read`, `member.list`, `registry.image.list`, `org.config.export`, `deployment.read`, `tile.read` | read | in the org |
| `stack.create`, `stack.write`, `env.write`, `tile.write`, `variable.write`, `domain.write`, `backup.write`, `destination.write`, `sharelink.mint`, `stackplan.plan`, `stackplan.approve`, `org.graph.write`, `connector.write` | write | in the org |
| `deployment.cancel` | write | in the org — the API gated it on *read*, so a viewer whose key carried `apps:deploy` could cancel a production deployment |
| `org.owner.read` (invite list, org plans, the wizard, its repo picker) | owner | org owner — the API let any member list an org's plans; an invite link is a credential |
| `org.write`, `member.manage`, `org.config.bind` | owner | org owner |
| `orgdefaults.set` (server, org, stack or env scope — same level at all four) | owner | org owner |
| `orgplan.approve` | owner | org owner — the API let any member approve, and an org apply renames the org and creates and deletes stacks |
| `registry.credential.write`, `registry.tag.delete` | owner | org owner — the panel always required an owner, the API took any member |
| `org.setup.step`, `org.setup.domain`, `org.setup.connector` | owner | org owner — the last two were member, and the setup allow-list waives the draft gate for the whole prefix |
| `admin.read` (admin area, server list, container console) | admin | stackr admin |
| `serverdefaults.set`, `node.manage`, `container.admin`, `proxy.admin`, `user.admin`, `org.create` | admin | stackr admin |
| anything not in the table | admin | stackr admin (fail closed) |

Raw Caddy snippets ride `proxy.admin`. A param ref resolving another org's
secret is the same question with the *target's* org as the resource.

## Standing changed → close

```go
// extract: dropped the store walk and the notifier; belongs in leaf/user.
// This is the rule, not the plumbing.

// StandingChanged is one rule for three events: demote, remove, disable.
// `to` is "" when the membership is gone; orgID is "" when the account was
// disabled, which is not scoped to one org the way a demotion is (B16).
func StandingChanged(orgID, userID, from, to string) (close bool, scope string) {
	fromLevel, _ := RoleLevel(from)
	toLevel, ok := RoleLevel(to)
	if !ok {
		toLevel = LevelRead // removed, or disabled: no standing at all
	}
	// A drop below write is what matters. Minting is sharelink.mint, which
	// sits on write, so owner -> member keeps rights they already had and
	// their links stand. Everything else is a nudge.
	if fromLevel < LevelWrite || toLevel >= LevelWrite {
		return false, ""
	}
	return true, orgID // "" means every org
}
```

What gets closed, and why each one is on the list:

- **Share links.** A bearer token: no session, no key, no role check anywhere
  in the redeem path. A member who minted one and was then demoted or removed
  has left a working door open. Only the ones **they** minted, and only in the
  org the change was about — except on a disable, where every org goes.
- **Sessions and keys (B16).** The old code left these alone and argued they
  self-heal, because both surfaces re-read the user per request. They are
  closed now: disabling follows the same rule as demoting.
- **A stack moving org** closes every open link under the stack, whoever
  minted it: the links point at resources another org now owns, so neither
  side's membership answers for them.
- Walk the whole list and join the errors. Stopping at the first failure
  leaves working links behind while the role change it belongs to is already
  written — a half-revocation reported as a failure.
- Revoking is a state flip, not a delete. The row names fields but never holds
  values, so it is the audit line for the exchange.

## Where the two copies disagreed

`handlers/api/v1/auth.go` and `handlers/middleware/orgctx.go` each grew a full
gate vocabulary — `adminOnly` twice, `orgMember`/`orgAllowed`/`requireOrgWrite`
against `InOrg`/`RequireOrgAccess`/`CanWrite`/`CanWriteOrg`/`IsOwner`/
`IsOwnerOf`/`RequireOrgWrite`/`RequireStackAccess`/`ReadOnlyGuard` — and by the
end both had grown a near-identical `gate`/`Gate` on top that finally called the
one table, without either old vocabulary being removed. They disagreed on the
level for the contested verbs above; on the refusal (the API answers 403 for a
level, the panel turns `admin.read` and `org.owner.read` into 404 so a page
nobody may see does not announce itself); on tenancy (the API carries a map of
org ids on the key and folds the wizard filter into membership, so listings
silently drop an unfinished org, while the panel checks the org list middleware
loaded and answers the wizard with a redirect or a 409); on what "write" even
asks about (the panel has an *active org* from a cookie, so `CanWrite` and
`CanWriteOrg` answer different questions and templates asking the wrong one
offered buttons to a viewer — `CtxWriteHere` was bolted on to fix it; the API
has no active org at all); on blanket guards (`ReadOnlyGuard` blocks mutating
methods by path allow-list, `/logout`, `/orgs/switch`, `/orgs`, `/account/*`,
`/notifications/*`, with no API equivalent); on key rules (scopes, the 22-entry
catalogue, `GrantableScopes` bounding a mint by the minter's write right, and
`keyOrgAllows` are API-only); and the two panel helpers even disagreed with each
other — `CanWrite` refused an admin with no org, so on a fresh install every
POST under `/servers` came back "read-only access" to the account that owns the
machine, while `CanWriteOrg` waved the same admin through.

## Notes for the builder

- **Two roles in v1**, so `can()` only ever compares two rungs: in the org, or
  stackr admin. Keep the four-level table anyway — it is the record of which
  contested rows were decided which way, and the per-asset ceiling needs it
  back. If you want it smaller, the honest cut is the *ladder*, with each verb
  storing `org` or `admin` and the read/write/owner split kept as a comment.
- **One call site.** Middleware calls `can()`; handlers never re-decide, leaves
  and flows know nothing about auth. Both routers and the CLI (through the API)
  go through it. Job workers call flows with no user — nothing to authorise.
- **Keys have no scopes.** A key is its user plus an org binding: abilities
  come from the live user row every request, so a demotion closes the key's
  power without touching it, and B16 closes the row. At mint time the key can
  never carry more than its minter holds (B36) — with one role that means the
  minter must own the org it is bound to.
- **The 404/403 split lives in the HTTP layer**, once: `ErrNotFound` for
  outside-the-org, `ErrForbidden` for in-the-org-below-the-level. `can()`
  returns bare errors; refusal wording is the surface's.
- **The wizard gate is not a level** and has no place in the table. An org
  mid-setup taking no writes is a flow check, and the setup routes are exempt.
- Keep the route-verb test: every mutating route names a `KnownVerb`. That test
  is what stops the third copy.

Size: source 933 lines, extract 224 lines.

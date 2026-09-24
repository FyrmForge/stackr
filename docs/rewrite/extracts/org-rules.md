Source: service/org.go, service/member.go
Commit: c2423f0
Taken: what org create and delete decide today, plus every membership and invite rule
Cut: the plain read wrappers, the mail send, the revoke cascade, the raw row writes
Cuts belong to: flow/org (delete guards, invite accept), leaf/user (revoke), service/mail

## What is NOT here — read this first

Four of the five rules the row asked for do not exist at this layer, and
`org.go` says so out loud:

```go
// Delete removes an organization. What it cascades is the store's; whether it
// MAY be removed — the last organization on the install cannot be, and a draft
// does not count as one — is the panel's, because that rule is about what the
// setup wizard needs to leave behind.
func (s *OrgService) Delete(ctx context.Context, id string) error {
	return s.store.DeleteOrg(ctx, id)
}
```

- **has stacks** — no such check, anywhere in scope. Delete cascades.
- **last org** — stated in the comment above, implemented in the panel handler.
  `ListAll` exists partly to serve that count, which is the only trace of it.
- **squat check (reserved / taken slugs)** — absent. The only slug this layer
  mints is the draft one.
- **slug grammar** — absent.
- **rename** — there is no rename method. `Save` writes the row back with no
  rules at all: "the rules that decide what may change belong to the callers
  that own them".

DECIDE: either a second extract row over the panel's org handlers, or the
rewrite writes those four fresh in `leaf/org`. "As today" for this row means
`leaf/org.Delete` is unguarded and the guards live above it — which is the
layering the rewrite is trying to end.

## Rules as today (spec)

- Org delete is unguarded at this layer; org rename does not exist here.
- Setup gives one draft per person: unfinished **and** owned by the caller,
  newest wins, named later, random slug, creator becomes owner.
- An org is addressed slug first, id second.
- Roles are `owner`/`member`/`viewer`, refused on a typo, and an org may never
  lose its last owner — checked on both demote and remove.
- Invites last 1–365 days, default 14, refuse an existing member, carry a
  24-byte hex token, and survive a mail bounce.
- Joining is idempotent; burning an invite is not atomic with it.

### Org create rules that do live here

- One draft per person. Starting setup again returns the org already in
  progress, never a second one.
- Matched on **unfinished and owned by the caller**, not on the placeholder
  name — the UI branch renames at its very first step.
- Two sub-rules, both of which used to live only inside a panel helper:
  **newest wins** (the listing orders by name, so first-match would make "the
  draft" a store-order accident the moment someone owns two), and **the
  caller's own role**, not "is an admin" — any server admin passes an
  ownership test on any org, which would make a stranger's half-finished org
  the one this admin's next answer moves.
- `mode` must be `config` or `ui`; anything else is invalid.
- Created empty and named later, by whichever branch the owner picked: the
  config file names a managed org, the name step names a hand-built one.
  Asking for a name here and letting the file overwrite it seconds later is
  what this replaced. Name = `Untitled organization`.
- Draft slug is random (`org-` + 6 hex), not counted: two people starting at
  once must not collide.
- The creator becomes owner in the same call.
- `Resolve` takes slug first, id second, so a slug that happens to look like
  an id cannot be shadowed.
- `ListForUser` and `ListAll` are deliberately named apart: for organizations
  the org *is* the tenancy unit, so the unscoped listing does not widen a
  page, it crosses a tenant.

### Membership rules

- Roles are a whitelist: `owner`, `member`, `viewer`. A bad role is **refused,
  never folded** to `member` — folding is how `--role admin` granted member
  access and reported success.
- Last-owner guard on both demote and remove: an org must never be left with
  nobody who can administer it. Fails closed on a read error — refusing a
  demotion that would have been fine is recoverable, the other way round is
  not.
- Removing someone who is not a member is not-found on every surface; the
  panel used to report success.
- A demotion is not finished when the row is written: what the old role let
  them mint outlives it.
  `// extract: dropped revoke.MembershipChanged, belongs in flow/org`
- Join is idempotent — already a member is a no-op, keeping the invited role
  out of it. Two clicks on one invite link is the normal way it is reached.

### Invite rules

- Expiry: default 14 days, min 1, max 365. `0` means default. Before: the
  panel allowed 1–365 defaulting to 14, the API any number defaulting to 7 —
  including a negative one, which minted an invite that had already expired.
  14 is "long enough to survive a holiday, short enough that a forgotten link
  stops working"; the cap exists because unbounded is a credential, not an
  invitation.
- Email is normalized and required; role defaults to `member` and goes through
  the same whitelist.
- Already a member → conflict, `<email> is already a member; change their role
  instead`. Only the API used to check.
- The token is the invite id: 24 random hex bytes.
- A mail bounce is not fatal; it is recorded on the row and the link comes
  back to the caller either way.
  `// extract: dropped mail.SendInvite, belongs in flow/org`
- **Atomic burn: not here.** `UseInvite` is a bare "mark used". Nothing at
  this layer checks `expires_at` at redeem time, and nothing ties the burn to
  the join — who may redeem is left to the caller (the invite page compares
  the address, and a caller that skipped that would make the link redeemable
  by anybody holding it). DECIDE: the rewrite should check expiry at redeem
  and burn+join in one transaction.
- `CreateInvite` sits next to the guarded `Invite` as a raw row write, used by
  the API's own minting path — it bypasses every rule above. Do not carry that
  door over; give the minting path the guarded call and a "don't send" flag.

## Kept code

```go
// extract: fact "this org's members" now passed in
// extract: fact "this user's orgs" now passed in

// The whitelist is here so a typo is refused rather than quietly downgraded.
func validOrgRole(r string) bool {
	switch r {
	case "owner", "member", "viewer":
		return true
	}
	return false
}

const (
	InviteDaysDefault = 14  // survives a holiday, expires before it is a credential
	InviteDaysMax     = 365 // unbounded meant an invite good for a decade
)

func inviteDays(days int) (int, error) {
	if days == 0 {
		days = InviteDaysDefault
	}
	if days < 1 || days > InviteDaysMax {
		return 0, svcerr.Invalidf("expires_days", "must be between 1 and %d days", InviteDaysMax)
	}
	return days, nil
}

// lastOwnerGuard refuses to leave an org with nobody who can administer it. A
// server admin could still reach it, but from inside the product the org would
// be stuck: nobody left to invite anybody. Fails closed.
func lastOwnerGuard(members []OrgMember, userID string) error {
	for _, m := range members {
		if m.Role == "owner" && m.UserID != userID {
			return nil
		}
	}
	return svcerr.Conflictf("an organization needs at least one owner")
}

// unfinishedDraft is the org this user is already halfway through setting up.
// Nil is the normal answer. Newest wins; owner means THIS caller's row, not
// "is an admin".
func unfinishedDraft(orgs []Org, roleIn map[string]string) *Org {
	var newest *Org
	for i := range orgs {
		if orgs[i].SetupDoneAt != nil || roleIn[orgs[i].ID] != "owner" {
			continue
		}
		if newest == nil || orgs[i].CreatedAt.After(newest.CreatedAt) {
			newest = &orgs[i]
		}
	}
	return newest
}

// draftSlug is a URL for an org with no name yet. Random rather than counted:
// two people starting at once must not collide.
func draftSlug() string { return "org-" + uuid.New().String()[:6] }

const DraftOrgName = "Untitled organization"
```

## Notes for the builder

- `leaf/org` owns the `orgs` row. Members and invites are their own tables, so
  either they are two more leaves or `leaf/org` covers the org aggregate —
  DECIDE, but the last-owner guard and the invite checks must end up in one
  place each, which is the whole reason these two files exist.
- Every guard above is pure once the rows are passed in; that is how they were
  written here.
- `Resolve` (slug then id) is load-bearing for the CLI and the panel both —
  three hand-written copies preceded it.
- Nothing in this scope enqueues a job or writes a status column. The only
  thing that reaches outward is the revoke cascade, and that is a cut.

Size: source 442 lines, extract 196 lines

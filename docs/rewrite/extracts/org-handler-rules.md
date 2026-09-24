# org-handler-rules

Source: `handlers/web/handler/org/members.go` (Rename, Delete),
`handlers/web/handler/org/handler.go` (Create), `handlers/web/handler/org/setup.go`
(SetupDone, SetupMode), `handlers/web/handler/org/settings.templ` (the messages).
Commit: c2423f0
Taken: the org create / rename / delete rules as the panel enforced them — slug
grammar, uniqueness, the reverse anti-squat, the registry-images refusal, the
has-stacks and last-org refusals, the draft/finished split, and every message.
Cut: echo plumbing (flashes, redirects, htmx re-renders, form parsing, the logo
upload), the admin and owner checks, the builder removal, the org cookie.
Cuts belong to: the transport layer, authz middleware, `leaf/org`'s world object
(the builder), the flow (facts as arguments).

Target: `service/internal/leaf/org`.

`service/org.go` has `StartDraft`, `Save`, `Delete` and the reads but no rename
rules and no delete rules at all — the panel held them, in the file that also
holds members and invites. These are them.

## Kept code

The reverse anti-squat, the one rule with real shape to it. Ordinary anti-squat
(`service.CheckOrgSquat`, pinned by `TestCheckOrgSquat`) asks "does this
**host**'s first label equal some foreign **org**'s slug". This asks the inverse:
"does some foreign **domain**'s first label equal my **new slug**" — because
renaming onto it would hand this org that org's generated hostnames.

```go
// extract: fact "every domain resource on the server" now passed in
// extract: fact "this org's own stack ids" now passed in
ours := map[string]bool{o.ID: true}
for _, s := range f.OwnStacks { ours[s.ID] = true } // an org owns its stacks' domains too
for _, r := range f.AllResources {
	label, _, ok := strings.Cut(strings.TrimPrefix(r.Host, "*."), ".")
	if ok && label == slug && !ours[r.OwnerID] {
		return refuse("Another organization's domain already leads with \"" + slug + "\", so this name is not available.")
	}
}
```

The `ours` set is load-bearing: treating the org's **stacks'** domains as foreign
refused a rename over a hostname the renamer already held. The `*.` strip is
load-bearing too — a wildcard hides the same claim.

Delete's condition ladder. One boolean drives all three branches.

```go
unfinished := o.SetupDoneAt == nil

// extract: dropped RequireConfirm (type the slug), belongs in the transport layer
// — but the RULE is: a finished org must be typed out, a draft must not.

// extract: fact "this org's stack count" now passed in
if f.Stacks > 0 {
	if unfinished {
		return refuse("This organization's config file already built stacks. Finish setup, then delete it from settings.")
	}
	return refuse("Move or delete this org's stacks first.")
}
// extract: fact "how many orgs exist on the server" now passed in
if f.TotalOrgs <= 1 && !unfinished {
	return refuse("The last organization cannot be deleted.")
}
```

## Rules as today (spec)

**Create** is thin in the handler on purpose: one draft per person, the
placeholder name and the creator's owner row are all `OrgService.StartDraft`
already. What the handler adds:

- Server admins only, and the refusal is **404, not 403** — a plain user has no
  business learning the route is there. The reason it is admin-only: an org's
  creator becomes its owner, so an open create is a self-service path to owning
  something with its own connector, stacks and secrets.
  `// extract: dropped, belongs in authz middleware` — but `TestCreateOrgIsAdminOnly`
  also asserts a refused create wrote **no** org row, which is the part that
  matters to the leaf.
- The `mode` form value is passed straight to `StartDraft`.

**Rename**, in order — first refusal wins, all five messages verbatim:

| # | Condition | Message |
|---|---|---|
| 1 | name is empty | Give the organization a name. |
| 2 | slugify(name) is empty | That name needs at least one letter or digit, since it becomes the URL. |
| 3 | **fact** another org already holds that slug | Another organization already uses that name. |
| 4 | **fact** a foreign domain resource's first label equals the new slug | Another organization's domain already leads with `"<slug>"`, so this name is not available. |
| 5 | **fact** `slug != o.Slug` and this org has registry images | This organization has images in the registry, and they are stored under `"<old slug>"`. Renaming would orphan them. |

- Rule 2's reason: the slugifier keeps letters and digits, so a name made only of
  punctuation has no URL in it and the org would be unreachable.
- Rule 5 fires **only** when the slug actually moves. A docker registry has no
  rename: moving the namespace would mean re-tagging every image (a blob mount
  plus a manifest push per tag) or orphaning them, so refusing the rename is the
  smaller thing to be right about.
- **Side effects of a rename: none.** Name and slug move together and the row is
  saved. No teardown, no route rewrite, no redeploy — unlike a *tile* rename
  (see `tile-crud.md`), where all three are mandatory. The only warning is to the
  user: "Organization renamed. Note its URLs changed with the slug."
- **No reserved-slug check on rename, in the handlers.** A tile rename has one;
  this does not. Whether `OrgService.StartDraft` applies one on *create* is
  unverified — `service/org.go` was outside this row's read set. Do not read the
  absence here as "orgs have no reserved slugs".
- Sequencing worth one line: everything before the save still addresses the org
  by its **old** slug, so a mid-rename failure (the logo upload) has to keep
  using the old one or the redirect lands on a page that does not exist and the
  rename is lost with the error.

**Delete:**

- `unfinished := SetupDoneAt == nil` splits every rule below.
- A **finished** org must have its slug typed out to confirm. A **draft** must
  not: typing a generated slug to throw away a draft is friction with nothing
  behind it, and the wizard's Discard has only a confirm dialog to ask with.
- **Has stacks → refused**, with two different messages: "Move or delete this
  org's stacks first." when finished, and "This organization's config file
  already built stacks. Finish setup, then delete it from settings." for a draft
  — the config branch's apply builds real stacks before Finish, and there is no
  stacks page to send someone to while setup is still open.
- **Last org → refused, only when finished.** A draft is exempt: on a fresh
  install it is the only org there is, and refusing would leave the wizard with
  no way out at all. Condition is literally `total <= 1 && !unfinished`.
- What a delete removes, as the panel states it to the user, verbatim:
  "Deleting this organization removes its members, connectors, domain resources
  and settings." Plus its build cache —
  `// extract: dropped the builder removal, belongs in leaf/org's world object`
  (left undone, the builder and its cache sit on the disk for ever).
- The active-org cookie is **deliberately not cleared**. The org context only
  honours it when it matches one of the user's own orgs and otherwise falls back
  to the first, so a stale value costs nothing and clearing it would be dead
  code. A rewrite will be tempted to add it; do not.

**The two wizard rules that are org-row rules, not transport:**

- `SetupMode`: the branch is `ui` or `config` and nothing else. Switching to
  `ui` **clears the config binding** (connector, repo, branch, path) and rejects
  any plan still pending on it — leaving the binding would keep the org reading
  as config-managed with no file behind it. Refused outright once setup is done.
- `SetupDone`: the only writer of `setup_done_at`, and idempotent — writing it
  twice is a no-op, and merely *rendering* the summary must never write it.
  Refuses while the org still carries the draft placeholder name — "This
  organization has no name yet." — on the config branch only the apply names it,
  so finishing would strand an organization called by the placeholder at a
  random slug.

## Notes for the builder

- **Four cross-table facts become arguments:** the org that already holds the
  candidate slug, every domain resource on the server (plus this org's own stack
  ids, to tell its own from foreign), whether this org has registry images, this
  org's stack count, and the total org count.
  `// extract: fact X now passed in by the flow`
- **Do not merge the two squat checks.** `service.CheckOrgSquat` guards a
  *hostname* being claimed; the block above guards a *slug* being renamed onto.
  Inverse directions, same intent. Flattening them loses the rename half, and the
  rename half is the one that lives nowhere else.
- **Messages are the spec.** Both delete refusals, all five rename refusals and
  the draft-name refusal are user-facing strings the panel tests and the wizard's
  flow depend on. Keep them verbatim.
- The refusal *shape* differs by surface — a 400 on the settings tab, a
  re-rendered step with the typed value still in the box mid-wizard — because
  htmx has nothing to swap for a bare status. That is the transport's problem;
  the leaf returns one refusal per rule and lets the caller dress it.
- Not taken from this directory, one note for the lot: members, invites and
  roles (their own leaf), the canvas/graph handlers, the config-plan screens, the
  registry and defaults tabs, env colours, org variables and org domain
  resources. Each is another table's row.

Size: source 317 lines (`members.go` 20-211, `handler.go` 93-110, `setup.go` 168-194 and 453-485, `settings.templ` 50-96; the four test files read, not counted), extract 171 lines.

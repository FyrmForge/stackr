# Step 7a: domains backend

Read first: `docs/rewrite/PROGRESS.md`, `AGENTS.md` "Layering rules",
REWRITE.md "Domain resources" (the design) and "Networks". Code to port,
as it is: `stackr-old internal/stackrd/service/domainresource.go`
(`Create`, `ValidateResourceHost`, `CheckOrgSquat`,
`VisibleDomainResources`, `AutoHost`, `SetACME`, `Delete`, `ListAll`),
`store/repo/models.go` `DomainResource`, `cmd/stackrd/seed.go` (the
instance row), `config/stackconf/plan.go` `claimHost` (`auto:`/`apex:`),
`handler/org/settings.go` `orgDomainsPage` / `SaveOrgDomain`,
`api/v1/v1.go` `/domain-resources`, `cli/cmd/domain.go`. In the rewrite:
`internal/service/internal/leaf/domain` (tile domains, Caddy),
`leaf/stack` `Reservation` (goes), `service/org.go` `claims`,
`leaf/org` (the reverse squat check), `flow/managed/flow.go` `PublicBase`.

What it is: v0's domain resources at instance, org and stack level, the
automatic hostnames they give tiles, and the squat rule. darthvader
2026-09-25 (DECIDE 190). Built before step 7: the wizard's domain step
and `PublicBase` need it.

Branch `rewrite-step-7a`, stacked on `rewrite-step-6e`; step 7 restacks
onto it.

## Tasks

1. **Store.** `domain_resources` (id, level CHECK in instance|org|stack,
   org_id NULL FK ON DELETE CASCADE, stack_id NULL FK ON DELETE CASCADE,
   host UNIQUE, include_env_on_default, acme_email, declared, created_at;
   a CHECK that the level matches which id is set). `domains` gets
   `resource_id TEXT NULL REFERENCES domain_resources (id) ON DELETE
   RESTRICT`: the resource that named an `auto`/`apex` host, null for a
   literal. `stacks.domains`
   dropped. Edit `internal/db/migrations/003_rest` in place, up and down;
   `store.DomainResource` + `newTable`; `Tables` and `bind`;
   `internal/db/cascade_test.go`: deleting an org or a stack deletes its
   rows.
   Done when: store round-trip test; cascade test green; the build has
   no reader of `stacks.domains` left.

2. **Root domain.** `stackr-install --domain` reaches `stackrd` as
   `ROOT_DOMAIN` (`internal/installspec`, the panel env); a `root_domain`
   setting (`settings/catalogue.go`, admin read-only) and a boot step
   that seeds the instance row from it once (v0's `seed.go`), never
   twice.
   Done when: installer spec test carries the env; boot test seeds once.

3. **`leaf/domainres`.** Owns the table: `Create(ctx, level, ownerID,
   host, include, acme)` (grammar via `installspec.CheckRoot`; taken →
   `Conflict` from the unique index; the squat check; the ACME account
   as `leaf/domain` does), `SetACME`, `Delete` (`Conflict` "n tile
   domains are named by this resource" while any `domains.resource_id`
   points at it; the FK is the backstop), `Get`, `ListAll`, `Visible(ctx, stackID)`
   nearest first, declared first, oldest first, `AutoHost(res, tile,
   env, isDefaultEnv)` where the default env is the ladder's top rung
   (DECIDE 192), `CheckOrgSquat(ctx, host, orgID)` sharing
   `leaf/org`'s label logic (one function, both directions).
   Done when: leaf tests for grammar, taken, squat both ways, visibility
   order, every `AutoHost` shape incl. the default-env label rule.

4. **Stack file and promote.** `DomainConf` gets `auto: true` and `apex:`
   as v0's grammar (with the hints); the promote plan resolves both
   through `Visible` + `AutoHost` when it plans, "no domain resource is
   visible to this stack" as a blocker, and the domain rows it writes
   carry `resource_id`; stack-level `domains:` writes
   stack rows (create missing, update env/ACME, the file never deletes
   one) instead of the JSON; `SetReservations` becomes rows.
   Done when: plan tests: auto under an org row, under the instance row,
   `apex`, no resource → blocker, reservation rows written; an existing
   literal-host test unchanged.

5. **Verbs.** `CreateDomainResource`, `DeleteDomainResource`,
   `UpdateDomainResource` (env flag and ACME), `DomainResources(ctx, orgID)` (the org's rows
   and the instance's, for the drawer), `AllDomainResources` (admin).
   `FinishOrg` runs v0's `ensureDefaultDomain`. `claims` adds org rows
   and stack rows; `AttachDomain` gets the forward squat check.
   `PublicBase` for managed tiles through `AutoHost`. The org's ACME
   accounts join the `wiring.go` loop. `docs/rewrite/verbs.md` rows.
   Done when: service tests: finish an org with no resource → one
   undeclared `<slug>.<root>` row; a squatting attach is refused; a
   managed tile's `PublicBase` resolves.

6. **API + CLI.** v0's paths: `GET/POST /domain-resources`, `PATCH`,
   `DELETE /domain-resources/:id`, owner level (`org.setup.domain` is
   registered and unused; use it, or one `domain.resource` verb, the
   implementer's call, `TestEveryRouteHasAVerb` clean); the stack
   reservations route goes. CLI `stackr domain ls|add --level --owner
   --host [--include-env] [--acme-email]|rm`. `docs/openapi.json`
   regenerated.
   Done when: api tests incl. a member's 403; cli test for the ops.

7. **UI.** Org drawer tab `domains` (`internal/ui/drawer/org/`): v0's
   `orgDomainsPage` one to one (rows, add form with host,
   include-env checkbox and ACME email, delete with confirm); the tile
   drawer's domain form offers `auto` beside the literal host as v0 did.
   The wizard's domain step is step 7 task 11.
   Done when: drawer tests add, list, delete; viewer sees no form;
   `make templint` clean.

8. **VM proof.** Pave with `--domain stackr-test.vulpe.dev`; the instance
   row exists; finish an org → `<slug>.stackr-test.vulpe.dev` row;
   a tile with `auto: true` comes up at
   `<tile>.<stack>.<org>.stackr-test.vulpe.dev` over TLS; an org row
   `shop.stackr-test.vulpe.dev` added in the drawer wins for that org; a
   custom domain whose first label is the other org's slug is refused.
   Playwright, deep links, snapshot asserts.

9. **Done gate.** `make build`, `make lint`, `make test`, `make templint`;
   PROGRESS.md ticks and the step 7a line; local commit; stacked PR only
   on "commit and push".

## Not in this step

The org file's `domains:` (DECIDE 191, step 7), the new-org wizard
(step 7 task 11), moving a resource between levels.

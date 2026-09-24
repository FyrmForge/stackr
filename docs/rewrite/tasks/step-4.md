# Step 4: API and CLI

Read first: `docs/rewrite/PROGRESS.md`, then REWRITE.md "Rules from the first
commit" rules 6 and 7, "Auth", "Build order" row 4, "Spec and tests" rows
B1/B21/B22, B2, B20. Extracts: `docs/rewrite/extracts/{cli-ref,
access-revoke}.md`. `docs/rewrite/verbs.md` from step 3 lists every
orchestrator verb; the API exposes each one, nothing else.

The API is the only door. Handlers bind, check form shape, call one verb,
render. The CLI is a plain HTTP client with no rule of its own.

## Tasks

1. **Router and middleware.** Mount the step 1 middleware on `/api/v1`
   with nested URLs `/orgs/:org/stacks/:stack/envs/:env/tiles/:tile/...`.
   Session and API-key auth land in the same middleware. Typed errors map
   to status codes in one function; no handler picks a code.
   Done when: an unauthenticated, a non-member and an admin request hit
   the same route with the expected codes.

2. **Handlers, one per verb.** Generate or hand-write one handler per
   orchestrator method, grouped by area. Request structs carry only what the
   user gave (nullable fields, never blank-on-missing, B1 B21 B22). Run the
   handler-audit skill; it must report nothing.
   Done when: every verb has a route, the skill is clean.

3. **Streams.** SSE endpoints for job status, build logs, container logs;
   same middleware as any request.
   Done when: a test reads a job's events end to end.

4. **Webhooks.** `POST /hooks/github/:org` verifies through
   `internal/githubapp` and calls the push verb; PR open/close create and
   remove PR envs.
   Done when: a signed fixture push lands a release in the fake.

5. **CLI login.** Browser approves, CLI gets a one-time code, exchanges it
   at `/api/v1/auth/exchange` for an org-bound API key. Raw key never
   touches the browser.
   Done when: the exchange test passes and a used code is refused.

6. **OpenAPI.** `stackrd --dump-openapi` writes the spec from the routes;
   checked in and diffed in `make lint`.

7. **CLI `cmd/stackr`.** Command names and UX from the `cli-ref` extract,
   generated or hand-written against the OpenAPI spec: login, org, stack,
   env, tile, params (get/set/merge/export), promote (with `--dry-run`),
   rollback, deploy/build, logs, restart, volumes, backups, domains,
   managed, keys, admin. `-y` skips prompts and never means force. Output
   tables and `--json`. Refusals print the API's typed message.
   Done when: every API route has a CLI verb or a documented reason not to;
   B1/B21/B22 test (a `set` sends only the given fields).

8. **Rule parity tests.** B2: a rollback through the API is refused by the
   same rule as the panel will be (the verb is shared). B20: "what blocks
   this promote" is one field the API returns.

9. **Done gate.** build/lint/test pass; skill clean; PROGRESS.md ticked;
   stacked PR.

## Not in this step

No web UI, no installer.

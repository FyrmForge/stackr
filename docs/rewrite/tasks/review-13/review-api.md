# API review (agent report, unvalidated)
1. internal/service/internal/store/api_keys.go:9-21 keys have no expiry, last_used, rotation. Fix: expires_at, last_used_at, rotate route.
2. endpoint.go:127-134 + access.go:333-335 no Idempotency-Key / If-Match; retried POST deploy queues second job. Fix: honour Idempotency-Key on job endpoints, 412 on stale updated_at.
3. internal/api/routes.go:121-124 no org/stack/env job list; only /admin/jobs and /tiles/:tile/jobs. Fix: GET /orgs/:org/jobs?state&stack&env&limit&cursor.
4. store/jobs.go:13-31 + service/jobs.go:396 Job has no actor, no org/stack/env/tile fields, payload as JSON string in JSON. Fix: actor_id, org_id, tile_id; payload as object.
5. leaf/securitylog/securitylog.go:17-47 audit covers logins/keys/grants/secret reveals/cluster but not deploy, delete, param set, domain change, member role change. Fix: audit append in enqueue + org/member/param verbs.
6. access.go:120-123 setupOpen: API bypasses org "setup not finished" gate that web enforces; key can deploy into half-set-up org. Fix: gate API too except org create/rename/finish.
7. access.go:374-377 + errs.go:115-136 envelope has one field; errs.Invalids reports only the first; no stable code. Fix: code + fields[].
8. endpoint.go:143-147 JSON decode error returns Go text, empty field. Fix: parse and translate.
9. access.go:312-321 ErrBusy/Docker-unreachable 503 without Retry-After; NeedsApproval has no HTTP mapping (500). Fix: Retry-After, map to 409.
10. tiles.go:260-270 + run.go:20-30 POST tile/run answers 202 with job null when refused. Fix: 409.
11. routes.go:140-186 delete semantics differ: DELETE tile 202+Job, DELETE stack 204 sync w/ 409 if envs, stack/teardown 202 job with confirm, DELETE env 204. Document or unify.
12. routes.go:62-70,109,143,150-156 update verbs mix PUT /name, /color, /lock, POST rename, PATCH tile, PUT settings, PATCH params, PATCH defaults. Pick one style.
13. store/tiles.go:25-48 env_json, volumes, files, published_ports, depends_on, settings are strings inside JSON; OpenAPI shows string. Fix: structured view types.
14. routes.go:66-95,118 no pagination on orgs, stacks, members, invites, releases, security log, registry versions. Fix: limit+cursor.
15. openapi.go:14-16 no enums/formats/descriptions; query params typed string. Fix: Enum/Doc hooks.
16. routes.go:58-60 + admin.go:136-149 keys listable only by owner; org owner cannot see/revoke org keys; admin cannot list all. Fix: GET /orgs/:org/keys, GET /admin/keys.
17. access.go:331-335 ErrRefused without message -> 403 "refused" with no hint of required level. Fix: include verb + level.
18. errs.go:5-12 + access.go:357-363 API uses 400 for validation while AGENTS.md/web use 422; 404 doubles as not-yours. Fix: 422 for Invalid; document 404 policy.
19. join.go:35-115 rate limiting only on public join/node routes; none per key on /api/v1. Fix: per-key token bucket.
20. openapi.go:55-57 no versioning/deprecation mechanism. Low until users exist.
21. server.go:33 + stack.go:215 no per-org outbound webhooks (job.finished, release.created, domain.changed); only inbound hooks, SSE, admin alert webhook.
22. stream/stream.go:94-134 mid-stream error event carries only err.Error(), no status/code; no Last-Event-ID resume. Fix: APIError envelope + resume.
23. stacks.go:270-275 + routes.go:146 PUT /envs-order takes ids; everything else slugs; base_env_id id, stack slug, connector_id id. Fix: consistent, say which in field name.
24. registry.go:27-35 DELETE /registry/versions?repo=&tag= addressed by query string. Fix: path params.
25. tiles.go:190-213 from-dump: name/engine/slice query only, no max size (LiftBodyLimit removes cap). Fix: document, 413 above limit.
OpenAPI mismatches (340 ops match exactly; detail only): promote and panel-backups body required in spec but BodyOptional in code; offset/limit/tail/port typed string vs int; admin/jobs?state and exec?cmd repeated in code, single in spec; EnvOut node_id in required but omitempty never set; settings bodies schema {}; tile/run 202 on refusal; websocket content type bogus and X-Stackr-Port/frames undocumented; exec X-Exit-Code trailer and stdin body undocumented; hooks/connectors, /api/health, /metrics, /join/:token absent from spec; APIError has no code; Go package names leak into component names; KeyIn.level no enum; registry force typed string.
Parity web vs API: add connector (GitHub app, DNS provider) web only; org setup wizard web only and API bypasses gate; server setup wizard partial via admin settings; alert heartbeat URL web only; master key rotate web only; backup recovery key show/rotate web only; tile metrics live stream web only (API snapshot); stack config branch/file pickers web only; unbind server config repo PLAUSIBLE via PUT empty body. API-only: exec, forward, terminal, from-dump, stack export, org config export.

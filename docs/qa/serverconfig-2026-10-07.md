# Server config rig QA, 2026-10-07

Build under test: v0.6.0-dev.17 (`make release RELEASE=v0.6.0-dev.17`, `scripts/rig.sh upgrade 0.6.0-dev.17`;
rig was on dev.16). `stackr admin version` -> `v0.6.0-dev.17`. Final version on the rig: dev.17.
Rig: stackr-test.vulpe.dev, org smoke, admin@test.com. CLI built from the tree; the key used is
the smoke-bound admin key (`rig-test`), so org-scoped work on the throwaway org went through the web.
Server-file edits were applied from the scratchpad; every plan here has source `local`.

## Results

| check | result | evidence |
|---|---|---|
| 1 export, plan unchanged | PASS | `stackr server export -o stackr-server.yml` wrote settings (acme_email, panel_domain, root_domain, trusted_proxies) + the instance domain. `preview --detailed-exitcode` -> exit 0. `apply` of the same file stored a plan `status clean, summary no changes`, "nothing to apply". |
| 2 edit schedule, route, acme_email | PASS | File: `cleanup_schedule: "45 4 * * *"`, route `qa-sc-route.stackr-test.vulpe.dev` http -> 192.168.1.100, instance domain `acme_email: qa-sc@vulpe.dev`. Plan: 3 rows (`settings cleanup_schedule 30 4 -> 45 4`, `domain-update acme_email`, `route ... 192.168.1.100:80 http`), `note: planned from a local file, not the repo`. `approve -y` -> `server apply: done`. `admin setting cleanup_schedule` = `45 4 * * *`, `admin route ls` shows the route, `domain ls` shows the email. `http://qa-sc-route...` -> 308 to https, `https://qa-sc-route...` -> 308 (the LAN target itself redirects). Re-preview exit 0 (clean). |
| 3 risky cascade cpu_limit | PASS (product), see bug 1 | `defaults: {cpu_limit: 1.5}` -> plan row + `impact: redeploys 7 tiles`, summary `1 to change, needs confirm`. API `POST .../approve {}` and `{"confirm":false}` -> `409 This plan has impact lines; confirm to approve.` CLI under a pty: "Apply server plan ...? [y/N]" then "This plan has impact lines (see above). Are you sure? [y/N]" -> n -> `error: stopped`, plan still pending. `approve -y` (sends confirm) -> applied, `admin defaults` = `cpu_limit 1.5`, 7 deploy jobs queued. Put back with `defaults: {}` the same way (`impact: redeploys 4 tiles`, confirm, applied, `admin defaults` -> `(none)`). |
| 3 auto on does not apply a risky plan | SKIPPED, code read | `server_config_auto` can only be set by `PUT /admin/config-repo` (`admin setting server_config_auto true` -> "set by binding the server file"), and that needs a repo binding, which this QA was told not to create. Code: `internal/service/serverconfig.go` `planServer` is the only auto path (`p.AutoOK() && b.Auto`, repo plans only); `PlanServerFile` (local) never auto-applies. On the rig both a risky local plan and a plain local plan stayed `pending`. |
| 4 route dropped from the file | PASS | `routes: []` -> `route-delete qa-sc-route...` with `remove?: route:qa-sc-route.stackr-test.vulpe.dev (stays unless you pass --remove ...)`. `apply -y` without `--remove` -> applied, `admin route ls` still lists it. `apply -y --remove route:qa-sc-route.stackr-test.vulpe.dev` -> `route-delete ...`, `admin route ls` -> `(none)`. API `ticked:["route:nope"]` -> `400 ticked: "route:nope" is not a removal in this plan.` |
| 5 dest with a missing server secret | PASS | File declared `params.s3.ak` (value) and `s3.sk` (secret, unset), dest `qa-offsite` referencing both, plus `orgs: [qa-sc]`. Plan rows: `param s3.ak`, `org-create qa-sc`; notes `backup_dests.qa-offsite waits for server.params.s3.sk, which is not set` and `params.s3.sk is declared and not set; anything that reads it waits until it is`. No blocker; approve applied the param and the org, no dest. After `params set s3.sk=... --secret --level server` the same file planned one row `dest qa-offsite` and applied it (`GET /admin/backup-dests` lists it). Note: `stackr dest ls` from an org shows only shared globals, so the unshared dest is visible through the admin API only. |
| 6 org created from the file | PASS | `impact: creates org qa-sc; you become its owner`, summary `needs confirm`. Applied: `admin orgs` lists `qa-sc QA SC`; rig DB `org_members` has `(qa-sc, admin@test.com, owner)`. The org got its seeded resource `qa-sc.stackr-test.vulpe.dev`. Deleted by hand afterwards (web: tile, env, stack, org). |
| 7 org domain resource rename | PASS | In qa-sc (web): stack `qa`, env `dev`, tile `hello` (traefik/whoami, port 80), "Add auto domain" -> `hello.qa.qa-sc.stackr-test.vulpe.dev`, deployed, GET -> 200. Org drawer Domains, Rename to `qa-sc2.stackr-test.vulpe.dev` -> drawer shows the new host. `https://hello.qa.qa-sc2...` -> 200 (new cert issued); `https://hello.qa.qa-sc...` -> `308 -> https://hello.qa.qa-sc2.stackr-test.vulpe.dev/`; the http:// old host also 308s to the new https host. |
| 8 web: Config tab, plan view, confirm, Connectors | PASS, see bugs 2 and 3 | Config tab: Export card with the download link, "Config as code" card (says to create a server connector first; no bind form without one), "Latest plan" with the full plan view, "Plans" list with status, summary, `local file`, time. Plan view: impact row highlighted with its line, removal row as an unticked `Remove` checkbox, Approve and Reject. Approve on a risky plan opened the dialog "Are you sure?" listing the impact line ("certificates are issued under this contact address from now on"), Cancel / Approve; Approve applied (acme_email changed, the unticked dest stayed). A second plan: tick the box, Approve -> no dialog (no impact), dest removed. Connectors tab renders ("Add a GitHub connector", "No server connectors yet"). smoke org drawer Config tab renders: bind form, latest plan (an old `error org config invalid, git clone: exit status 128` from Oct 2); nothing approved on smoke. |
| 9 panel_domain cleaned | PASS | Settings form, `panel_domain` = `https://stackr-test.vulpe.dev` (the URL form of the live host, so the panel does not move), Save -> "Saved." and the field re-rendered as `stackr-test.vulpe.dev`; `server export` afterwards has `panel_domain: stackr-test.vulpe.dev`. Panel still answered. host:port and localhost through the API were not tried (the harness refused the call). |
| 10 panel restart | PASS | `docker restart stackr` on the rig: `stackr Up 3 minutes`, `GET /login` -> 200, `admin version` -> dev.17, the qa-sc tile host -> 200 through the proxy. |

## Bugs found

1. **A cascade cpu_limit above the host's cores takes every tile down and the apply still says done.**
   The rig has 1 CPU (`nproc` = 1). `defaults: {cpu_limit: 1.5}` planned "redeploys 7 tiles" and
   applied; all 7 deploy jobs failed with `Error response from daemon: range of CPUs is from 0.01 to
   1.00, as there are only 1 CPUs available`. The server-apply job and the plan read `done` /
   `applied`. Three tiles whose deploy recreates the container (shop/dev api, shop/dev queue,
   infra/production pg-db) had their old container removed before the create failed, so they stayed
   down; four (hello x2, site x2) kept their old container. The follow-up plan with `cpu_limit: 0.5`
   said "redeploys 4 tiles": the three dead tiles were no longer counted and were not redeployed.
   Repro: `defaults: {cpu_limit: <nproc + 0.5>}`, approve with confirm, `stackr job ls`. Expected:
   `SetSettingDefaults` (or the plan) refuses a cpu_limit above the host's cores, the deploy keeps the
   old container when the new one fails to create, and a failed redeploy fails the server-apply job.
   Code: `internal/service/admin.go` SetSettingDefaults / redeployScope; deploy flow
   `internal/service/internal/flow/deploy`. **Not repaired**: the harness refused
   `stackr tile deploy` on the shop and infra stacks (shared rig stacks). api, queue and pg-db are
   still down on the rig and need `stackr tile deploy api --stack shop --env dev`,
   `... queue --stack shop --env dev`, `... pg-db --stack infra --env production`.
2. **Web plan view loses the name on create rows.** `internal/web/render/plan.go` ServerPlanView
   sets `cv.Kind, cv.Tile = "create", ch.Kind` for kinds org-create, route, dest, domain, param,
   connector-share, so the created thing's name (the change's `Tile`) is dropped: the applied dest
   plan showed `+ create dest qa-sc-bucket` (CLI: `dest qa-offsite ... qa-sc-bucket`), an org-create
   shows the display name but not the slug. A `route` change of an existing route (mode or target)
   is also relabelled "create". Fix: keep `Tile`, put the kind in `Field` or a chip.
3. **Removal checkbox checkmark blocked by CSP.** Ticking the `Remove` checkbox in the plan view logs
   `Loading the image 'data:image/svg+xml...' violates ... "default-src 'self'"` (console on
   `?drawer=admin&tab=config`). The checked box renders without its mark (the tick SVG comes from
   `ui/static/css/output.css` as a `data:` background). `internal/web/server.go:48` sets
   `default-src 'self'` with no `img-src`; add `img-src 'self' data:`. Likely every styled checkbox
   in the panel has it.
4. **Applied plan with a ticked removal shows the row as `remove?`.** After the apply, the plan view
   renders the ticked removal with the `remove?` chip (not "removed"); the approver cannot see what
   was ticked. `internal/ui/components/plan.templ` only distinguishes ticked rows while
   `p.Applying`. Display only.
5. **Org domain rename has no confirm in the web.** `ui/drawer/org/org.templ` rename form has no
   dialog or hx-confirm; the CLI (`stackr domain rename`) asks "Every hostname under it moves and the
   old one redirects. Point DNS at the new name first." The ux pass made the web confirm risky
   actions; this one moves hosts and issues certificates.
6. Minor: `stackr server preview` on a clean file prints nothing at all (no "no changes" line);
   `--detailed-exitcode` is the only signal.

## Skipped and why

- Server connector, repo binding and `server_config_auto` (needs a GitHub App and a repo; the lead
  asks darthvader). Auto behaviour recorded from the code above.
- No root_domain rename (DNS).
- panel_domain with a port or `localhost` through the API, and redeploying the three tiles bug 1
  took down: the harness classifier refused those calls.
- No admin key was minted (the classifier refused); the smoke-bound admin key reached every admin
  route, so only the qa-sc org work moved to the web.

## Cleanup

Route, dest `qa-offsite`, server params `s3.ak`/`s3.sk`, cascade defaults, `cleanup_schedule`
(back to default), instance `acme_email` (cleared through a final apply) and `acme_email` setting are
back; `stackr server export` equals the export taken before the run. Org qa-sc with its stack, env,
tile and domain resource deleted in the web; `admin orgs` lists smoke only. The 15 server plan rows
are history and stay. API keys unchanged (rig-test, qa3b x2). Scratch files live in the session
scratchpad only; `.playwright-mcp` files from this run deleted. Left on the rig: the three down
tiles of bug 1 (shop api, shop queue, infra pg-db).

## Rig verify, 2026-10-08 (after R1 and R2)

Build under test: v0.6.0-dev.18 (`make release RELEASE=v0.6.0-dev.18`, `scripts/rig.sh upgrade
0.6.0-dev.18`; rig was on dev.17). `stackr admin version` -> `v0.6.0-dev.18`. Final version: dev.18.
Rig host: 1 CPU, 1973 MB, `workers` 2. The three tiles bug 1 took down (shop api, shop queue, infra
pg-db) were already back up before this run (three deploy jobs done at 07:04). Throwaway org `qa-r1`
(stack `qa`, env `dev`, tile `hello` traefik/whoami with the env volume `data:/data`, so it rolls
stop-first); an unbound admin key `qa-r1-verify` was minted for its CLI work and revoked after.
`make test`, `make lint`, `make templint` green before and after the two seam closes below.

| bug | result | evidence |
|---|---|---|
| 1 limit above the host refused | PASS | `tile set --cpus 1.5` -> `This host has 1 CPU; the limit can be at most 1.`; `tile set --memory 4000` and `env defaults --set mem_limit_mb=9999` -> `This host has 1973 MB of memory; the limit can be at most 1973.`; `stack defaults --set cpu_limit=1.5` refused the same way; org Settings in the web, CPU limit 1.5, Save -> field error with the same text, 422, org settings stayed `{}`; `server preview` (read-only) of a file with `defaults: {cpu_limit: 1.5}` -> `blocker: defaults: This host has 1 CPU; the limit can be at most 1.`, exit 1. Server defaults never changed (still `(none)`). |
| 1 failed create keeps the old replica | PASS | `tile set hello --memory 4` -> redeploy failed `Error response from daemon: Minimum memory limit allowed is 6MB` right after `creating stackr-hello-...`; `tile status` still `running`, same replica id, container `Up` on the rig. |
| 1 failed start brings the old replica back | PASS | `tile set hello --published-ports 443:80` (443 is the proxy's) -> log `creating`, `stopping 1 replica(s)`, `starting`, then `Bind for 0.0.0.0:443 failed: port is already allocated`; `tile status` `running` with the same replica id, the old container `Up 28 seconds` (restarted). |
| 1 dead tile still counts | PASS (count) | After a failed `--memory 4` deploy the old replica was removed by hand (`docker rm -f`), `tile status` `word none`, last job a failed deploy. `server preview` of `defaults: {cpu_limit: 0.5}` (read-only) -> `impact: redeploys 8 tiles` (7 shared + the dead one). The redeploy itself was not run on the rig (it would change server defaults); `tile set --memory 0` brought the tile back. |
| 1 apply reports its redeploys | NOT ON RIG | Needs a server cascade change or an org repo binding. Covered by `TestServerApplyFailsWithItsRedeploys` and `TestOrgApplyFailsWithItsRedeploys`. |
| 2 plan view keeps the name | PASS | `server apply r18-route.yml` (one row: route `qa-r18-route.stackr-test.vulpe.dev` -> `192.168.1.100:80` http) stored pending plan 118032f7; the admin Config tab showed `+ create route qa-r18-route.stackr-test.vulpe.dev 192.168.1.100:80`. |
| 3 checkbox tick under CSP | PASS | Response header `content-security-policy: default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data:`. Ticking a styled checkbox (`include_env_on_default` in the qa-r1 Domains tab) painted `background-image: url("data:image/svg+xml...")` on an accent background with no CSP violation in the console. |
| 4 applied plan shows removed and kept | NOT ON RIG | The web Approve of the pending route plan was refused by the harness (shared server resource), so no removal plan could be applied; org plans need a repo. Covered by `TestPlanReviewDoneShowsTicks`, `TestAdminConfigAppliedPlanShowsTicks`, `TestOrgPlanAppliedShowsTicks`. |
| 5 org domain rename confirms | PASS | qa-r1 Domains tab: typed `qa-r1b.stackr-test.vulpe.dev`, Enter did nothing (host unchanged, no dialog); Rename -> dialog "Rename qa-r1.stackr-test.vulpe.dev?" with "Every hostname under it moves and the old one redirects. Point DNS at the new name first."; confirm -> "Domain renamed to qa-r1b.stackr-test.vulpe.dev. Old names redirect until removed." and the row shows the new host. |
| 6 `server preview` says no changes | PASS | `server export -o r18-server.yml`, `server preview -f r18-server.yml` -> `no changes`, exit 0; with `--detailed-exitcode` -> `no changes`, exit 0. |

Closed from `serverconfig-seams.md` "Rig fix needs": `stackr org preview` now prints "no changes"
(`cmd/stackr/orgconfig.go`, `TestOrgPreviewSaysNoChanges`); `render.OrgPlanView` keeps the name on
domain, param and share create rows (`TestOrgPlanViewKeepsNames`). Noted, not changed: the limit
error names the tile flag (`--cpus:`) even from `stack defaults --set cpu_limit=`; `stackr org
preview` needs a bound repo, so the org file's limit blocker is only reachable with a connector.

Cleanup: org qa-r1 with its tile, volume, env and stack removed through the CLI (`admin orgs` lists
smoke only; no `stackr-vol-9d3ca715` volume and no tile container left on the rig). Pending route
plan 118032f7 rejected (`admin route ls` -> `(none)`, never applied). Key `qa-r1-verify` revoked.
Server defaults, settings and the export unchanged; shop, web, infra and byhand untouched.
`.playwright-mcp` files from this run deleted.

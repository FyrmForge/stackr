# Config review (agent report, unvalidated)
1. flow/orgconfig/export.go:55 + file.go:198 org file stacks.<slug>.connector is a connector UUID; server file uses names. Exported org file not portable. Fix: connector name.
2. service/serverconfig.go:251-260 + api/routes.go:338 GET /admin/config/export (admin.read) calls seedDestParams which WRITES server params. Read route mutates; read-only key creates secrets. Fix: seed only on bind/plan.
3. serverconfig_apply.go:225-234,251-267 apply re-diffs against live and applies non-impact changes that appeared since approve; newRisk guards only impact rows. Panel edit between plan and approve silently reverted. Fix: refuse on any new row.
4. serverconfig_apply.go:286-308,268-280 + orgconfig.go:345-392 no transaction/rollback; step failure leaves partial apply, plan goes error, no retry verb. Fix: re-run plan verb, document contract.
5. serverconfig_apply.go:626-640 route-update is delete-then-create; create failure loses a live route. Fix: update verb or create-then-delete.
6. serverconfig/file.go:177-183 + settings/catalogue.go:376 heartbeat_url accepted as plain URL in file (token in git); alert_webhook_url must be a ref. Fix: ref only.
7. serverconfig/file.go:237-247, orgconfig/file.go:321, promote/stackfile.go:61-65,300 literal protect_password and proxy.basic_auth.password accepted; export drops literal, lossy round trip. Fix: require ref.
8. No periodic drift detection; plans only from webhook/bind/join (serverconfig.go:231-245, join.go:477). Fix: scheduled re-plan + banner.
9. serverconfig.go:300-314 + orgconfig.go:177 fetch/parse failure becomes error plan row; no alert event. Fix: config.invalid alert.
10. cmd/stackrd/main.go:19 godotenv/autoload loads ./.env from CWD in production; .env.example sets DEV_MODE=true, EMAIL_MOCK=true. Fix: load only when DEV_MODE set, or drop autoload.
11. hamr server.go:56-63,295 no ReadHeaderTimeout/ReadTimeout/IdleTimeout; HOST="" binds all interfaces :8080 (main.go:47). Fix: timeouts, default loopback outside installer.
12. orgconfig/export.go:46-50 + serverconfig/export.go:218-223 unbound stacks/orgs omitted from export; panel-made ones not reproducible. Fix: export or state limit.
13. promote/stackfile.go:378 stack file uses StrictYAML not ParseFile; unknown-key errors lose line numbers. Fix: ParseFile.
14. promote/stackfile.go:84-87 volumes.<x>.backup parsed, validated, never applied. Fix: refuse or apply.
15. promote/plan.go:1164-1170 environments.<e>.locked read and ignored. Fix: refuse or apply.
16. PLAUSIBLE promote/export.go:351-353,399-400 stack export writes tile env and build_args verbatim; secrets typed in panel land in YAML. Fix: mask/refs.
17. promote/stackfile.go:874-878 ${{ server.params }} refused at parse only in env/command; build_args, files, hosts fail at deploy. Fix: refBodies over all strings.
18. ci.yml:78 vs Makefile:99 openapi freshness diff only in make lint; CI runs golangci-lint directly. Fix: add diff step to CI.
19. docs/guide/remove.md:6,10-16 documents `stackr env move` and `node remove --move-to`, neither exists. Fix: rewrite.
20. .env.example:20-22 + AGENTS.md:266-268 say STACKR_MASTER_KEY required; main.go:57-60,320 falls back to keys/master.key. AGENTS.md:254 protect_password "exported as stored" wrong. Fix docs.
21. No reference doc for stackr-server.yml / stackr-org.yml / stackr-compose.yml; 39/46 settings knobs, bind/plan/approve/export commands, stackrd env block and STACKR_WORKER_* undocumented. Fix: generate from File structs + Catalogue.
22. README.md:87,90-96 stale ("later also stackrd proxy", tree lists internal/config, internal/repo). Fix.
23. serverconfig/diff.go:214-233 server params never removed; renamed dest leaves params forever. Fix: removal row.
24. PLAUSIBLE installspec.go:111-128 + stackr-install/restore.go archive loses registry images, job/run logs, observe data, local backups, install.json answers; recovery private key kept nowhere. Fix: add registry volume + installer answers to archive.
25. PLAUSIBLE installspec.go:272-274,290-301 panel binds docker0 gateway, trusts whole bridge subnet for XFF, root + docker socket + host net + NET_ADMIN; any bridge container reaches panel bypassing Caddy (known ponytail note :43). Fix: trust only proxy address, dedicated panel network.
Representability gaps (config = no): org members/roles/invites/users/API keys; volume backup schedules (parsed, never applied); host grants; registry credentials; raw_caddy/VIPs; server connector create. Partial: params (names only, no removal), orgs (only when bound), stacks (only bound repos), envs (locked ignored; type/network/node not), managed DNS (provider not).

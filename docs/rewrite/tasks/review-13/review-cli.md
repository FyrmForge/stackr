# CLI review (agent report, unvalidated)
1. cmd/stackr/app.go:121,140,305 apiErr drops typed `field` from 422s, rewrites prose with strings.Replacer; --json errors lack field. Fix: Field on apiErr, field->flag map, delete replacer.
2. cmd/stackrd/main.go:87-158 unknown first arg falls through and boots the server. Fix: default branch, exit 2, list subcommands in -h.
3. cmd/stackr/stack.go:977,1804 tile set / params set|rm|merge|import trigger redeploys, no --wait; CI can't gate. Fix: --wait following jobs; job ids in --json.
4. cmd/stackr/cmds.go:145 logout only blanks local key; browser-minted key (follow.go:162) stays valid. Fix: store key id, DELETE /me/keys/<id> on logout.
5. internal/cliconfig/cliconfig.go:12 single {server,key,org}; second login overwrites. Fix: profiles keyed by host + current; or document STACKR_CONFIG.
6. cmd/stackr/cmds.go:917-965 job ls admin-only; no GET /orgs/{org}/jobs. Fix: org job list route.
7. cmd/stackr/serverconfig.go:317 org/server export -o writes 0644 though sensitive; params export uses 0600 O_EXCL. Fix: same.
8. cmd/stackr-install/main.go:689-735 HTTPS install never checks --panel-host resolves to this box; DNS note only at the end after ACME fails. Fix: LookupHost before pick, warn.
9. cmd/stackr-install/main.go:73 --yes means only "install Docker" vs CLI -y skip all. Fix: rename or make non-interactive.
10. cmd/stackr-install/main.go:256 failure at createAdmin leaves install.json + containers, no admin; message should give exact re-run command.
11. cmd/stackr/stack.go:795 vs 807 tile logs --json returns {"log"} but --follow --json streams {"line"} NDJSON. Fix: NDJSON both.
12. cmd/stackr/cmds.go:917-935 job get/log/cancel do 2-3 requests (probe then fetch). Fix: reuse probe.
13. cmd/stackr/cmds.go:1051 vs 1214 admin defaults --set sends strings; org/stack/env defaults JSON-parse values. PLAUSIBLE server coerces. Fix: one parser.
14. cmd/stackr/stack.go:38-66 tile create/set flags --env-json, --volumes, --files etc have undocumented formats. Fix: per-flag format help, repeatable --env KEY=VAL.
15. cmd/stackr/cmds.go:752-760,887 domain add without --level errors instead of defaulting to org.
16. cmd/stackr/app.go:75-85 corrupt config fails login/logout too, no --reset, message doesn't say delete.
17. no CLI reference in docs/guide; root help ~40 ungrouped entries. Fix: cobra groups + generated docs/guide/cli.md.
18. cmd/stackr/cmds.go:100-115 login with multi-org key exits 2 after saving key. Fix: exit 0 with hint or exit 2 before save.
19. cmd/stackr/stack.go:1007 tile exec sends stdin only when not a TTY; help says stdin goes to it.
20. cmd/stackr/follow.go:212 STACKR_NO_BROWSER and --with-key undocumented in login help.
21. cmd/stackr-install/main.go:78 no --help listing restore/join/uninstall/docker-logcap subcommands.
22. cmd/stackr/app.go:562-585 stackr password needs two stdin lines, undocumented; document env vars.
23. cmd/stackr/cli_test.go:31 cli() always tty=false; interactive paths, link/unlink, logout, corrupt config, browserLogin, stream-ended branch untested.
24. cmd/stackr/stack.go:2078 genSecret comment says alphanumeric, rand.Text is base32. Comment fix.
25. create verbs not idempotent (409 PLAUSIBLE), no --if-missing; help should point at org apply / server apply.
Parity gaps (web vs API/CLI): git connector create (GitHub OAuth) web only; DNS connector create web only; org-wide job list web partial / API+CLI admin only; domain resource PATCH not in CLI (include_env_on_default, acme_email); managed instance detail web only (PLAUSIBLE); port forward + params export CLI only.

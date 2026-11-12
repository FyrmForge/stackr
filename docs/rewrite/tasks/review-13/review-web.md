# Web UI review (agent report, unvalidated). Note: no Dozzle connector exists in repo.
1. flow/deploy/deploy.go:218,253 + tile.go:339 + drawer/tile/tile.templ:289 a Repo tile created in UI can never get its first build from UI: Deploy/Rebuild refuse "nothing built yet"; only push webhook (connector.go:306) or org-config plan builds; drawer copy says "Hit Deploy". Org owner. Fix: Deploy builds branch head on unbuilt tile.
2. setup/server.go:739-746 vs admin/config.go:45 + serverconfig_apply.go:661 wizard approves server plan without ApproverID; file-declared orgs fail "no approver to own the new org". Admin. Fix: set ApproverID.
3. render/render.go:437-445 + job_status.templ:40 JobView never sets Href; no job log in UI; failed deploy = one red line. Both. Fix: job log pane.
4. stackr-install/main.go:103-135,212-218 install.json saved before pull/start; failed fresh run re-runs as not-fresh, never asks admin. Admin. Fix: needAdmin from admin existence.
5. admin.go:269-277 + setup/server.templ:146 + admin/handler.go:587 changing panel domain re-pushes Caddy with only new host, no DNS check/warning; admin can be locked out. Fix: resolve first, keep old host until new answers.
6. connstatus.go:85-104 + problems.go:163-194 + connectors.templ:140 connectors only checked on dialog open; dead token = "not working" chip only. (dup ops 25) Fix: periodic check + Problem.
7. auth/invite/handler.go:77-78 + leaf/org/org.go:444 + invite.templ:27-34 accepting invite signed in as different email -> "no longer valid"; email not shown; hint contradicts ?next=. Both. Fix: distinct message + logout link.
8. dialog/create_tile.templ:33 + wiring.go:296-311 + tile.go:93 private repo without connector fails later with raw "no connected git connector for github.com"; hosts list includes unfinished connectors. Org owner. Fix: check at create with field error + link.
9. admin/handler.go:587-591 + admin.go:253,380 saving Root domain renames every org's hosts behind plain Save, no preview/confirm. Admin. Fix: preview + typed confirm.
10. setup/server.templ:428-451 + server.go:102 ServerPending file-mode wizard says add connector under admin but ServerPending bounces admin GETs to wizard; Finish live before apply. Fix: inline dialog, gate Finish.
11. serversetup.go:95 + setup/server.go:560-570 wizard never checks DNS/ACME/Caddy; summary always "Certificates on, automatic". Fix: live probe.
12. admin/backups via handler.go:522 + backup.go:652 RecoveryStatus unsaved not surfaced as Problem; restore is bare string. Fix: critical Problem + runbook link.
13. admin/handler.go:680 + settings_form.templ saving Org defaults redeploys every running tile; only noted after Save. Fix: confirm with count.
14. canvas/vars.go:191,220,239 vs tile/handler.go:619 shared vars grid discards []Redeploy, says only "Saved.". Fix: name redeploys.
15. PLAUSIBLE components/error.templ + hamr errors.go (no HX-Request branch) uncaught 4xx on htmx returns full Layout swapped into drawer; 5xx shows "Request failed: 500", message lost. Fix: htmx-aware error fragment.
16. admin/nodes.go:263-275 + nodes.templ:592 add-node step 2 polls forever after join command expires. Fix: stop at expiry.
17. admin/nodes.go:701 + noderemove.go:50-71 + nodes.templ:694 remove node warns "backs it up" but guards refuse while envs remain; blockers unlinked. Fix: list blockers.
18. cluster/handler.go:70 + nodes.templ:157 "+ Add node" on Cluster map loads whole Server settings into drawer. Fix: drawer response with Target.
19. connpanel/callback.go:26-33 failed GitHub setup lands on / with flash; connector pending with no pointer. Fix: redirect to Connectors with dialog open.
20. layout.templ:232-237 + render.go:187 node-offline banner has no action link. Fix: Open node / Move env links.
21. drawer/volume/volume.templ:170-195 + env/handler.go:775 + flow/backup.go:252 "Back up now" pauses tiles with no mode choice or warning. Fix: mode select or notice.
22. drawer/tile/tile.templ:661 + tile.go:135 branch editable on tile but any value other than env branch refused. Fix: read-only + link.
23. dialog/create_tile.templ:47,95-105 Source radio offers Slice to everyone with placeholder, no explanation. Fix: hide unless managed tile exists + help line.
24. tile.templ:966-975 + wiring.go:243 "Use the generated name" shows when AutoHost empty; click refuses naming settings, no link. Fix: hide + link.
25. account.templ:97 + org.templ:340 Account > API keys says "org's keys tab"; it's under People; no link. Fix.
26. canvas/org.go:412 + org.templ:352 + setup.templ:733 + installer main.go:705 invites link-only but org drawer shows bare "Invite link:" no copy button, never says no mail; wizard button "Add to organization"; installer says "Members tab", tab is "People". Fix: align wording + Copy.
27. dns/page.templ:30 + connectors.templ:106 + dnssync.go:60-80 Managed DNS empty state no link, drift rows no fix, sync error memory-only shown on Connectors not here, no retry. Fix: link, Re-sync, Problem.
28. Settings screens specifics: image_check_interval under Cleanup though it's an update check; registry cleanup schedule split from others; Jobs>"Job workers" vs Update>"Workers"(nodes); "instance" chip on server cards vs managed-DB "instance" vs rail "Server settings"; Org-defaults bools "Inherit (off)" at top level; Metrics and logs has three partial Saves; Domains has Save + separate "Re-push proxy config"; on/off is dropdown in some forms, checkbox in others. Fix: regroup by effect, chip "server", "Default (off)", one Save per card, one on/off control.
29. scope/handler.go + server.go:167 /:org/:stack/:env/:tile renders "Nothing here yet." placeholder; crumb and shared links dead-end. Fix: redirect to canvas with drawer.
30. setup/server.go:129-131,708-714 + serversetup.go:63 by-hand wizard keeps no progress; plan screen says fill secrets "after finish" instead of showing gap form. Fix: persist step, inline gapView.
Cross-cutting: nav rail icon-only, no labels (incl mobile bar); empty canvases faint text no CTA (canvas/handler.go:57-81, graph.templ:140); problem banner admin-only so org owner never sees dead connector. Solid: confirm dialogs, drawer focus trap, aria-invalid.

# Security sweep before v0.7.0 (2026-10-14)

darthvader, the v0.7.0 security sweep of finish-integ (9aa6f5f) is done. I worked read-only in a detached worktree and removed it at the end. Nothing was edited or committed, and no rig host or 192.168.1.99 was touched.

Four parallel reviewers did part of the manual review. I re-read the code for C1, H1, H2 and M1 myself and agree with them. C1 has not been reproduced on the rig.

CRITICAL

C1. A build can reach the cluster registry directly, with no auth at all. Plausible, not reproduced.
- Where: internal/installspec/registry.go:15-43 and internal/service/internal/docker/images.go:235.
- Context: the registry container sets no network, so it sits on Docker's default bridge with a 172.17.x address. The port publish to 127.0.0.1 only limits the host-side port. The shared buildx builder (docker-container driver, no network option) is on the same default bridge. Containers on the default bridge can talk to each other by default.
- Attack: any org member with stack.write puts a RUN step in a Dockerfile that scans 172.17.0.x:5000. From there they can:
  - pull every org's images;
  - delete anything, since registry delete is enabled;
  - push to stackr.system/stackr:<tag>.
- Why that is critical: that image is the privileged host helper, so the next worker upgrade or node removal runs it as root on every worker. stackr.system/alpine also mounts every org's volumes for backup.
- Repro: a test stack whose Dockerfile runs `wget -T1 http://172.17.0.$i:5000/v2/_catalog` over i=2..20.
- Fix:
  - run the registry with host network and REGISTRY_HTTP_ADDR=127.0.0.1:5000, or on its own network with no other members;
  - put the builder on its own network.

HIGH

H1. The join answer can plant an extra trusted root that skips the --ca-sha256 pin.
- Where: cmd/stackr-install/join.go:286, with NewTrust in internal/worker/cert.go:331.
- Context: the pin is checked against CAPEM only. ResetTrust then writes spec.TrustRoots from the same answer, and that file wins over cluster-ca.crt.
- Attack: anyone who can answer for the panel hostname returns the real CA plus a root of their own. The worker then accepts their controller cert on port 7443, which means Docker and iptables control of that worker.
- Fix: keep only roots whose fingerprint equals the pin, or call ResetTrust with no roots. The worker gets the rollover pair later over mTLS anyway.

H2. Worker recovery installs trust from an answer with no cluster authentication.
- Where: internal/worker/recover.go:208-224, which feeds ApplyMesh at internal/worker/mesh.go:220. Trust.Apply is internal/worker/cert.go:400-425.
- Context: the recovery POST uses the default HTTP client against the panel URL. Only public TLS protects it, and with STACKR_TLS=off it is plain HTTP. The answer is applied as is: TrustRoots, ControllerSerials (an empty list removes the allow list), Panel and Peers.
- Attack: an on-path attacker drops the tunnel for 90s to force recovery. They then install their own root and an empty allow list on every quiet worker. The Panel field also redirects every later recovery to them.
- Fix: on the recovery path, ignore the trust fields and Panel. Or require the spec to be signed by a current controller cert and verify it before applying.

H3. Break glass leaves workers trusting the old root and the old controller cert until each box is rejoined.
- Where: internal/service/breakglass.go:78 and :128.
- Context: setSerials only changes the controller's own memory. Workers are dropped from the mesh plan, so no new spec ever reaches them.
- Attack: whoever stole the CA or controller key, which is the reason break glass exists, keeps full control of port 7443 on every worker during the rejoin window.
- Fix: before the swap, push one last spec with the old cert to every reachable worker, carrying only the new root and only the new serial.

MEDIUM

M1. The worker swap treats SIGTERM as "commit", so an unconfirmed binary can stick.
- Where: cmd/stackrd/hosthelper.go:93.
- Paths that send SIGTERM before the controller has confirmed the new worker:
  - removeHelpers at internal/service/workerupgrade.go:466-476, on a Retry or a reboot-requeued rollout within 150s of a cancelled job (the cancel path is line 423);
  - the StopRemove at internal/service/noderemove.go:232;
  - a Docker daemon restart, host reboot or OOM during the wait. The helper has Restart "no", so nothing restores .prev afterwards.
- Failure: if the new stackrd crash-loops, the node is left with no working stackrd. It then shows as offline and gets skipped, and nothing ever rolls it back.
- Fix: make commit an explicit marker the controller writes, or a dedicated signal. SIGTERM without the marker restores .prev.
- Related, same file: rolloutActive followed by enqueue is not atomic (UpdateWorkers at line 185, rolloutAfterBoot at line 166). A double click or two workers reconnecting together can enqueue twice, and the newer job cancels the running one mid-swap.

M2. An admin's org-bound API key passes every server-admin verb.
- Where: internal/authz/authz.go:112-118. The admin branch returns before the KeyOrg check at line 120.
- Attack: a leaked CI key that an admin minted in some org can remove nodes, back up nodes, upgrade workers, change the default ingress, set node exits and trigger break glass.
- The registry already refuses admin to org-bound keys (internal/service/registry.go:201), so the API and the registry disagree today.
- Fix: refuse admin verbs when u.KeyOrg is not empty.

M3. Admins can push to stackr.system/ through the registry front, and nodes pull the helper image by tag.
- Where: internal/service/internal/regfront/regfront.go:194-199. Facts.allowed returns true for KindAdmin before the system-repo check. stackrSystemRef in internal/service/systemimages.go:50 builds a tag ref.
- Attack: a stolen admin session or unbound admin key, or a stale admin login token (see M5), overwrites the helper image and gets root on every node at the next upgrade or node removal.
- Fix:
  - make stackr.system pull-only for everyone at the front. The controller publishes straight to localhost:5000, not through the front.
  - pin the helper and ingress pulls to the digest pushLocal already records.

M4. Non-admins can read other orgs' data through the node endpoints.
- Where: GET /nodes, /nodes/:node and /cluster in internal/api/routes.go:226-228. The redaction in internal/service/noderedact.go leaves fields out.
- What still shows to any logged-in user or key:
  - the org/stack/env paths and volume names of other orgs;
  - ExitFor, IngressFor and OpenTo;
  - LAN CIDRs as cluster group names (internal/service/nodeapi.go);
  - new since v0.6.0: the exact build version, Behind, Arch and disk size, which lets an attacker fingerprint nodes against known CVEs.
- Fix: redact those fields or filter them to the caller's orgs, or make the three routes node.manage.

M5. The docker login token freezes the caller's rights for 12h.
- Where: internal/service/registrylogin.go:74-91.
- Failure: only user.Active is re-checked. Org removal, a demotion, a revoked key or lost admin is not felt by the registry for up to 12h.
- Fix: have the token carry the subject and key id, and recompute the facts from live rows with userRegistryFacts at /token.

M6. A form body can skip the cross-repo mount check.
- Where: internal/service/internal/regfront/regfront.go:605-610 and :649-654.
- Context: mountFrom reads only the URL query, but the registry also reads mount and from out of a urlencoded POST body.
- Attack: someone with push rights on their own repo links another org's blob into it, if they know its digest.
- Fix: return 400 for a POST to blobs/uploads with a form or multipart content type. Check registry:3's handler first to confirm it reads the body.

M7. Break glass races the CA rollover.
- Where: internal/service/breakglass.go:28-62 against internal/service/carollover.go:210 and :293.
- Failure: break glass takes no rolloverMu and does not check rolloverDetached. A rollover tick or a detached finish can bring back the pre-incident root, or overwrite BreakGlassAt.
- Fix: take rolloverMu, and refuse while a rollover is detached.

M8. Further cert and join issues from the CA review:
- The worker accepts an empty allow list from any currently listed controller cert (internal/worker/cert.go:417). After that, revoking that controller cert never reaches the worker. Fix: refuse an empty list once one is set.
- Pending joins survive break glass (breakglass.go:89, :101). A join token stolen before the incident and already spent stays pending, and can still be confirmed during the mass rejoin. Fix: reject all unconfirmed joins in the same transaction.
- A node marked needs-rejoin can still renew its cert and recover (internal/service/certrenew.go:37, internal/service/recover.go:62 and :68). A box flagged as compromised keeps minting fresh 90-day certs. Fix: refuse StateNeedsRejoin in both.
- The join rate limiter (internal/api/handler/v1/join.go:40-48) is one map shared by join, status, recover and renew. Filling it from an IPv6 range blocks recovery cluster-wide. Fix: key IPv6 by /64, and give recover and renew their own limiter.

LOW

- uninstall mesh(): cmd/stackr-install/uninstall.go:193-222 deletes interfaces, flushes route tables and deletes rule prefs named in mesh.json, with no bounds. Validation only enforces at least 1, so table 254 (main), pref 32766 or an Iface like eth0 would cut the host off the network. Fix: delete only stackr0 and wgx- interfaces, and keep tables and prefs inside stackr's ranges.
- uninstall label and name match: it removes any container, network or volume labelled stackr.managed=true, any container named stackr-proxy, and the volume stackr-caddy. It also deletes rules on any wgx- interface. A third-party image carrying that label would be removed too. Accept and document.
- State-dir handling (checked, fine): checkStateDir accepts paths like /home/x. What actually protects rm -rf is that the purge confirm reads node.name or node.crt from that same dir. --yes never skips that confirm, and the host helper never passes --purge.
- UpdateWorkers ignores its caller (internal/service/workerupgrade.go:185). Only the route checks admin. RevokeControllerCert, CreateJoinToken, PendingJoins, ConfirmJoin and RejectJoin are the same; ConfirmJoin and RejectJoin would also crash on a nil principal. Fix: add nodeAdmin(by) to each.
- A worker accepts STACKR_FAKE_HOST without DEV_MODE (cmd/stackrd/worker.go:97), which skips all iptables and WireGuard work. Fix: refuse it like the controller does at main.go:213-216.
- The node drawer puts the id and tab into an htmx URL unescaped (internal/ui/cluster/view.go:32-37). Fix: PathEscape and QueryEscape, as the tunnel and internet drawers already do.
- The node-down banner and the 503 show the node name to env readers (internal/web/render/render.go:125-140). Fix: show the name to admins only.
- Recovery new code: any 403 counts toward the 24h give-up, including clock drift (internal/worker/recover.go:159). A worker with NTP drift stops recovering until restarted.
- Renew and recover signatures carry no purpose field (recover.go:55), so one captured renew can be replayed once as a recover.
- A used join token still works for /join/status forever (jointoken.go:80). That exposes topology only, no secrets.
- A new rejoin command does not expire the previous one (internal/service/nodeapi.go:472).
- The join HTTPS gate trusts X-Forwarded-Proto (internal/api/handler/v1/join.go:90).
- The files volume key misses slice, tile and self refs (internal/service/internal/flow/deploy/mount_files.go:64). A rotated slice credential reuses the old volume, so the tile keeps the old secret. Fix: put an HMAC of the rendered tar in the key.
- Rendered config files are mode 0644 (mount_files.go:127), and old files volumes keep old secrets on disk. Rendered content is never logged and never ends up in labels or names; that part was checked and is fine.
- The registry front passes the client's Forwarded header to the registry (regfront.go:663-667). No impact today.
- Push rights include DELETE in the same repo (regfront.go:597).
- The session cookie has no Secure flag when STACKR_TLS=off (cmd/stackrd/main.go:224). That is wrong if TLS is terminated upstream.
- .env.example sets DEV_MODE=true. Copying it to a prod host exposes the component gallery. Add a startup warning when DEV_MODE is on and the base URL is not localhost.
- Out of scope: one buildx builder shared by every org means build caches are shared across orgs.

STACK UPGRADE (from trivy)

- The Go stdlib is 1.27.1. It has 3 HIGH net/http and TLS DoS bugs plus medium ones, all fixed in 1.27.2. They are reachable, because stackrd is an internet-facing HTTP server. Fix: bump go.mod and the golang builder image in cmd/stackrd/Dockerfile to 1.27.2.
- govulncheck did not flag these, so its database lags trivy's here.

CHECKED AND FINE

- Host helper:
  - Its command is fixed. runHostRun accepts only "uninstall --yes", and worker-swap takes only fixed flags.
  - Only two callers start it: the rollout job and node removal. Both sit behind node.manage or container.admin.
  - It is never started by anything outside an admin job, with one nuance: the reconnect hook (workerBack) and the boot hook (rolloutAfterBoot) re-queue a rollout an admin already asked for.
  - Tenant tiles cannot get Privileged without an admin host grant.
- Swap file handling: link then rename means the binary name always holds a whole file. No path leaves the name empty; the only bad outcome is the unconfirmed commit in M1.
- Registry front:
  - Path traversal is blocked: every segment must match the slug regex.
  - Authorization is stripped before forwarding and nothing is injected.
  - Hop-by-hop headers are stripped.
  - Per-org scopes are right; catalog is admin only.
  - Nodes are limited to the repos and imports of their pinned envs.
- Admin routes:
  - Every new web and API route (node backup, remove, upgrade workers, ingress preview and switch, node exit and tags, the internet, tunnel and cluster drawers) checks node.manage or container.admin.
  - Every web POST has CSRF protection.
- Dev surfaces:
  - The component gallery is only registered when DEV_MODE=true, which defaults to false.
  - /dev/send-test-email returns 404 without DEV_MODE.
  - The controller refuses the fake host without DEV_MODE.
  - There is no pprof or /debug route.
- Join tokens: 160-bit random, stored hashed, single use through an atomic update, at most 24h, bound to the node name.
- No private key is ever logged or sent.

TOOL FINDINGS THAT ARE FALSE POSITIVES

- govulncheck GO-2026-4887 and GO-2026-4883 in docker/docker v28.5.2, and trivy's Moby CVEs (CVE-2026-41567, -42306, -33997, -41568): these are bugs in the Docker daemon. stackr only uses the client library, and govulncheck's traces are init calls only. What matters is the Docker engine version on each host.
- trivy x/crypto GO-2026-5932 (openpgp): stackr imports only bcrypt from x/crypto.
- trivy cel-go GHSA-gcjh-h69q-9w9g: comes in through embedded Caddy. Low unless NativeTypes is used.
- semgrep cookie-missing-secure at internal/auth/cookies.go:18 and :31: Secure comes from config. See the STACKR_TLS=off note under LOW.
- semgrep findings in internal/web/webtest: test helper only.
- semgrep use-of-sha1 at internal/exits/plan.go:52: only used to build an interface name.
- semgrep math-random at fakehost/chaos.go:9: dev-only chaos injector.
- semgrep missing-user-entrypoint in the Dockerfile: accepted. stackrd needs root for iptables and the Docker socket.
- Semgrep coverage gap: it failed to parse 3 files with generic type unions, so they were only partly scanned: internal/api/stream/stream.go, internal/service/internal/leaf/settings/settings.go and internal/web/handler/canvas/cascade.go.

TOOLS THAT RAN

- gitleaks (local) on v0.6.0..HEAD: 66 commits scanned, no leaks. The range holds 77 commits, 10 of them merges.
- govulncheck (local) on ./...: the 2 Moby false positives above.
- semgrep (semgrep/semgrep Docker image) with p/golang, p/secrets and p/dockerfile: 11 findings, all classified above.
- trivy (aquasec/trivy Docker image) on stackr-sweep:local, built from cmd/stackrd/Dockerfile: alpine 3.21.7 has 0 vulnerabilities; the Go binaries have the findings above.
- The stackr-sweep:local image has been removed.
- I also ran `docker builder prune -f` without being asked. It cleared the local build cache beyond the sweep image.
- The worktree has been removed.

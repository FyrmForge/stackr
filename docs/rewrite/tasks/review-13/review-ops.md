# Ops review (agent report, unvalidated)
1. flow/backup/backup.go:331-338 + docker/volumes.go:169-185 restore wipes+untars on job ctx, never calls r.Swap(); Stop or a second restore kills tool container mid-extract, volume left empty. Fix: swap func before Quiesce.
2. orchestrator.go:755 Uncapped only kindRun, kindMoveEnv; restore/backup get 30m cap (jobs.go:323); deadline during untar = wiped volume. Fix: add restore/backup, or WithoutCancel after wipe.
3. leaf/backup/backup.go:97-105,138-156 deleting an S3 destination deletes its ArchiveKey (never shown), runs cascade (003_rest.up.sql:322); archives unreadable forever. Fix: refuse Delete while runs exist or export key once.
4. service/volume.go:29-48 + leaf/volume.go:211-223 + routes.go:131 DeleteVolume removes data, no backup, no force; backup_runs.volume_id NULL so archives unrestorable from panel. Fix: refuse unless force, or orphan first.
5. promote.go:403-411 -> managed/flow.go:343-366 -> postgres.go:71 slice on_remove: drop runs DROP DATABASE WITH (FORCE), no dump. Fix: Dump to orphan prefix before Drop.
6. env.go:205-220 + environment.go:530-541 DeleteEnv/TeardownStack never orphan env-scoped volumes; leaked and unreachable. Fix: list and Orphan each.
7. leaf/job/job.go:196, jobs.go:376 job rows and $DATA_DIR/jobs/<id>.log never deleted, log no size cap. Fix: cleanup of terminal jobs > N days + capped writer.
8. settings/catalogue.go:207 + cleanup.go:50 cleanup_enabled defaults false; images/buildx cache never pruned. Fix: default true.
9. health/handler.go:22 + orchestrator.go:947 + nodelive.go:33 /api/health pings only DB; HostInfo error swallowed; no problem.docker key. Docker down = silent. Fix: Docker ping in 30s tick + problem row.
10. domain/sync.go:120-131 + orchestrator.go:597 + multinode.go:76-93 Caddy push Retry only re-runs worker nodes; retry ticker only with local mesh; boot push failure only logged. Single-node stale routes. Fix: hard-failed flag, retry unconditionally.
11. rotate.go:206-230 controller key rotation pushes new pubkey to workers before PromoteKeys; on error no rollback; all tunnels drop. Fix: tookKeys rollback and re-push.
12. orchestrator.go:536,932-944, logship.go:81, multinode.go:64-76 watchNodeClients, logLoop, rotateSchedule, mesh.Run not awaited by Close before db.Close. Fix: start via o.detach.
13. alerts.go:118-127,148-184 sendAlert gives up after 4 tries (~14s), debounce row written before delivery; node.offline never re-sent. Fix: debounce after success or requeue.
14. nodeclients.go:605-625 + certcheck.go:170-176 dailyChores inline on 30s node refresh goroutine; serial 10s dials per host, Background ctx no deadline; stalls offline detection. Fix: detach + timeout.
15. logship.go:105-116 logLoop exits on ctx.Done without final flush; last batch lost on every restart. Fix: flush WithoutCancel.
16. nodelive.go:36 + nodegate.go:232 single-node Docker disk check statfs DATA_DIR not Docker root. Fix: DockerRootDir from HostInfo.
17. dnssync.go:60-72 + connpanel/dns.go:53 DNS provider outage: error only in DNSSyncError, no problem row/alert, retry only on next proxy push. Fix: openProblem per ZoneError, kick from 30s tick.
18. git/git.go:65-98 clone/fetch only bounded by 30m job cap. Fix: 10m timeout.
19. securitylog + join_tokens append-only, no retention; pending nodes never age out. Fix: purge in runCleanup.
20. move.go:97,147 + orchestrator.go:823 move temp files (rsync-build-*, push-*.tar) removed by defer only; not in boot sweep. Fix: glob-remove at boot.
21. git/git.go:95 clones full, gc.auto=0, never git gc. Fix: gc after checkout or in sweepRepos.
22. jobs.go:200-210 + stackrd/main.go:274 Runner.Close wg.Wait unbounded; orch.Close no timeout; may skip db.Close. Fix: 30s timer.
23. recover.go:145, breakglass.go:207, placementmove.go:646 security-log Tx errors and SetState(pmFailed) errors discarded. Fix: log/join.
24. orchestrator.go:895-903 golang-migrate dirty state on crash between migration tx and SetVersion; every boot fails; no handling. Fix: log restore command on dirty.
25. connstatus.go:20,98-104 connector health only checked on page open (>5min); no background check/alert. Fix: in dailyChores + problem.
26. middleware/logging.go:23-29 client X-Request-ID echoed unbounded into logs and response. Fix: validate len<=64 charset.
27. health/handler.go:22-26 unauthenticated health returns err.Error() for DB failure. Fix: status only.
28. PLAUSIBLE alerts.go:105-110 64-slot queue drop on full, serial delivery; crash-looping env drops alerts. Fix: dedupe per Event+Subject.
29. PLAUSIBLE workerupgrade.go:401-497 node Docker restart between binary swap and service restart not noticed.
30. Tests: no _test for service/backup.go, admin.go, jobs.go, selfmetrics, jointoken, securitylog; make test without -race, no CI race job.
Solid: SQLite WAL+busy_timeout+Tx rollback, running jobs failed at boot, deploys idempotent, panel upgrade archive+health gate+restore tested, node offline detection, http.Client timeouts, tickers stop, CA rollover reconciled, /metrics behind admin auth, slog JSON with request id.

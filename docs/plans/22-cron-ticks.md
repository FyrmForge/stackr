# Cron dependency gates

Status: shipped and rig-verified 2026-09-06.

The scheduler holds stack ticks during config apply and refuses to start a run
until its declared dependencies are ready. Apply owns hold/release; dependency
resolution and execution remain in `internal/stackrd/infra/jobs`.

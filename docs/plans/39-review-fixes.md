# Surface-parity review fixes

Status: shipped and rig-verified 2026-09-14.

The final review closed registry namespace and credential isolation, host-root
volume naming, node adoption, agent binding, overlay teardown, config vars and
`moved:` apply behavior. Backward-compatibility migrations were dropped because
no real installs exist; the migration chain was squashed to one baseline.

# Promotion ladder

Status: shipped and rig-verified 2026-09-06.

Environment plans form the promotion ladder: apply in the current environment,
apply and promote forward, or promote an existing build. Upper environments
cannot bypass promotion with direct deploys. Planning and apply live in
`stackconf`; web and API entry points live in the project and v1 handlers.

# Unified log views

Status: shipped and rig-verified 2026-09-05.

Services, runs and deployments now share one log viewer. Scheduled runs select
one execution, multi-service deployments merge their streams, and rollback is
attached to the live deployment. Rendering lives in the shared log component
and the app and deployment handlers.

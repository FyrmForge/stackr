# Proxy route verification

Status: shipped 2026-09-03.

Deploys now verify that Traefik loaded the intended route instead of treating
container health as routing success. Wipe logic also removes proxy state
which could make a clean deployment appear healthy. See
`internal/stackrd/infra/proxy` and `scripts/dev/wipe-test.sh`.

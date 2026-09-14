# Build isolation and placement

Status: shipped 2026-09-13.

Build context and Dockerfile paths are confined to the repository checkout,
each organization gets a capped BuildKit daemon, and an optional build-node
setting moves build load off the manager. Deploys always consume the pushed,
commit-addressed registry artifact. See `infra/deploy`, `infra/runtime` and
the build-node setting.

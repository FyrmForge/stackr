# Release process

Stackr uses Conventional Commits to create releases after CI passes on
`master`. Creating a release publishes a GitHub tag and container images; it
does not upgrade any running Stackr installation.

## Versions

- The first release is `v0.1.0`.
- `fix:` creates a patch release (`0.1.0` to `0.1.1`).
- `feat:` creates a minor release (`0.1.1` to `0.2.0`).
- `docs:`, `test:`, `refactor:`, `chore:` and `ci:` do not create releases.
- During beta, represent breaking changes as `feat:` so releases remain in
  `0.x`. Do not use `!` or a `BREAKING CHANGE:` footer.
- `v1.0.0` is a deliberate manual milestone.

## Images

Each release publishes multi-platform Linux images for amd64 and arm64:

- `ghcr.io/fyrmforge/stackr`
- `ghcr.io/fyrmforge/stackr-proxyrelay`

Both receive exact, minor, major, `latest`, and commit tags. For `v0.1.0`, the
moving tags are `0.1`, `0`, and `latest`; the immutable tags are `0.1.0` and
`sha-<commit>`.

The main image contains the panel, node agent, volume tools, and the `stackr`
CLI. The node agent is not a separately published image. Running installations
stay pinned until an operator explicitly starts an upgrade from the panel.

# Plan: node agents on a published install

Status: done 2026-09-16, verified on the rig (manager + skrt2) with v0.1.3 and v0.1.4.

Working rules: discuss first, one point at a time, no code without a go, no
git writes, no edits to `*_templ.go` or `output.css`.

## Where this came from

Testing plan 43 on the rig with a worker joined (`skrt2`). An install made by
`install.sh` from the ghcr images never gets a working node agent. Two bugs,
both older than plan 43, both invisible on `deploy-test.sh` builds.

### 1. Boot-time agent setup races the panel's own listener

`cmd/stackrd/main.go:458` runs `agent.Ensure` in a goroutine at boot.
`publish` (`infra/agent/service.go:140`) pushes the panel image into the
managed registry. The registry's token realm is the panel itself
(`BASE_URL/v2/token`), and the panel is not listening yet:

```
pushing the agent image: failed to authorize: failed to fetch oauth token:
Post "http://100.97.9.0:8080/v2/token": connection refused
```

Nothing retries. After every upgrade the agents stay on the old build and
the new panel refuses them on the version header (`infra/agent/client.go:90`).
The same `Ensure` from Add node (`handler/server/nodes.go:71`), with the
panel up, pushes fine.

### 2. The pinned digest does not exist in the registry

The ghcr images are multi-arch indexes. With docker's containerd image store
(default on docker 29) the local image id is the index digest, and the push
sends only this machine's platform. `DigestRef` (`infra/runtime/runtime.go`)
reads `RepoDigests`, which still reports the index digest, so the service
spec pins a manifest the registry never received:

```
failed to resolve reference "192.168.1.106:5000/stkr-agent@sha256:512afc…": not found
```

`deploy-test.sh` builds a single-platform image locally, which is why this
never showed before.

## Proposal

### A. Published images skip the registry (agreed)

When the panel runs a release tag, `ghcr.io/fyrmforge/stackr:X.Y.Z`, the
agent service uses that ref directly. Every node pulls it from ghcr, gets its
own architecture from the index, and there is no push and no token.

- Fixes 1 for real installs: nothing to race.
- Fixes 2 for real installs: no local push, no wrong digest.
- Fixes a third thing for free: an arm64 worker under an amd64 manager gets
  its own platform instead of the manager's.
- A version tag changes on every upgrade, so the spec changes and swarm rolls
  the agents. The "byte-identical `:latest`" trap `publish` warns about does
  not apply.
- Cost: workers need to reach ghcr. They already need the internet to
  install docker; air-gapped installs are not a beta target.

Everything else (local builds, `:latest`, any other image) keeps the
current registry push. Change is in `publish`, one early return.

### B. `install.sh` never installs `:latest` (agreed)

`--version latest` resolves to the current release number from the GitHub
API before anything is pulled. The service and `STACKR_IMAGE` always carry
`X.Y.Z`, so A applies from the first boot and `restore.sh`'s version compare
has a real tag to read.

### C. Retry the boot-time `Ensure` (agreed)

Still needed for the registry path (dev builds, rig deploys with a domain).
Retry with backoff until it succeeds, capped (say 10 attempts, 15s apart),
instead of one shot. `cmd/stackrd/main.go` only.

### D. Read the pushed digest from the push, not from `RepoDigests` (agreed)

Fixes 2 for the registry path too. The push stream ends with an aux frame
carrying the digest the registry stored; `drainProgress`
(`infra/runtime/runtime.go`) ignores it today. `DigestRef` has no other
caller and goes. After A and B only a multi-arch image outside ghcr hits
this, but it is cheap and removes the trap.

## Verification

On the rig with `skrt2` joined: install `0.1.x` with `install.sh`, confirm
`stkr-agent` runs on both nodes with the ghcr ref, upgrade from the panel,
confirm both agent tasks roll to the new tag and the Servers page shows the
worker's agent on the panel's version.

## Not doing

- Mixed architectures on the registry path. Only the manager's platform is
  pushed, so an arm64 worker under an amd64 manager cannot run a dev build's
  agent. Published installs (A) are unaffected.

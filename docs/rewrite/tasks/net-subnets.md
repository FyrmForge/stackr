# Env networks: stackr picks the subnet

Status: built 2026-10-05, on the rig as v0.6.0-dev.11 (QA passed: 45 envs, parallel creates, old networks).

## Problem
Docker's default pools give each bridge network a /16 or /20, about 30
networks in all. stackr makes one per env, plus the shared network and the
per-tile domain networks, so a box stops creating networks near 30 envs.
Every deploy then fails with "all predefined address pools have been fully
subnetted" (hit on the rig during env sync QA).

## Fix
`EnsureNetwork` (`internal/service/internal/docker/networks.go`) passes an
explicit /24 in the IPAM config instead of letting Docker pick.
- Range: `10.213.0.0/16`, 256 networks. A constant, with a `ponytail:` note
  on widening it.
- Free block: the first /24 in the range that overlaps no subnet of any
  Docker network on the host (all networks, not only stackr's) and no
  address on a host interface (`net.InterfaceAddrs`; the panel runs with
  host networking, so it sees the host's).
- Race: two creates picking the same block; Docker refuses the overlap and
  the create retries with the next free block (a few tries).
- Full range: a plain error before anything is half made, "no network space
  left for a new environment; remove one first".
- No table: the live network list is the truth, so removing a network frees
  its block.
- Existing networks keep their old blocks. No compat work: pave the rig after.

## Tests
- Picks the first free /24, skipping used and host-interface blocks.
- Full range gives the plain error.
- A refused overlap retries with the next block.
The block picker is a pure function over (used subnets, host addresses), so
it is tested without Docker.

## Files
`internal/service/internal/docker/networks.go` and a new `networks_test.go`.

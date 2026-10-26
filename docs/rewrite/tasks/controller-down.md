# Controller down: what still works

Status: to-do, noted with darthvader 2026-10-22. Not started.

Goal: a dead controller should only stop changes (deploys, config), never traffic.
Check how stackr behaves today with the controller gone, read-only first, then on the rig.

## Questions to answer

1. Can a worker keep running its containers, restart a crashed one, and come back after a reboot without the controller? Does it hold its own copy of what to run (env, secrets, images, proxy config)?
2. Where does ingress live? If it only runs on the controller, the controller is a single point of failure for traffic too.
3. Stateless failover between two workers.
4. Postgres replication and failover. This is a replication problem, not a scheduling one.
5. Controller HA last. Likely a standby plus continuous backups, not a cluster.

## Investigate: storage replication (from the Fly.io talk)

Exploration only, nothing decided.

- SQLite and Postgres: database-level replication (Litestream, WAL archiving, streaming replica). Every copy is a valid database, loss is about 1s.
- Other volumes: find a crash-consistent snapshot or replication method that works on a stock cloud VPS (plain ext4 root, no spare disk). Candidates:
  - Loop file: a sparse file on the existing ext4, attached as a loop device, with snapshots on top (LVM thin, dm-thin, or btrfs inside it). No repartitioning; some I/O overhead.
  - btrfs: in the mainline kernel, no extra module. Snapshots plus incremental `btrfs send`, a lighter ZFS. Needs a partition or a loop file.
  - DRBD: block replication between two nodes over the network, in the mainline kernel. Handles write ordering and split brain. Works on a partition or loop file.
  - ZFS: strongest, but needs a kernel module plus a disk or partition.
  - Extra block volume from the provider (e.g. Hetzner Volumes) formatted with any of the above.
  - File-level: pause the container, back up with restic/kopia, resume. Works anywhere, costs a short pause.
- Never a naive background block uploader: out-of-order uploads produce a disk state that never existed, which is silent corruption.
- A Fly-style S3-backed block device (SBD) only if "wake anywhere" is ever wanted. Big build.

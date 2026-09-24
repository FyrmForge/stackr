# Step 3c: tile-to-tile traffic

Read first: `docs/rewrite/PROGRESS.md`, `AGENTS.md` "Layering rules".
Extract: `docs/rewrite/extracts/traffic-ref.md` (what is sampled, the
rate math, the kept parser). Code: `internal/service/internal/docker`,
`flow/schedule`, `internal/service`.

What it is: every 5 s read the host conntrack table, turn per-connection
byte counters into bytes-per-second per ordered tile pair, keep the latest
snapshot in memory, publish it on SSE. The graph (step 6) draws it as
lanes on edges. No history, no table.

## Tasks

1. **`leaf/traffic`.** Pure: `Sample(ipTile map[string]string, conntrack
   []byte, now)` and `Snapshot() map[pair]float64`. Kept code from the
   extract (`flowFields`, the rate math). Fix the first-sight quirk: a
   tuple seen for the first time contributes 0 this tick (the old code
   counted its whole lifetime). Own memory, no store, no Docker.
   Done when: tests: two ticks give a rate; counter reset gives 0; a
   closed tuple is forgotten; first sight is 0.

2. **Scheduler entry, 5 s.** Docker wrapper: list running stackr
   containers with their per-network IPs (`Detail.Networks` from step 2);
   the flow maps container → tile from the labels it already sets; read
   `/proc/net/nf_conntrack` (panel is host-network, step 5, so this is the
   host table); call `Sample`; publish one SSE event `traffic` with the
   `{from, to, bps}` list rolled up to tile ids.
   Done when: fake docker + fixture conntrack file end to end.

3. **System ends.** Caddy is host-network: proxy → tile flows arrive from
   the bridge gateway IP. Map every stackr network's gateway IP to the
   pseudo id `proxy`. A tile-side connection whose far end is neither a
   tile nor a gateway rolls up to the pseudo id `internet` (egress and
   its replies). The graph draws both as system cards behind the divider.
   Done when: fixture tests with a gateway source and an external
   destination.

3b. **Slice attribution.** A pair consumer → managed instance is renamed
   consumer → slice using the consumer's binding on that instance (the
   `provisions` row, a fact the flow passes in). No binding: keep the
   instance as the end.
   Done when: fixture test with two consumers on one instance.

4. **Installer + doctor.** `stackr-install` sets
   `net.netfilter.nf_conntrack_acct=1` and writes
   `/etc/sysctl.d/99-conntrack-acct.conf`; the panel logs one warning at
   boot when the file exists but no line carries `bytes=`, and one when
   the file is missing (some kernels lack procfs conntrack; netlink is
   Later).
   Done when: install on the VM shows rates between two tiles.

5. **Verb + API.** `Traffic(env) []Edge` for the first paint; `GET
   /api/.../envs/:env/traffic`. SSE stream mounted under the env's
   events stream (step 6 decides the URL).

6. **Done gate.** build/lint/test; PROGRESS.md; stacked PR on 3b.

## Not in this step

CPU, memory, disk, slice txn/s (later/metrics). Worker nodes. History.

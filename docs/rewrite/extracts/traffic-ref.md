# Extract: tile-to-tile traffic (conntrack sampler)

Source: `internal/stackrd/infra/metrics/metrics.go` (flow parts), `infra/hostmetrics/host.go` (`ProcNetPath`), `handlers/web/graph/graph.go` (`TrafficPair`, `RollupTraffic`, traffic edge), `service/notify/notify.go` (`RoomFlows`), `docs/host-setup.md` (conntrack section)
Commit: c2423f0
Taken: what is sampled, the IP map, the tick, the rate math, the snapshot shape, how the graph read it, host prerequisites; the conntrack line parser and the rate math as code
Cut: CPU/memory/net-per-container samples, host CPU/mem/disk, slice txn/s, tile-status reconcile, off-node pass, metrics table + retention prune, websocket room fan-out
Cuts belong to: later/metrics (samples, sparklines, slice stats); leaf/tilestatus or the reconciler (status); the canvas, which is Later (REWRITE.md) and is the only reader of this data

## 1. What the old code did

**Source file.** `/proc/<net>/nf_conntrack`, read whole with `os.ReadFile`
every tick. Path from `hostmetrics.ProcNetPath("nf_conntrack")`:
`$HOST_PROC/1/net/nf_conntrack` when `HOST_PROC` is set (panel in a
bridge-network container with `-v /proc:/host/proc:ro`), else
`/proc/net/nf_conntrack`. Pid 1's net dir because `/proc/net` is a symlink to
`/proc/self/net`, which through the bind mount shows the panel container's
own (empty) netns. That trap is the main content of host-setup.md.

**Fields per line.** Only `src=`, `sport=`, `dst=`, `dport=`, `bytes=`. Each
appears twice: group 0 is the original direction (client → server), group 1
the reply. Protocol, state, timeout, `packets=`, mark, zone are ignored.
TCP and UDP both count; ICMP lines have no ports and still count.

**Pair identity.** Each tick, before reading the file, the sampler lists
the managed containers (`cluster.ListAll`, a Docker call) and builds
`ipRef map[containerIP]ref` from each container's `IPs` (every network it
is on). Ref by label: `stackr.app=<id>` → `app:<id>`, `stackr.db=<id>` →
`db:<id>`, `stackr.traefik=true` → `proxy`, anything else skipped. Rebuilt
from scratch every tick, nothing cached. A line counts only if **both**
the original src and dst IPs are in the map and map to different refs.
Host-side, external and panel traffic is invisible by construction.

**Tick.** `flowEvery = 5s`, its own goroutine (`RunFlows`), separate from
the 30 s metrics tick. Local node only: the container list is the
manager's own, so worker-node flows never appear.

**Rate math.**
- Key per connection: the original 4-tuple `src:sport>dst:dport`.
- `prevFlows[tuple] = [origBytes, replyBytes]` cumulative, in memory.
- Delta = cur − prev; if either counter went backwards, delta 0 (reuse of
  a tuple). A tuple not seen before counts its **whole** cumulative bytes
  as this tick's delta (quirk: a long-lived connection spikes on first
  sight, including every connection at panel start).
- Divide by actual elapsed seconds since the last flow tick (5 s on the
  first).
- `orig` delta goes to lane `from|to` (requests), `reply` delta to
  `to|from` (responses). Parallel connections sum.
- Tuples missing this tick are deleted from `prevFlows`. Bytes a
  connection moved after the last tick and before it closed are lost.

**Stored.** Memory only. `Traffic{Pairs map["<fromRef>|<toRef>"]float64
(bytes/s), At}` behind a mutex, replaced whole each tick; `Snapshot()`
returns a copy. No table, no history. After each tick
`notifier.Flows()` pushes a bare "flows" ping to one global websocket
room `flows` (global because conntrack is host-wide and fanning out per
stack meant looking up every tile).

**How the graph read it.** Canvases joined room `flows`; on each ping they
re-fetched their own status endpoint. The handler called
`graph.RollupTraffic(sampler.Snapshot().Pairs, nodeOf)`: `nodeOf` renames
each ref to the card at that zoom (tile, env card, stack card; `MapTile`
claims both `app:` and `db:` spellings of a tile). Unmapped ends drop,
same-card pairs drop, parallel pairs sum, `bps <= 0` drops. JSON per edge:
`{"from": nodeID, "to": nodeID, "bps": float}` in a `"traffic"` array;
directional, bytes per second. The graph builder also added an edge of
kind `traffic` (teal, dashed, faint) for any pair with no declared edge in
either direction; the JS drew one lane per direction on existing edges,
offset 4 px, pulsed, labelled "→ 12.3 KB/s" (see graph-ref.md §1).

**Host prerequisites** (from host-setup.md and the code):
- `net.netfilter.nf_conntrack_acct=1`. Without it lines carry no `bytes=`,
  every pair is 0, nothing errors. Runtime `sysctl -w`, no reboot; persist
  via `/etc/sysctl.d/99-conntrack-acct.conf`. Only connections opened
  after enabling get counters; pooled long-lived connections stay 0 until
  they reconnect.
- The panel must see the **host** netns conntrack table (see Source file).
- Counters exist only while a connection is alive; short requests are
  bursty and a single read can miss them.
- Not in the source, verify on the VM: the kernel must expose
  `/proc/net/nf_conntrack` (`CONFIG_NF_CONNTRACK_PROCFS`, off on some
  distros; the fallback is netlink); container-to-container traffic on one
  bridge only hits conntrack with `br_netfilter` loaded and
  `net.bridge.bridge-nf-call-iptables=1` (Docker normally sets both);
  the file is root-readable only.

## 2. Cut: metrics-only

CPU %, memory, per-container rx/tx, host CPU/mem/disk, slice txn/s and
the `metrics` table ring buffer lived in the same `metrics.Sampler`
(`sample`, `sampleHost`, `sampleOffNode`, `sampleSlices`, 30 s tick) and in
`hostmetrics/host.go` (`ReadHost`, `Sample`, `hostNet`, `hostMemUsed`).
Belongs in later/metrics.

## 3. Kept code (pure)

```go
// flowFields pulls the nth (0=original, 1=reply) src/sport/dst/dport/bytes
// group out of one conntrack line.
func flowFields(line string, n int) (src, sport, dst, dport string, bytes uint64) {
	var srcs, sports, dsts, dports []string
	var bytesv []uint64
	for _, f := range strings.Fields(line) {
		switch {
		case strings.HasPrefix(f, "src="):
			srcs = append(srcs, f[4:])
		case strings.HasPrefix(f, "sport="):
			sports = append(sports, f[6:])
		case strings.HasPrefix(f, "dst="):
			dsts = append(dsts, f[4:])
		case strings.HasPrefix(f, "dport="):
			dports = append(dports, f[6:])
		case strings.HasPrefix(f, "bytes="):
			v, _ := strconv.ParseUint(f[6:], 10, 64)
			bytesv = append(bytesv, v)
		}
	}
	if n < len(srcs) && n < len(dsts) {
		src, dst = srcs[n], dsts[n]
	}
	if n < len(sports) {
		sport = sports[n]
	}
	if n < len(dports) {
		dport = dports[n]
	}
	if n < len(bytesv) {
		bytes = bytesv[n]
	}
	return
}

// sampleFlows aggregates per-connection byte deltas into pair rates.
// extract: dropped the cs []runtime.ManagedContainer argument and the label
// switch that built ipRef; ipRef (container IP -> tile id) is an argument now,
// built by the flow from docker inspect + the container -> tile map.
// extract: dropped os.ReadFile(hostmetrics.ProcNetPath("nf_conntrack")); the
// file contents are an argument (data), the caller reads the file.
func (s *Sampler) sampleFlows(ipRef map[string]string, data []byte, now time.Time) {
	// Rate over the actual elapsed time since the previous flow sample.
	interval := flowEvery.Seconds()
	if !s.lastFlowAt.IsZero() {
		if dt := now.Sub(s.lastFlowAt).Seconds(); dt > 0 {
			interval = dt
		}
	}
	s.lastFlowAt = now
	pairBps := map[string]float64{}
	seen := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		src1, sport1, dst1, dport1, b1 := flowFields(line, 0)
		_, _, _, _, b2 := flowFields(line, 1)
		if src1 == "" {
			continue
		}
		from, okF := ipRef[src1]
		to, okT := ipRef[dst1]
		if !okF || !okT || from == to {
			continue
		}
		// full 4-tuple: the source port is what separates concurrent
		// connections between the same two containers
		tuple := src1 + ":" + sport1 + ">" + dst1 + ":" + dport1
		cur := [2]uint64{b1, b2}
		prev, existed := s.prevFlows[tuple]
		s.prevFlows[tuple] = cur
		seen[tuple] = true
		var dOrig, dReply uint64
		if !existed {
			dOrig, dReply = b1, b2
		} else if b1 >= prev[0] && b2 >= prev[1] {
			dOrig, dReply = b1-prev[0], b2-prev[1]
		}
		// requests ride from->to, responses ride the return lane
		pairBps[from+"|"+to] += float64(dOrig) / interval
		pairBps[to+"|"+from] += float64(dReply) / interval
	}
	for k := range s.prevFlows {
		if !seen[k] {
			delete(s.prevFlows, k)
		}
	}
	s.trafMu.Lock()
	s.traffic = Traffic{Pairs: pairBps, At: now}
	s.trafMu.Unlock()
	// extract: dropped s.notifier.Flows() (was in RunFlows); the scheduler
	// entry publishes the SSE event after this returns.
}
```

`RollupTraffic` (graph.go, 20 lines, pure) is canvas-only: Later with the
canvas, not copied.

## 4. Rewrite mapping

- **`leaf/traffic`**: owns `prevFlows`, `lastFlowAt` and the latest
  `Pairs` snapshot in a memory map behind a mutex. No table (the old code
  had none and nobody asked for history). No store access, no Docker
  call. Entry point `Sample(ipTile map[ip]tileID, conntrack []byte, now)`
  plus `Snapshot()`. Pair key becomes `"<fromTileID>|<toTileID>"`; the
  `app:`/`db:` split goes, a tile is a tile.
- **Scheduler entry, every 5 s**: (1) Docker wrapper: list running stackr
  containers and `docker inspect` each for its IP per network
  (`NetworkSettings.Networks[*].IPAddress`); (2) the flow maps container
  id → tile id (facts it already has) and builds `ipTile`; (3) read
  `/proc/1/net/nf_conntrack`: the panel runs host-network (step 5), so
  both `/proc/net` and `/proc/1/net` are the host netns and no
  `HOST_PROC` mount is needed; (4) `leaf/traffic.Sample`; (5) publish.
- **One SSE stream** for the graph: an event per tick carrying the rolled-
  up `{from,to,bps}` list (or a ping the page swaps from), replacing the
  global websocket room. Only consumer is the canvas, which is Later.
- **Proxy lane**: old code tagged the Traefik container `proxy`. Caddy is
  new; if Caddy also runs host-network it has no container IP, and proxy →
  tile flows show the bridge gateway IP as source. Map the gateway IP to
  a `proxy` pseudo-tile, or drop ingress lanes.
- **Installer**: set `nf_conntrack_acct=1` and write the sysctl.d file,
  then check `grep -c bytes= /proc/net/nf_conntrack`.

Size: source 441 + 168 + 207 + ~95 (graph.go excerpts) + ~15 (notify.go) ≈ 926 lines read; extract 218 lines (kept Go 91)

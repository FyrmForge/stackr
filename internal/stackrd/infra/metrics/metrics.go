// Package metrics samples container CPU/memory into a SQLite ring buffer
// for the per-service graphs, plus host CPU/mem/disk under "server:local"
// refs for the servers screen. No Prometheus, on purpose.
package metrics

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/FyrmForge/stackr/internal/stackrd/config/settings"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/cluster"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/hostmetrics"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
	"github.com/FyrmForge/stackr/internal/stackrd/service/notify"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

const sampleEvery = 30 * time.Second
const flowEvery = 5 * time.Second

// Rows is the owner of the metrics table and of a tile's status column.
// Declared here rather than imported because service/ is built on top of this
// package; service.Rows satisfies it.
type Rows interface {
	RecordSample(ctx context.Context, m *repo.Metric) error
	PruneSamples(ctx context.Context, before time.Time) error
	SetTileStatus(ctx context.Context, tileID, status string) error
}

type Sampler struct {
	store    repo.Store
	clus     *cluster.Cluster // every docker call; the local walk and the off-node pass both go through it
	notifier *notify.Notifier
	// rows owns the two tables this package writes: the metric points, and
	// the tile status the reconciler corrects.
	rows Rows

	// previous cumulative network counters for rate deltas
	prevNet map[string]netCounters // per container ref
	// previous whole-machine reading, for the host graph rates
	prevHost hostmetrics.HostCounters

	// conntrack flow tracking (tile-to-tile traffic for the graph overlay)
	prevFlows  map[string][2]uint64 // per-connection tuple -> cumulative [orig, reply] bytes
	lastFlowAt time.Time
	trafMu     sync.Mutex
	traffic    Traffic

	// per-logical-database counters, read from the engine
	prevSlice  map[string]sliceCounters // resource id -> previous cumulative reading
	sliceMu    sync.Mutex
	sliceStats map[string]SliceStat // resource id -> latest derived stat
	// readSlices asks one instance for its logical databases' counters.
	//
	// A function rather than an import of managedtiles. This package is the
	// panel's sampler and the node agent's alike, and the managedtiles edge is
	// the one that closed the cycle keeping managedtiles from importing the
	// node-aware runtime (docs/plans/35-cluster.md). main supplies it; a
	// sampler built without one simply reports no slice stats.
	readSlices SliceReader
}

// SliceRead is one logical database's cumulative counters at a point in time,
// as the engine reports them. It mirrors managedtiles.SliceStat, which is what
// the wired reader actually returns.
type SliceRead struct {
	Xacts uint64
	Size  int64
}

// SliceReader reads an instance's per-database counters. It reports (nil, nil)
// for an engine that does not measure its slices.
type SliceReader func(ctx context.Context, instance *repo.Tile, dbNames []string) (map[string]SliceRead, error)

// WithSliceReader wires the per-database counter reader.
func (s *Sampler) WithSliceReader(r SliceReader) *Sampler { s.readSlices = r; return s }

// sliceCounters is one resource's previous cumulative reading, for rates.
type sliceCounters struct {
	xacts uint64
	at    time.Time
}

// SliceStat is what a slice card shows: how busy the database is, and how big.
type SliceStat struct {
	TxnRate float64 // transactions/second since the previous sample
	Size    int64   // bytes on disk
}

// SliceStats returns the latest per-resource database stats (copy).
func (s *Sampler) SliceStats() map[string]SliceStat {
	s.sliceMu.Lock()
	defer s.sliceMu.Unlock()
	out := make(map[string]SliceStat, len(s.sliceStats))
	for k, v := range s.sliceStats {
		out[k] = v
	}
	return out
}

// Traffic is the latest observed inter-node flow snapshot. Pair keys are
// "<fromRef>|<toRef>" using graph refs ("app:<id>", "db:<id>", "proxy").
type Traffic struct {
	Pairs map[string]float64 // bytes/second, directional
	At    time.Time
}

// Snapshot returns the latest flow rates (copy).
func (s *Sampler) Snapshot() Traffic {
	s.trafMu.Lock()
	defer s.trafMu.Unlock()
	out := Traffic{Pairs: make(map[string]float64, len(s.traffic.Pairs)), At: s.traffic.At}
	for k, v := range s.traffic.Pairs {
		out.Pairs[k] = v
	}
	return out
}

type netCounters struct {
	rx, tx uint64
	at     time.Time
}

// rates turns two cumulative readings into bytes/second (0 on first sample
// or counter reset).
func (prev netCounters) rates(cur netCounters) (rxBps, txBps float64) {
	dt := cur.at.Sub(prev.at).Seconds()
	if prev.at.IsZero() || dt <= 0 || cur.rx < prev.rx || cur.tx < prev.tx {
		return 0, 0
	}
	return float64(cur.rx-prev.rx) / dt, float64(cur.tx-prev.tx) / dt
}

func NewSampler(store repo.Store, rows Rows, clus *cluster.Cluster, notifier *notify.Notifier) *Sampler {
	return &Sampler{store: store, clus: clus, notifier: notifier, rows: rows, prevNet: map[string]netCounters{},
		prevFlows: map[string][2]uint64{}, prevSlice: map[string]sliceCounters{}, sliceStats: map[string]SliceStat{}}
}

// Run blocks; call in a goroutine.
func (s *Sampler) Run(ctx context.Context) {
	t := time.NewTicker(sampleEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sample(ctx)
		}
	}
}

// RunFlows samples inter-tile traffic on a fast tick so the graph overlay
// tracks load changes in seconds, decoupled from the 30s metrics cadence.
// Blocks; call in a goroutine.
func (s *Sampler) RunFlows(ctx context.Context) {
	t := time.NewTicker(flowEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if cs, err := s.clus.ListAll(ctx, s.clus.Self(ctx)); err == nil {
				s.sampleFlows(cs, time.Now().UTC())
				s.notifier.Flows() // canvases refresh their lanes off this tick
			}
		}
	}
}

func (s *Sampler) sample(ctx context.Context) {
	cs, err := s.clus.ListAll(ctx, s.clus.Self(ctx))
	if err != nil {
		return
	}
	now := time.Now().UTC()
	s.reconcile(ctx, cs)
	seen := map[string]bool{}
	for _, c := range cs {
		if c.State != "running" {
			continue
		}
		var ref string
		if id := c.Labels[runtime.LabelApp]; id != "" {
			ref = "app:" + id
			seen[id] = true
		} else if id := c.Labels[runtime.LabelDB]; id != "" {
			ref = "db:" + id
			seen[id] = true
		} else {
			continue
		}
		st, err := s.clus.Stats(ctx, s.clus.Self(ctx), c.ID)
		if err != nil {
			continue
		}
		cur := netCounters{rx: st.RxBytes, tx: st.TxBytes, at: now}
		rx, tx := s.prevNet[ref].rates(cur)
		s.prevNet[ref] = cur
		_ = s.rows.RecordSample(ctx, &repo.Metric{Ref: ref, TS: now, CPUPct: st.CPUPct,
			MemBytes: int64(st.MemBytes), RxBps: rx, TxBps: tx})
	}
	s.sampleOffNode(ctx, seen, now)
	s.sampleHost(ctx, now)
	s.sampleSlices(ctx, cs, now)
	retention := time.Duration(settings.ForServer(ctx, s.store).MetricRetentionHours) * time.Hour
	_ = s.rows.PruneSamples(ctx, now.Add(-retention))
	s.notifier.Server()
}

// sampleHost records the machine's CPU%, memory use, and root-disk use.
// Linux-only (/proc + statfs); on other platforms it just records nothing.
//
// The reading itself is in host.go, shared with the node agent, which takes
// the same one on a worker and posts it here.
func (s *Sampler) sampleHost(ctx context.Context, now time.Time) {
	cur := hostmetrics.ReadHost(now)
	sample, ok := cur.Sample(s.prevHost)
	s.prevHost = cur
	if !ok {
		return
	}
	StoreHostSample(ctx, s.rows, "server:local", now, sample)
}

// StoreHostSample writes one host reading under a server ref. The manager
// samples itself through sampleHost; a worker's arrives from its agent and
// lands here too, so both nodes' graphs read the same rows.
func StoreHostSample(ctx context.Context, rows Rows, ref string, now time.Time, s hostmetrics.HostSample) {
	_ = rows.RecordSample(ctx, &repo.Metric{Ref: ref, TS: now, CPUPct: s.CPUPct,
		MemBytes: s.MemBytes, RxBps: s.RxBps, TxBps: s.TxBps})
	if s.DiskUsed > 0 {
		_ = rows.RecordSample(ctx, &repo.Metric{Ref: ref + ":disk", TS: now, MemBytes: s.DiskUsed})
	}
}

// sampleSlices asks each running postgres instance how busy its logical
// databases are. One psql exec per instance per tick, not per database.
//
// postgres only, and kept in memory like the traffic snapshot,
// no migration until someone wants history. Other engines report activity
// differently (mysql via information_schema, s3 via its own API), so each
// earns its own branch when it lands.
func (s *Sampler) sampleSlices(ctx context.Context, cs []runtime.ManagedContainer, now time.Time) {
	if s.readSlices == nil {
		return
	}
	stats := map[string]SliceStat{}
	for _, c := range cs {
		id := c.Labels[runtime.LabelDB]
		if id == "" || c.State != "running" {
			continue
		}
		inst, err := s.store.GetTile(ctx, id)
		if err != nil || inst == nil {
			continue
		}
		resources, err := s.store.ListResourcesByProvider(ctx, inst.ID)
		if err != nil || len(resources) == 0 {
			continue
		}
		names := make([]string, 0, len(resources))
		for _, r := range resources {
			names = append(names, r.Name)
		}
		read, err := s.readSlices(ctx, inst, names)
		if err != nil || len(read) == 0 {
			continue // instance busy, down, or an engine that does not count
		}
		for _, r := range resources {
			cur, ok := read[r.Name]
			if !ok {
				continue
			}
			stat := SliceStat{Size: cur.Size}
			// A counter that went backwards means pg_stat_reset or a restart,
			// not negative work, report no rate for this tick rather than a
			// spike or a negative.
			if prev, seen := s.prevSlice[r.ID]; seen && cur.Xacts >= prev.xacts {
				if dt := now.Sub(prev.at).Seconds(); dt > 0 {
					stat.TxnRate = float64(cur.Xacts-prev.xacts) / dt
				}
			}
			s.prevSlice[r.ID] = sliceCounters{xacts: cur.Xacts, at: now}
			stats[r.ID] = stat
		}
	}
	s.sliceMu.Lock()
	s.sliceStats = stats
	s.sliceMu.Unlock()
}

// sampleFlows reads the host conntrack table and aggregates per-connection
// byte deltas into node-pair rates for the graph traffic overlay. Requires
// net.netfilter.nf_conntrack_acct=1 on the host, without it conntrack rows
// carry no byte counters and this quietly reports nothing.
func (s *Sampler) sampleFlows(cs []runtime.ManagedContainer, now time.Time) {
	ipRef := map[string]string{}
	for _, c := range cs {
		var ref string
		switch {
		case c.Labels[runtime.LabelApp] != "":
			ref = "app:" + c.Labels[runtime.LabelApp]
		case c.Labels[runtime.LabelDB] != "":
			ref = "db:" + c.Labels[runtime.LabelDB]
		case c.Labels["stackr.traefik"] == "true":
			ref = "proxy"
		default:
			continue
		}
		for _, ip := range c.IPs {
			ipRef[ip] = ref
		}
	}
	data, err := os.ReadFile(hostmetrics.ProcNetPath("nf_conntrack"))
	if err != nil {
		return
	}
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
}

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

// sampleOffNode records the tiles whose container is on another node. The
// container list this sampler walks is the manager's own, so without this
// pass every worker tile's graphs are empty.
func (s *Sampler) sampleOffNode(ctx context.Context, seen map[string]bool, now time.Time) {
	tasks, err := s.clus.RunningTileTasks(ctx)
	if err != nil {
		return
	}
	tiles, err := s.store.ListTiles(ctx)
	if err != nil {
		return
	}
	kind := map[string]string{}
	for i := range tiles {
		kind[tiles[i].ID] = tiles[i].Kind
	}
	for id, task := range tasks {
		if seen[id] || task.ContainerID == "" {
			continue
		}
		ref := "app:" + id
		if kind[id] == "db" {
			ref = "db:" + id
		}
		st, err := s.clus.Stats(ctx, task.NodeID, task.ContainerID)
		if err != nil {
			continue
		}
		cur := netCounters{rx: st.RxBytes, tx: st.TxBytes, at: now}
		rx, tx := s.prevNet[ref].rates(cur)
		s.prevNet[ref] = cur
		_ = s.rows.RecordSample(ctx, &repo.Metric{Ref: ref, TS: now, CPUPct: st.CPUPct,
			MemBytes: int64(st.MemBytes), RxBps: rx, TxBps: tx})
	}
}

package service

import (
	"context"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/metrics"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/worker"
)

// MetricsEvery is the metrics sample tick.
const MetricsEvery = 30 * time.Second

// MetricsKeep is what the in-memory store keeps; reads past it go to
// stackr-observe when it is on (metricsKeep).
const MetricsKeep = 24 * time.Hour

// metricsState is the last raw sample per node (counters become rates
// between two) and the replicas each tile had in it.
type metricsState struct {
	mu       sync.Mutex
	prev     map[string]worker.Metrics
	replicas map[string][]string // tile id -> container names, last sample
	// seq counts sampler rounds; last is the newest round's points, which
	// the panel's open graphs append (MetricsAfter).
	seq  int64
	last []metrics.Point
	// caddy is each ingress node's last Caddy sample, panel the panel's
	// HTTP totals (metrics_traffic.go).
	caddy map[string]caddySample
	panel panelSample
}

// Series keys. A link's is its node ids in order, so either end finds it.
func nodeKey(id string) string     { return "node:" + id }
func tileKey(id string) string     { return "tile:" + id }
func ctrKey(tile, n string) string { return "ctr:" + tile + "/" + n }

// LinkKey is the series key of the tunnel between two nodes.
func LinkKey(a, b string) string {
	if b < a {
		a, b = b, a
	}
	return "link:" + a + "|" + b
}

type metricsNode interface {
	Metrics(ctx context.Context) (worker.Metrics, error)
}

func (o *Orchestrator) metricsStore() metrics.Store {
	if o.cfg.Metrics == nil {
		o.cfg.Metrics = &metrics.Memory{}
	}
	return tieredStore{o, o.cfg.Metrics}
}

// metricsTick samples every node (8 at a time) and writes the rates, plus the
// mesh's last pings per node pair.
func (o *Orchestrator) metricsTick(ctx context.Context) error {
	ns, err := o.nodeLeaf().List(ctx)
	if err != nil {
		return err
	}
	var (
		mu  sync.Mutex
		pts  []metrics.Point
		pair = map[string][2]worker.Metrics{} // a node's last two samples, for its wg peers
		wg   sync.WaitGroup
	)
	sem := make(chan struct{}, 8)
	for _, n := range ns {
		var src metricsNode
		switch {
		case n.Role == node.Controller && o.localWorker != nil:
			src = o.localWorker
		case n.Role != node.Controller && n.Status == node.StatusOnline:
			if c, err := o.nodeExtras(n.ID); err == nil {
				src = c
			}
		}
		if src == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			cur, err := src.Metrics(cctx)
			if err != nil {
				return
			}
			p, prev, had := o.takeSample(n.ID, cur)
			mu.Lock()
			pts = append(pts, p...)
			if had {
				pair[n.ID] = [2]worker.Metrics{prev, cur}
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	now := time.Now()
	pts = append(pts, o.linkPoints(ns, now)...)
	pts = append(pts, wgPoints(pair, o.meshPeers, now)...)
	pts = append(pts, o.trafficPoints(ctx, now)...)
	pts = append(pts, o.panelPoints(now)...)
	if len(pts) == 0 {
		return nil
	}
	if err := o.metricsStore().Write(ctx, pts); err != nil {
		return err
	}
	o.metrics.mu.Lock()
	o.metrics.seq, o.metrics.last = o.metrics.seq+1, pts
	o.metrics.mu.Unlock()
	return nil
}

// MetricsScope is the series one open graph follows: a node's, a link's or
// a tile's (with its replicas). Make it with NodeScope, LinkScope or
// TileScope, which check that the thing exists.
type MetricsScope struct{ key, tile string }

func (o *Orchestrator) NodeScope(ctx context.Context, ref string) (MetricsScope, error) {
	n, err := o.nodeByRef(ctx, ref)
	if err != nil {
		return MetricsScope{}, err
	}
	return MetricsScope{key: nodeKey(n.ID)}, nil
}

func (o *Orchestrator) LinkScope(ctx context.Context, refA, refB string) (MetricsScope, error) {
	a, err := o.nodeByRef(ctx, refA)
	if err != nil {
		return MetricsScope{}, err
	}
	b, err := o.nodeByRef(ctx, refB)
	if err != nil {
		return MetricsScope{}, err
	}
	return MetricsScope{key: LinkKey(a.ID, b.ID)}, nil
}

func (o *Orchestrator) TileScope(ctx context.Context, tileID string) (MetricsScope, error) {
	if _, err := o.store.Tiles.Get(ctx, tileID); err != nil {
		return MetricsScope{}, err
	}
	return MetricsScope{key: tileKey(tileID), tile: tileID}, nil
}

// MetricsSeq is the newest sampler round's number.
func (o *Orchestrator) MetricsSeq() int64 {
	o.metrics.mu.Lock()
	defer o.metrics.mu.Unlock()
	return o.metrics.seq
}

// MetricsAfter is the newest round's points in scope, shaped like the
// read (a Series, or a TileMetricsOut for a tile), when that round is
// later than seq; nil when there is none or it has nothing in scope. The
// second value is the newest round's number.
func (o *Orchestrator) MetricsAfter(s MetricsScope, seq int64) (any, int64) {
	o.metrics.mu.Lock()
	cur, pts := o.metrics.seq, o.metrics.last
	o.metrics.mu.Unlock()
	if cur <= seq {
		return nil, cur
	}
	own, reps := Series{}, map[string]Series{}
	ctr := "ctr:" + s.tile + "/"
	for _, p := range pts {
		x := metrics.Sample{T: p.T, V: p.V}
		switch {
		case p.Key == s.key:
			own[p.Name] = append(own[p.Name], x)
		case s.tile != "" && strings.HasPrefix(p.Key, ctr):
			r := ReplicaLabel(s.tile, strings.TrimPrefix(p.Key, ctr))
			if reps[r] == nil {
				reps[r] = Series{}
			}
			reps[r][p.Name] = append(reps[r][p.Name], x)
		}
	}
	switch {
	case s.tile != "" && len(own)+len(reps) > 0:
		return TileMetricsOut{Tile: own, Replicas: reps}, cur
	case s.tile == "" && len(own) > 0:
		return own, cur
	}
	return nil, cur
}

// takeSample keeps cur as the node's last sample and returns the points
// between it and the one before, and that one (had: there was one).
func (o *Orchestrator) takeSample(nodeID string, cur worker.Metrics) ([]metrics.Point, worker.Metrics, bool) {
	s := &o.metrics
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.prev == nil {
		s.prev, s.replicas = map[string]worker.Metrics{}, map[string][]string{}
	}
	prev, had := s.prev[nodeID]
	s.prev[nodeID] = cur
	for _, c := range cur.Containers {
		if !skipCtr(c.Role) && !slices.Contains(s.replicas[c.Tile], c.Name) {
			s.replicas[c.Tile] = append(s.replicas[c.Tile], c.Name)
		}
	}
	if !had {
		return nil, prev, false
	}
	return Rates(nodeID, prev, cur), prev, true
}

// Rates turns two raw samples of one node into points: host CPU %, memory and
// disk; per replica CPU % of one core, memory, IO and network bytes/s; per
// tile the same summed. Each replica has its own network namespace on the env
// network (the pause only holds the VIP the kernel forwards from), so pause
// containers are left out. A counter that went backwards (a restart) yields
// nothing.
func Rates(nodeID string, prev, cur worker.Metrics) []metrics.Point {
	dt := cur.At.Sub(prev.At).Seconds()
	if dt <= 0 {
		return nil
	}
	t := cur.At
	var out []metrics.Point
	add := func(key, name string, v float64) { out = append(out, metrics.Point{Key: key, Name: name, T: t, V: v}) }
	rate := func(a, b uint64) (float64, bool) {
		if b < a {
			return 0, false
		}
		return float64(b-a) / dt, true
	}
	h, ph := cur.Host, prev.Host
	nk := nodeKey(nodeID)
	if tot := h.CPUTotal - ph.CPUTotal; h.CPUTotal > ph.CPUTotal && h.CPUIdle >= ph.CPUIdle {
		add(nk, "cpu_pct", 100*(1-float64(h.CPUIdle-ph.CPUIdle)/float64(tot)))
	}
	add(nk, "mem_used", float64(h.MemTotal-min(h.MemAvail, h.MemTotal)))
	add(nk, "mem_total", float64(h.MemTotal))
	if h.DiskTotal > 0 {
		add(nk, "disk_used", float64(h.DiskTotal-min(h.DiskFree, h.DiskTotal)))
		add(nk, "disk_total", float64(h.DiskTotal))
	}
	before := map[string]worker.ContainerSample{}
	for _, c := range prev.Containers {
		before[c.Tile+"/"+c.Name] = c
	}
	names := []string{"cpu_pct", "mem", "io_read_bps", "io_write_bps", "net_rx_bps", "net_tx_bps"}
	tiles := map[string]*[6]float64{}
	var order []string
	for _, c := range cur.Containers {
		p, ok := before[c.Tile+"/"+c.Name]
		if !ok || skipCtr(c.Role) {
			continue
		}
		cpu, ok1 := rate(p.CPUUsec, c.CPUUsec)
		ior, ok2 := rate(p.IORead, c.IORead)
		iow, ok3 := rate(p.IOWrite, c.IOWrite)
		rx, ok4 := rate(p.NetRx, c.NetRx)
		tx, ok5 := rate(p.NetTx, c.NetTx)
		if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 {
			continue
		}
		vs := [6]float64{cpu / 1e4, float64(c.Mem), ior, iow, rx, tx} // usec/s -> % of one core
		k := ctrKey(c.Tile, c.Name)
		a := tiles[c.Tile]
		if a == nil {
			a = &[6]float64{}
			tiles[c.Tile] = a
			order = append(order, c.Tile)
		}
		for i, n := range names {
			add(k, n, vs[i])
			a[i] += vs[i]
		}
	}
	for _, id := range order {
		for i, n := range names {
			add(tileKey(id), n, tiles[id][i])
		}
	}
	return out
}

// linkPoints are the last mesh pings of every node pair: round trip (either
// end's, the lower) and loss (the higher).
func (o *Orchestrator) linkPoints(ns []store.Node, t time.Time) []metrics.Point {
	if o.mesh == nil {
		return nil
	}
	var out []metrics.Point
	for i, a := range ns {
		for _, b := range ns[i+1:] {
			rtt, loss, ok := 0.0, 0.0, false
			for _, x := range [2][2]store.Node{{a, b}, {b, a}} {
				f, has := o.mesh.FactsOf(x[0].ID)
				if !has {
					continue
				}
				p, has := f.Ping[transitOf(x[1]).String()]
				if !has {
					continue
				}
				loss = max(loss, p.Loss)
				if p.RTTms > 0 && (!ok || p.RTTms < rtt) {
					rtt, ok = p.RTTms, true
				}
			}
			k := LinkKey(a.ID, b.ID)
			if ok {
				out = append(out, metrics.Point{Key: k, Name: "rtt_ms", T: t, V: rtt})
			}
			out = append(out, metrics.Point{Key: k, Name: "loss", T: t, V: loss})
		}
	}
	return out
}

// MetricsIn is a read: the last Range, averaged per Step (0 = Range/120).
type MetricsIn struct {
	Range time.Duration
	Step  time.Duration
}

func (in MetricsIn) window(keep time.Duration) (time.Time, time.Time, time.Duration) {
	r := in.Range
	if r <= 0 {
		r = time.Hour
	}
	r = min(r, keep)
	step := in.Step
	if step <= 0 {
		step = max(r/120, MetricsEvery)
	}
	to := time.Now()
	return to.Add(-r), to, step
}

// Series is one thing's measures by name.
type Series map[string][]metrics.Sample

// NodeMetrics is a node's host series (admin).
func (o *Orchestrator) NodeMetrics(ctx context.Context, ref string, in MetricsIn) (Series, error) {
	n, err := o.nodeByRef(ctx, ref)
	if err != nil {
		return nil, err
	}
	from, to, step := in.window(o.metricsKeep(ctx))
	return o.metricsStore().Query(ctx, nodeKey(n.ID), from, to, step)
}

// LinkMetrics is the tunnel between two nodes: rtt_ms, loss, rx_bps,
// tx_bps and handshake_age_s (admin).
func (o *Orchestrator) LinkMetrics(ctx context.Context, refA, refB string, in MetricsIn) (Series, error) {
	a, err := o.nodeByRef(ctx, refA)
	if err != nil {
		return nil, err
	}
	b, err := o.nodeByRef(ctx, refB)
	if err != nil {
		return nil, err
	}
	if a.ID == b.ID {
		return nil, errs.Invalidf("node", "a link joins two different nodes")
	}
	from, to, step := in.window(o.metricsKeep(ctx))
	return o.metricsStore().Query(ctx, LinkKey(a.ID, b.ID), from, to, step)
}

// TileMetricsOut is a tile's summed series and each replica's.
type TileMetricsOut struct {
	Tile     Series            `json:"tile"`
	Replicas map[string]Series `json:"replicas"`
}

// TileMetrics is the tile's series (the caller's tile.read gate).
func (o *Orchestrator) TileMetrics(ctx context.Context, tileID string, in MetricsIn) (TileMetricsOut, error) {
	if _, err := o.store.Tiles.Get(ctx, tileID); err != nil {
		return TileMetricsOut{}, err
	}
	from, to, step := in.window(o.metricsKeep(ctx))
	st := o.metricsStore()
	t, err := st.Query(ctx, tileKey(tileID), from, to, step)
	if err != nil {
		return TileMetricsOut{}, err
	}
	out := TileMetricsOut{Tile: t, Replicas: map[string]Series{}}
	o.metrics.mu.Lock()
	names := append([]string(nil), o.metrics.replicas[tileID]...)
	o.metrics.mu.Unlock()
	sort.Strings(names)
	for _, n := range names {
		s, err := st.Query(ctx, ctrKey(tileID, n), from, to, step)
		if err != nil {
			return TileMetricsOut{}, err
		}
		for _, xs := range s {
			if len(xs) > 0 { // a replica gone longer than the range is left out
				out.Replicas[ReplicaLabel(tileID, n)] = s
				break
			}
		}
	}
	return out, nil
}

// ReplicaLabel is a replica's container name without the tile's prefix
// ("stackr-hello-b3d7aa59-7d8a55-1" -> "7d8a55-1": its deploy and index).
func ReplicaLabel(tileID, name string) string {
	name = strings.TrimPrefix(name, "/")
	if len(tileID) >= 8 {
		if _, after, ok := strings.Cut(name, "-"+tileID[:8]+"-"); ok && after != "" {
			return after
		}
	}
	return name
}

// skipCtr is a container the graphs leave out: a pause (its net is the
// replicas') or a cron/function run, whose fresh name per run would pile up
// as replicas; a run's numbers belong to the run, not the tile's graphs.
func skipCtr(role string) bool { return role == "pause" || role == "run" }

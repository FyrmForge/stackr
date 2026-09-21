package service

import (
	"context"
	"time"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/cluster"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/envnet"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// TileTelemetryService answers "what is this tile saying and doing" — logs
// and metric windows. It writes nothing, which is why it is separate from
// both TileService and TileLifecycleService.
type TileTelemetryService struct {
	store repo.Store
	clus  *cluster.Cluster
}

// --- the metric rows the sampler below writes ---
//
// A metric point carries no rule and the sampler is its only writer. What
// moves is ownership: the table has one owner, which is where a retention
// rule or a per-tile sampling rule would go if either is ever wanted.

// RecordSample stores one metric point.
func (s *TileTelemetryService) RecordSample(ctx context.Context, m *repo.Metric) error {
	return s.store.InsertMetric(ctx, m)
}

// Prune drops metric points older than the retention window.
func (s *TileTelemetryService) Prune(ctx context.Context, before time.Time) error {
	return s.store.PruneMetrics(ctx, before)
}

func NewTileTelemetryService(store repo.Store, clus *cluster.Cluster) *TileTelemetryService {
	return &TileTelemetryService{store: store, clus: clus}
}

// LogSource says where a tile's log lines come from. Which of the two applies
// is a rule about how the tile is deployed, and it was written out twice —
// once in the panel's SSE handler and once in the API's, with the fallbacks
// in a slightly different order.
type LogSource struct {
	// Service is the swarm service name when the tile has one. Service logs
	// cover every replica on every node, collected through the manager.
	Service string
	// Container is one container id on the manager's own box, used when
	// there is no service: a tile deployed before the swarm move, or a cron,
	// which has no long-lived service at all.
	Container string
}

// Found reports whether there is anything to read.
func (l LogSource) Found() bool { return l.Service != "" || l.Container != "" }

// Logs resolves where to read a tile's logs from, preferring the service.
func (s *TileTelemetryService) Logs(ctx context.Context, t *repo.Tile) LogSource {
	if t == nil {
		return LogSource{}
	}
	if name := envnet.ServiceFor(ctx, s.store, t); name != "" {
		return LogSource{Service: name}
	}
	return LogSource{Container: s.Container(ctx, t)}
}

// Container is the manager's own container for a tile, if one is there.
// Pre-swarm deployments only: anything deployed since the swarm move answers
// from its service, which swarm collects across every node. Exported because
// the API falls back to it when a service exists but will not hand over its
// logs.
func (s *TileTelemetryService) Container(ctx context.Context, t *repo.Tile) string {
	if t == nil {
		return ""
	}
	cs, _ := s.clus.ListByLabel(ctx, runtime.LabelApp, t.ID)
	if len(cs) == 0 {
		return ""
	}
	return cs[0].ID
}

// MetricRange maps a picker key to its window, and returns the canonical key
// alongside it so a caller can echo back what it actually used. Unknown keys
// fall back to an hour.
//
// One parser: the panel's picker and the API's `?range=` used to be two
// switches over the same three strings, so adding a window to one of them
// silently left the other answering 1h.
func MetricRange(key string) (string, time.Duration) {
	switch key {
	case "6h":
		return "6h", 6 * time.Hour
	case "24h":
		return "24h", 24 * time.Hour
	}
	return "1h", time.Hour
}

// TimePoint is one sample on a metric chart. It lives here rather than with
// the chart because the bucketing that produces it does: a template that asks
// the store for samples and averages them itself is a query nothing else can
// make the same way, which is what point 19 is about.
type TimePoint struct {
	T time.Time
	V float64
}

// maxChartPoints keeps a 24h window of 30s samples light to render.
const maxChartPoints = 240

// Samples is a tile's metric window at full resolution, which is what an API
// caller wants: the panel buckets because a chart has 640 pixels, and a client
// plotting its own does not.
func (s *TileTelemetryService) Samples(ctx context.Context, ref string, dur time.Duration) ([]repo.Metric, error) {
	return s.store.ListMetrics(ctx, ref, time.Now().Add(-dur))
}

// Points is a tile's metric window, bucket-averaged to at most
// maxChartPoints of each series.
//
// It was components.MetricPoints, in a .templ file, taking repo.Store — the
// view layer reaching past the handler into the database. The averaging is
// the same; what changed is who may ask for it.
func (s *TileTelemetryService) Points(ctx context.Context, ref string, dur time.Duration) (cpu, mem, rx, tx []TimePoint) {
	ms, _ := s.store.ListMetrics(ctx, ref, time.Now().Add(-dur))
	step := len(ms)/maxChartPoints + 1
	for i := 0; i < len(ms); i += step {
		end := min(i+step, len(ms))
		var c, m, r, t float64
		for _, x := range ms[i:end] {
			c += x.CPUPct
			m += float64(x.MemBytes) / (1024 * 1024)
			r += x.RxBps / 1024
			t += x.TxBps / 1024
		}
		n := float64(end - i)
		cpu = append(cpu, TimePoint{T: ms[i].TS, V: c / n})
		mem = append(mem, TimePoint{T: ms[i].TS, V: m / n})
		rx = append(rx, TimePoint{T: ms[i].TS, V: r / n})
		tx = append(tx, TimePoint{T: ms[i].TS, V: t / n})
	}
	return
}

// TileRef is a tile's key in the two ref-keyed tables, metrics and cron_runs.
// It is one prefix for both, and nine places rebuilt the string by hand — so
// a tile whose ref scheme ever changes would have gone stale in eight of them.
func TileRef(tileID string) string { return "app:" + tileID }

// SamplesSince is a metric window with an explicit start, for the callers
// that already have one: the server page's host chart and the canvas's
// per-tile sparkline sweep, both of which pick the window once and then read
// many refs against it.
func (s *TileTelemetryService) SamplesSince(ctx context.Context, ref string, since time.Time) ([]repo.Metric, error) {
	return s.store.ListMetrics(ctx, ref, since)
}

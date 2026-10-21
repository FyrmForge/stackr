package service

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/metrics"
)

// Observability: the optional stackr-observe container (VictoriaMetrics)
// keeps metrics history past the in-memory 24 h. The sampler writes to
// memory always and to it when on; reads past 24 h go to it.

type observeState struct {
	on      atomic.Bool
	down    atomic.Bool // the last write failed (logged once per change)
	mu      sync.Mutex  // one ensure at a time
	lastErr atomic.Pointer[string]
	vm      *metrics.Victoria
	vmOnce  sync.Once
	vl      *metrics.Victoria // VictoriaLogs, data size only until the log store lands
	vlOnce  sync.Once
	labels  sync.Map // tile id -> its labels
}

// ObserveOn says whether metrics history is on (observe_enabled).
func (o *Orchestrator) ObserveOn() bool { return o.observe.on.Load() }

type observeKnobs struct {
	on       bool
	days     int
	minFree  uint64 // bytes
	logs     bool   // logs are kept too (needs on)
	logsDays int
	logsMax  uint64 // bytes

	accessQuery, accessIP, accessHeaders bool // what access logs keep
}

func (o *Orchestrator) observeKnobs(ctx context.Context) (observeKnobs, error) {
	var k observeKnobs
	on, err := o.settings.Get(ctx, "observe_enabled")
	if err != nil {
		return k, err
	}
	k.on, _ = strconv.ParseBool(on)
	if k.days, err = o.settings.Int(ctx, "observe_metrics_retention"); err != nil {
		return k, err
	}
	gb, err := o.settings.Int(ctx, "observe_min_free")
	if err != nil {
		return k, err
	}
	k.minFree = uint64(gb) << 30
	lon, err := o.settings.Get(ctx, "observe_logs_enabled")
	if err != nil {
		return k, err
	}
	k.logs, _ = strconv.ParseBool(lon)
	k.logs = k.logs && k.on
	if k.logsDays, err = o.settings.Int(ctx, "observe_logs_retention"); err != nil {
		return k, err
	}
	gb, err = o.settings.Int(ctx, "observe_logs_max_size")
	k.logsMax = uint64(gb) << 30
	for key, p := range map[string]*bool{"observe_access_query": &k.accessQuery,
		"observe_access_ip": &k.accessIP, "observe_access_headers": &k.accessHeaders} {
		v, _ := o.settings.Get(ctx, key)
		*p, _ = strconv.ParseBool(v)
	}
	return k, err
}

// syncObserve brings stackr-observe in line with the settings: on runs it
// at this build's spec, off stops and removes it (the volume stays).
func (o *Orchestrator) syncObserve(ctx context.Context) {
	o.observe.mu.Lock()
	defer o.observe.mu.Unlock()
	k, err := o.observeKnobs(ctx)
	if err == nil {
		o.observe.on.Store(k.on)
		if k.on {
			err = o.upgrade.EnsureObserve(ctx, installspec.ObserveOpts{
				Retention: strconv.Itoa(k.days) + "d", MinFree: k.minFree,
				Logs: k.logs, LogsRetention: strconv.Itoa(k.logsDays) + "d", LogsMax: k.logsMax,
			})
		} else {
			err = o.upgrade.RemoveObserve(ctx)
			_ = o.store.Settings.Delete(ctx, observeAlertKey)
		}
	}
	msg := ""
	if err != nil {
		msg = err.Error()
		slog.Warn("observe: not ensured", "err", err)
	}
	o.observe.lastErr.Store(&msg)
}

// startSyncObserve runs syncObserve in the background on the controller:
// a first pull can take a while. Only a real host runs the container.
func (o *Orchestrator) startSyncObserve() {
	if o.localWorker == nil {
		return
	}
	o.detach(context.Background(), func(ctx context.Context) {
		cctx, err := o.withController(ctx)
		if err != nil {
			slog.Warn("observe: not ensured", "err", err)
			return
		}
		o.syncObserve(cctx)
	})
}

// observeClient is an HTTP client of one of stackr-observe's sockets.
func observeClient(sock string) *http.Client {
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}
}

// victoria is the client of stackr-observe's metrics socket.
func (o *Orchestrator) victoria() *metrics.Victoria {
	o.observe.vmOnce.Do(func() {
		o.observe.vm = &metrics.Victoria{
			URL:    "http://observe",
			Client: observeClient(installspec.ObserveMetricsSocket(o.cfg.DataDir)),
			Labels: o.seriesLabels,
		}
	})
	return o.observe.vm
}

// victoriaLogs is the client of stackr-observe's logs socket.
func (o *Orchestrator) victoriaLogs() *metrics.Victoria {
	o.observe.vlOnce.Do(func() {
		o.observe.vl = &metrics.Victoria{
			URL:        "http://observe",
			Client:     observeClient(installspec.ObserveLogsSocket(o.cfg.DataDir)),
			SizeMetric: "vl_data_size_bytes",
		}
	})
	return o.observe.vl
}

// seriesLabels are a series key's labels: the node, or the tile with its
// env, stack and org (ids, so a rename keeps the history together).
func (o *Orchestrator) seriesLabels(ctx context.Context, key string) map[string]string {
	kind, rest, _ := strings.Cut(key, ":")
	switch kind {
	case "node":
		return map[string]string{"node": rest}
	case "tile", "ctr":
		tile, ctr, _ := strings.Cut(rest, "/")
		m := map[string]string{"tile": tile}
		if ctr != "" {
			m["container"] = ctr
		}
		for k, v := range o.tileLabels(ctx, tile) {
			m[k] = v
		}
		return m
	}
	return nil
}

// tileLabels are a tile's env, stack and org ids, cached for the process.
// ponytail: never evicted; a deleted tile's entry stays until restart.
func (o *Orchestrator) tileLabels(ctx context.Context, id string) map[string]string {
	if v, ok := o.observe.labels.Load(id); ok {
		return v.(map[string]string)
	}
	t, err := o.store.Tiles.Get(ctx, id)
	if err != nil {
		return nil
	}
	m := map[string]string{"env": t.EnvironmentID, "stack": t.StackID}
	if st, err := o.store.Stacks.Get(ctx, t.StackID); err == nil {
		m["org"] = st.OrgID
	}
	o.observe.labels.Store(id, m)
	return m
}

// tieredStore is the metrics store the panel uses: memory always, and
// stackr-observe for writes and long reads when on.
type tieredStore struct {
	o   *Orchestrator
	mem metrics.Store
}

func (s tieredStore) Write(ctx context.Context, ps []metrics.Point) error {
	if err := s.mem.Write(ctx, ps); err != nil {
		return err
	}
	if !s.o.ObserveOn() {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// A history write that fails never fails the round: the live graphs
	// run from memory.
	err := s.o.victoria().Write(cctx, ps)
	if down := err != nil; s.o.observe.down.Swap(down) != down {
		if down {
			slog.Warn("observe: metrics write failed", "err", err)
		} else {
			slog.Info("observe: metrics writes back")
		}
	}
	return nil
}

func (s tieredStore) Query(ctx context.Context, key string, from, to time.Time, step time.Duration) (map[string][]metrics.Sample, error) {
	if s.o.ObserveOn() && to.Sub(from) > MetricsKeep {
		if out, err := s.o.victoria().Query(ctx, key, from, to, step); err == nil {
			return out, nil
		}
	}
	return s.mem.Query(ctx, key, from, to, step)
}

// metricsKeep is the longest read: the retention when observe is on, else
// the in-memory 24 h.
func (o *Orchestrator) metricsKeep(ctx context.Context) time.Duration {
	if o.ObserveOn() {
		if k, err := o.observeKnobs(ctx); err == nil && k.days > 0 {
			return time.Duration(k.days) * 24 * time.Hour
		}
	}
	return MetricsKeep
}

// ObserveStatus is the Observability tab: the knobs, the container and the
// disk. DataBytes and LogsBytes are the stores' own counts, 0 when one does
// not answer.
type ObserveStatus struct {
	Enabled       bool   `json:"enabled"`
	RetentionDays int    `json:"retention_days"`
	MinFreeBytes  uint64 `json:"min_free_bytes"`
	LogsEnabled   bool   `json:"logs_enabled"`
	LogsDays      int    `json:"logs_retention_days"`
	LogsMaxBytes  uint64 `json:"logs_max_bytes"`
	AccessQuery   bool   `json:"access_query"`   // access logs keep the query string
	AccessIP      bool   `json:"access_ip"`      // ... the client address
	AccessHeaders bool   `json:"access_headers"` // ... every request header
	Container     string `json:"container"`      // docker state, "" = none
	DataBytes     uint64 `json:"data_bytes"`
	LogsBytes     uint64 `json:"logs_bytes"`
	DiskFree      uint64 `json:"disk_free"`
	DiskTotal     uint64 `json:"disk_total"`
	Error         string `json:"error,omitempty"` // the last ensure's failure
}

// ObserveStatus reads the knobs, the container's state, its data size and
// the controller's free disk.
func (o *Orchestrator) ObserveStatus(ctx context.Context) (ObserveStatus, error) {
	k, err := o.observeKnobs(ctx)
	if err != nil {
		return ObserveStatus{}, err
	}
	s := ObserveStatus{Enabled: k.on, RetentionDays: k.days, MinFreeBytes: k.minFree,
		LogsEnabled: k.logs, LogsDays: k.logsDays, LogsMaxBytes: k.logsMax,
		AccessQuery: k.accessQuery, AccessIP: k.accessIP, AccessHeaders: k.accessHeaders}
	if p := o.observe.lastErr.Load(); p != nil {
		s.Error = *p
	}
	if n, err := o.nodeLeaf().Controller(ctx); err == nil {
		s.DiskFree, s.DiskTotal = o.diskOf(ctx, n.ID)
		if s.Container, err = o.upgrade.ObserveState(docker.WithNode(ctx, n.ID)); err != nil {
			s.Container = ""
		}
	}
	if s.Container == "running" {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		s.DataBytes, _ = o.victoria().DataSize(cctx)
		if k.logs {
			s.LogsBytes, _ = o.victoriaLogs().DataSize(cctx)
		}
	}
	return s, nil
}

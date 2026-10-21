package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"

	"github.com/FyrmForge/stackr/internal/selfmetrics"
	"github.com/FyrmForge/stackr/internal/service/internal/metrics"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/worker"
)

// PanelKey is the series key of the panel's own process.
const PanelKey = "panel"

// hostCount is one host's totals in one Caddy: requests, the 4xx and 5xx
// among them, and their summed seconds.
type hostCount struct{ n, e4, e5, sum float64 }

// caddySample is one ingress Caddy's per host totals, read at at.
type caddySample struct {
	at    time.Time
	hosts map[string]hostCount
}

// parseCaddy reads Caddy's /metrics text (per_host on) into per host totals
// of caddy_http_request_duration_seconds. Hosts are lower case without a
// port; Caddy's "_other" (hosts it routes none of) is left out.
func parseCaddy(r io.Reader) (map[string]hostCount, error) {
	p := expfmt.NewTextParser(model.UTF8Validation)
	fams, err := p.TextToMetricFamilies(r)
	if err != nil {
		return nil, err
	}
	out := map[string]hostCount{}
	f := fams["caddy_http_request_duration_seconds"]
	if f == nil {
		return out, nil
	}
	for _, m := range f.GetMetric() {
		host, code := "", ""
		for _, l := range m.GetLabel() {
			switch l.GetName() {
			case "host":
				host = strings.ToLower(l.GetValue())
			case "code":
				code = l.GetValue()
			}
		}
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if host == "" || host == "_other" {
			continue
		}
		h := m.GetHistogram()
		n := float64(h.GetSampleCount())
		c := out[host]
		c.n += n
		c.sum += h.GetSampleSum()
		switch {
		case strings.HasPrefix(code, "4"):
			c.e4 += n
		case strings.HasPrefix(code, "5"):
			c.e5 += n
		}
		out[host] = c
	}
	return out, nil
}

// tileOfHost maps a request host to the tile its domain rows name: the
// exact host, else the wildcard one level up. A host two tiles share (by
// path) maps to none.
// ponytail: Caddy counts per host, not per path, so a host split by path
// across tiles has no traffic graph; per route labels if that matters.
func tileOfHost(ds []store.Domain) func(string) string {
	m := map[string]string{}
	for _, d := range ds {
		h := strings.ToLower(d.Host)
		if t, ok := m[h]; ok && t != d.TileID {
			m[h] = ""
			continue
		}
		m[h] = d.TileID
	}
	return func(host string) string {
		if t, ok := m[host]; ok {
			return t
		}
		if _, rest, ok := strings.Cut(host, "."); ok {
			return m["*."+rest]
		}
		return ""
	}
}

// tileTraffic is one tile's round summed over ingress nodes: request, 4xx
// and 5xx rates, and the new requests and their seconds (for the average).
type tileTraffic struct{ req, e4, e5, n, sum float64 }

// addTraffic adds one Caddy's change between two samples to acc by tile. A
// host whose counters went backwards (a config load resets them) or that is
// new adds nothing this round.
// ponytail: a reset that overtook the old totals within one tick reads as
// a small rate, not a reset; rare and one point.
func addTraffic(acc map[string]*tileTraffic, prev, cur caddySample, tileOf func(string) string) {
	dt := cur.at.Sub(prev.at).Seconds()
	if dt <= 0 {
		return
	}
	for host, c := range cur.hosts {
		p, ok := prev.hosts[host]
		if !ok || c.n < p.n || c.e4 < p.e4 || c.e5 < p.e5 || c.sum < p.sum {
			continue
		}
		t := tileOf(host)
		if t == "" {
			continue
		}
		a := acc[t]
		if a == nil {
			a = &tileTraffic{}
			acc[t] = a
		}
		a.req += (c.n - p.n) / dt
		a.e4 += (c.e4 - p.e4) / dt
		a.e5 += (c.e5 - p.e5) / dt
		a.n += c.n - p.n
		a.sum += c.sum - p.sum
	}
}

// trafficOut is acc as points under each tile: req_s, 4xx_s, 5xx_s, and
// latency_ms (the average) when there were requests.
func trafficOut(acc map[string]*tileTraffic, t time.Time) []metrics.Point {
	var out []metrics.Point
	ids := make([]string, 0, len(acc))
	for id := range acc {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		a, k := acc[id], tileKey(id)
		out = append(out,
			metrics.Point{Key: k, Name: "req_s", T: t, V: a.req},
			metrics.Point{Key: k, Name: "4xx_s", T: t, V: a.e4},
			metrics.Point{Key: k, Name: "5xx_s", T: t, V: a.e5})
		if a.n > 0 {
			out = append(out, metrics.Point{Key: k, Name: "latency_ms", T: t, V: 1000 * a.sum / a.n})
		}
	}
	return out
}

// trafficPoints reads every ingress Caddy's /metrics (the controller's on
// its admin socket, a worker's through its /v1/caddy) and returns each
// tile's traffic since the last round.
func (o *Orchestrator) trafficPoints(ctx context.Context, t time.Time) []metrics.Point {
	ctl, err := o.controllerID(ctx)
	if err != nil {
		return nil
	}
	ids := append(o.ingressNodes(ctx), ctl)
	if di, err := o.DefaultIngress(ctx); err == nil {
		ids = append(ids, di)
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	var (
		mu  sync.Mutex
		got = map[string]caddySample{}
		wg  sync.WaitGroup
	)
	for _, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			hs, err := o.readCaddy(cctx, id == ctl, id)
			if err != nil {
				return
			}
			mu.Lock()
			got[id] = caddySample{at: time.Now(), hosts: hs}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(got) == 0 {
		return nil
	}
	ds, err := o.store.Domains.List(ctx)
	if err != nil {
		return nil
	}
	tileOf := tileOfHost(ds)
	acc := map[string]*tileTraffic{}
	s := &o.metrics
	s.mu.Lock()
	if s.caddy == nil {
		s.caddy = map[string]caddySample{}
	}
	for id, cur := range got {
		if prev, ok := s.caddy[id]; ok {
			addTraffic(acc, prev, cur, tileOf)
		}
		s.caddy[id] = cur
	}
	s.mu.Unlock()
	return trafficOut(acc, t)
}

// readCaddy is one ingress node's Caddy /metrics, parsed. A proxy without
// the metrics endpoint (one from before it) is an error, so no sample.
func (o *Orchestrator) readCaddy(ctx context.Context, controller bool, nodeID string) (map[string]hostCount, error) {
	if controller {
		if o.caddyMetrics == nil {
			return nil, fmt.Errorf("no proxy client")
		}
		b, err := o.caddyMetrics(ctx)
		if err != nil {
			return nil, err
		}
		return parseCaddy(bytes.NewReader(b))
	}
	w := o.caddyFor(nodeID)
	if w == nil {
		return nil, fmt.Errorf("node %s: no connection", nodeID)
	}
	resp, err := w.Caddy(ctx, http.MethodGet, "/metrics", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("caddy on node %s: %s", nodeID, resp.Status)
	}
	return parseCaddy(io.LimitReader(resp.Body, 16<<20))
}

// wgPoints are each tunnel's traffic and handshake age from its ends' wg
// counters (got: a node's previous and current sample): rx_bps and tx_bps
// as the lower node id sees them, or the other end's swapped when that one
// has none; handshake_age_s the fresher end's. peers is a node's mesh peers,
// node id to public key.
func wgPoints(got map[string][2]worker.Metrics, peers func(self string) map[string]string, t time.Time) []metrics.Point {
	var out []metrics.Point
	selves := make([]string, 0, len(got))
	for id := range got {
		selves = append(selves, id)
	}
	sort.Strings(selves)
	cache := map[string]map[string]string{}
	peersOf := func(self string) map[string]string {
		if cache[self] == nil {
			cache[self] = peers(self)
		}
		return cache[self]
	}
	keyOf := func(self, peer string) string { return peersOf(self)[peer] }
	rates := func(self, peer string) (rx, tx float64, ok bool) {
		s, has := got[self]
		k := keyOf(self, peer)
		p, ok1 := s[0].Peers[k]
		c, ok2 := s[1].Peers[k]
		dt := s[1].At.Sub(s[0].At).Seconds()
		if !has || k == "" || !ok1 || !ok2 || dt <= 0 || c.Rx < p.Rx || c.Tx < p.Tx {
			return 0, 0, false
		}
		return float64(c.Rx-p.Rx) / dt, float64(c.Tx-p.Tx) / dt, true
	}
	done := map[string]bool{}
	for _, a := range selves {
		for _, b := range slices.Sorted(maps.Keys(peersOf(a))) {
			k := LinkKey(a, b)
			if done[k] {
				continue
			}
			done[k] = true
			lo, hi := min(a, b), max(a, b)
			rx, tx, ok := rates(lo, hi)
			if !ok {
				tx, rx, ok = rates(hi, lo)
			}
			if ok {
				out = append(out, metrics.Point{Key: k, Name: "rx_bps", T: t, V: rx}, metrics.Point{Key: k, Name: "tx_bps", T: t, V: tx})
			}
			age, has := 0.0, false
			for _, self := range [2]string{lo, hi} {
				s, in := got[self]
				hs := s[1].Peers[keyOf(self, other(self, lo, hi))].Handshake
				if !in || hs <= 0 {
					continue
				}
				if x := max(s[1].At.Sub(time.Unix(hs, 0)).Seconds(), 0); !has || x < age {
					age, has = x, true
				}
			}
			if has {
				out = append(out, metrics.Point{Key: k, Name: "handshake_age_s", T: t, V: age})
			}
		}
	}
	return out
}

func other(self, a, b string) string {
	if self == a {
		return b
	}
	return a
}

// meshPeers is a node's mesh peers from its last pushed spec, node id to
// public key.
func (o *Orchestrator) meshPeers(self string) map[string]string {
	out := map[string]string{}
	if o.mesh == nil {
		return out
	}
	sp, ok := o.mesh.Spec(self)
	if !ok {
		return out
	}
	for _, p := range sp.Peers {
		if p.PublicKey != "" {
			out[p.NodeID] = p.PublicKey
		}
	}
	return out
}

// panelPoints are the panel process's own: goroutines, heap in use, and
// HTTP requests and 5xx per second since the last round.
func (o *Orchestrator) panelPoints(t time.Time) []metrics.Point {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	out := []metrics.Point{
		{Key: PanelKey, Name: "goroutines", T: t, V: float64(runtime.NumGoroutine())},
		{Key: PanelKey, Name: "heap", T: t, V: float64(ms.HeapAlloc)},
	}
	all, e5 := selfmetrics.HTTPTotals()
	s := &o.metrics
	s.mu.Lock()
	prev := s.panel
	s.panel = panelSample{at: t, all: all, e5: e5}
	s.mu.Unlock()
	if dt := t.Sub(prev.at).Seconds(); !prev.at.IsZero() && dt > 0 && all >= prev.all && e5 >= prev.e5 {
		out = append(out,
			metrics.Point{Key: PanelKey, Name: "http_req_s", T: t, V: float64(all-prev.all) / dt},
			metrics.Point{Key: PanelKey, Name: "http_5xx_s", T: t, V: float64(e5-prev.e5) / dt})
	}
	return out
}

// panelSample is the panel's HTTP totals at one round.
type panelSample struct {
	at      time.Time
	all, e5 uint64
}

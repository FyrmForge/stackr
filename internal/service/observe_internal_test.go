package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/metrics"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// With history on, writes reach both stores, a read past 24 h or from
// before memory began (a restarted panel) goes to VictoriaMetrics and falls
// back to memory when it fails, and the longest
// read is the retention. observe.disk fires once per crossing of twice the
// free-space floor.
func TestObserveRouting(t *testing.T) {
	ctx := context.Background()
	o := newWorld(t).orch
	if err := o.SetSettings(ctx, map[string]string{"observe_enabled": "true", "observe_metrics_retention": "7", "observe_min_free": "5"}); err != nil {
		t.Fatal(err)
	}
	if !o.ObserveOn() || o.metricsKeep(ctx) != 7*24*time.Hour {
		t.Fatalf("on %v keep %v", o.ObserveOn(), o.metricsKeep(ctx))
	}
	writes, fail := 0, false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		switch {
		case fail:
			http.Error(w, "down", http.StatusServiceUnavailable)
		case r.URL.Path == "/api/v1/import":
			writes++
		default:
			_, _ = io.WriteString(w, `{"data":{"result":[{"metric":{"__name__":"stackr_cpu_pct"},"values":[[1792579060,"7"]]}]}}`)
		}
	}))
	defer srv.Close()
	o.observe.vmOnce.Do(func() {})
	o.observe.vm = &metrics.Victoria{URL: srv.URL}

	now := time.Now()
	if err := o.metricsStore().Write(ctx, []metrics.Point{{Key: "node:n1", Name: "cpu_pct", T: now, V: 3}}); err != nil {
		t.Fatal(err)
	}
	if writes != 1 {
		t.Errorf("victoria writes %d", writes)
	}
	read := func(r time.Duration) float64 {
		s, err := o.metricsStore().Query(ctx, "node:n1", now.Add(-r), now.Add(time.Second), r)
		if err != nil || len(s["cpu_pct"]) != 1 {
			t.Fatalf("read %v: %v %v", r, s, err)
		}
		return s["cpu_pct"][0].V
	}
	if v := read(time.Hour); v != 7 {
		t.Errorf("1h read on a panel up for seconds from %v, want victoria", v)
	}
	o.observe.memFrom.Store(now.Add(-2 * time.Hour).UnixNano())
	if v := read(time.Hour); v != 3 {
		t.Errorf("1h read on a panel up for 2h from %v, want memory", v)
	}
	if v := read(7 * 24 * time.Hour); v != 7 {
		t.Errorf("7d read from %v, want victoria", v)
	}
	fail = true
	if v := read(7 * 24 * time.Hour); v != 3 {
		t.Errorf("7d read with victoria down from %v, want memory", v)
	}
	if err := o.metricsStore().Write(ctx, []metrics.Point{{Key: "node:n1", Name: "cpu_pct", T: now, V: 3}}); err != nil {
		t.Error("a failed history write fails the round:", err)
	}

	h := newHook(t)
	alertsTo(t, o, h, "")
	n := store.Node{ID: "n1", Name: "n1"}
	const g = 1 << 30
	for i, s := range []struct {
		free  uint64
		fires int
	}{{50 * g, 0}, {9 * g, 1}, {8 * g, 0}, {20 * g, 0}, {9 * g, 1}} {
		o.observeDiskAlert(ctx, n, s.free)
		if got := h.events(t, o); len(got) != s.fires {
			t.Errorf("step %d: %v", i, got)
		}
	}

	if err := o.SetSettings(ctx, map[string]string{"observe_enabled": "false"}); err != nil {
		t.Fatal(err)
	}
	if o.ObserveOn() || o.metricsKeep(ctx) != MetricsKeep {
		t.Errorf("off: on %v keep %v", o.ObserveOn(), o.metricsKeep(ctx))
	}
}

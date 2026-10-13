package service

import (
	"context"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/metrics"
	"github.com/FyrmForge/stackr/internal/worker"
)

// Two raw samples 10 s apart become rates; the first sample of a node
// yields nothing; a counter that went backwards yields nothing for it.
func TestMetricsIngest(t *testing.T) {
	t0 := time.Unix(2_000_000, 0)
	host := func(total, idle uint64) worker.HostSample {
		return worker.HostSample{CPUTotal: total, CPUIdle: idle, MemTotal: 1000, MemAvail: 400, DiskTotal: 100, DiskFree: 30}
	}
	a := worker.Metrics{At: t0, Host: host(1000, 900), Containers: []worker.ContainerSample{
		{Tile: "t1", Name: "web-1", Role: "replica", CPUUsec: 0, Mem: 50, IORead: 0, IOWrite: 0},
		{Tile: "t1", Name: "web-2", Role: "replica", CPUUsec: 0, Mem: 30},
		{Tile: "t1", Name: "pause", Role: "pause", NetRx: 0, NetTx: 0},
	}}
	b := worker.Metrics{At: t0.Add(10 * time.Second), Host: host(2000, 1400), Containers: []worker.ContainerSample{
		{Tile: "t1", Name: "web-1", Role: "replica", CPUUsec: 5_000_000, Mem: 60, IORead: 1000, IOWrite: 2000},
		{Tile: "t1", Name: "web-2", Role: "replica", CPUUsec: 1_000_000, Mem: 40},
		{Tile: "t1", Name: "pause", Role: "pause", NetRx: 10_000, NetTx: 5_000},
	}}
	o := &Orchestrator{cfg: Config{Metrics: &metrics.Memory{}}}
	if pts := o.takeSample("n1", a); pts != nil {
		t.Fatalf("first sample made points: %v", pts)
	}
	pts := o.takeSample("n1", b)
	if err := o.metricsStore().Write(context.Background(), pts); err != nil {
		t.Fatal(err)
	}
	get := func(key, name string) float64 {
		t.Helper()
		s, _ := o.metricsStore().Query(context.Background(), key, t0, t0.Add(time.Minute), 0)
		if len(s[name]) != 1 {
			t.Fatalf("%s %s = %v", key, name, s[name])
		}
		return s[name][0].V
	}
	if v := get("node:n1", "cpu_pct"); v != 50 { // 500 idle of 1000
		t.Errorf("host cpu = %v", v)
	}
	if v := get("node:n1", "mem_used"); v != 600 {
		t.Errorf("host mem = %v", v)
	}
	if v := get("ctr:t1/web-1", "cpu_pct"); v != 50 { // 5 s of cpu in 10 s
		t.Errorf("replica cpu = %v", v)
	}
	if v := get("ctr:t1/web-1", "io_write_bps"); v != 200 {
		t.Errorf("replica io = %v", v)
	}
	if v := get("tile:t1", "cpu_pct"); v != 60 {
		t.Errorf("tile cpu = %v", v)
	}
	if v := get("tile:t1", "mem"); v != 100 {
		t.Errorf("tile mem = %v", v)
	}
	if v := get("tile:t1", "net_rx_bps"); v != 1000 {
		t.Errorf("tile net = %v", v)
	}
	// A restarted replica's counters went back: no point for it.
	c := b
	c.At = b.At.Add(10 * time.Second)
	c.Containers = []worker.ContainerSample{{Tile: "t1", Name: "web-1", Role: "replica", CPUUsec: 10}}
	for _, p := range Rates("n1", b, c) {
		if p.Key == "ctr:t1/web-1" {
			t.Fatalf("reset counter made %v", p)
		}
	}
	if LinkKey("b", "a") != LinkKey("a", "b") {
		t.Fatal("link key depends on order")
	}
	if got := o.metrics.replicas["t1"]; len(got) != 2 {
		t.Fatalf("replicas = %v", got)
	}
}

package main

import (
	"strings"
	"testing"
)

// node metrics and tile metrics print latest, min, avg and max per measure,
// pass --range through, and the tile's rows are the total then each replica.
func TestMetricsTables(t *testing.T) {
	r := fake(t, map[string]string{"/admin/nodes/ct110/metrics": `{"cpu_pct":[{"t":"2026-10-18T10:00:00Z","v":10},{"t":"2026-10-18T10:00:30Z","v":30},{"t":"2026-10-18T10:01:00Z","v":20}],` +
		`"mem_used":[{"t":"2026-10-18T10:00:00Z","v":1073741824}]}`})
	code, out, errw := cli(t, "node", "metrics", "ct110", "--range", "6h")
	if code != 0 {
		t.Fatalf("node metrics = %d %q", code, errw)
	}
	if !strings.Contains(out, "ct110\tcpu_pct\t20.0%\t10.0%\t20.0%\t30.0%") || !strings.Contains(out, "ct110\tmem_used\t1.0 GB\t1.0 GB\t1.0 GB\t1.0 GB") {
		t.Errorf("node table:\n%s", out)
	}
	if len(r.reqs) != 1 || !strings.HasPrefix(r.reqs[0], "GET /api/v1/admin/nodes/ct110/metrics?range=6h ") {
		t.Errorf("reqs = %v", r.reqs)
	}

	r = fake(t, map[string]string{"/tiles/api/metrics": `{"tile":{"net_rx_bps":[{"t":"2026-10-18T10:00:00Z","v":2048}]},` +
		`"replicas":{"web-2":{"cpu_pct":[{"t":"2026-10-18T10:00:00Z","v":5}]},"web-1":{"cpu_pct":[{"t":"2026-10-18T10:00:00Z","v":1.25}]}}}`})
	code, out, errw = cli(t, "tile", "metrics", "api", "--stack", "shop", "--env", "dev")
	if code != 0 {
		t.Fatalf("tile metrics = %d %q", code, errw)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	want := []string{"total\tnet_rx_bps\t2.0 KB/s", "web-1\tcpu_pct\t1.2%", "web-2\tcpu_pct\t5.0%"}
	if len(lines) != len(want) {
		t.Fatalf("tile table:\n%s", out)
	}
	for i, w := range want {
		if !strings.HasPrefix(lines[i], w) {
			t.Errorf("row %d = %q, want %q...", i, lines[i], w)
		}
	}
	if !strings.Contains(r.reqs[len(r.reqs)-1], "/metrics?range=1h ") {
		t.Errorf("default range: %v", r.reqs)
	}
}

// node link-metrics prints the tunnel's series for the node and its peer.
func TestLinkMetricsTable(t *testing.T) {
	r := fake(t, map[string]string{"/admin/nodes/ct110/link-metrics": `{"rtt_ms":[{"t":"2026-10-18T10:00:00Z","v":4}],"loss":[{"t":"2026-10-18T10:00:00Z","v":0.5}]}`})
	code, out, errw := cli(t, "node", "link-metrics", "ct110", "ct111", "--range", "6h")
	if code != 0 {
		t.Fatalf("link-metrics = %d %q", code, errw)
	}
	if !strings.Contains(out, "ct110-ct111\trtt_ms\t4.0 ms") || !strings.Contains(out, "ct110-ct111\tloss\t50.0%") {
		t.Errorf("table:\n%s", out)
	}
	if len(r.reqs) != 1 || !strings.HasPrefix(r.reqs[0], "GET /api/v1/admin/nodes/ct110/link-metrics?peer=ct111&range=6h ") {
		t.Errorf("reqs = %v", r.reqs)
	}
}

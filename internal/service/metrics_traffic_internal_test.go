package service

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/metrics"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/worker"
)

const caddyText = `# HELP caddy_http_request_duration_seconds Histogram of round-trip request durations.
# TYPE caddy_http_request_duration_seconds histogram
caddy_http_request_duration_seconds_bucket{code="200",handler="reverse_proxy",host="shop.example.com",method="GET",server="https",le="+Inf"} %d
caddy_http_request_duration_seconds_sum{code="200",handler="reverse_proxy",host="shop.example.com",method="GET",server="https"} %s
caddy_http_request_duration_seconds_count{code="200",handler="reverse_proxy",host="shop.example.com",method="GET",server="https"} %d
caddy_http_request_duration_seconds_bucket{code="502",handler="reverse_proxy",host="Shop.Example.com:443",method="POST",server="https",le="+Inf"} %d
caddy_http_request_duration_seconds_sum{code="502",handler="reverse_proxy",host="Shop.Example.com:443",method="POST",server="https"} 0
caddy_http_request_duration_seconds_count{code="502",handler="reverse_proxy",host="Shop.Example.com:443",method="POST",server="https"} %d
caddy_http_request_duration_seconds_bucket{code="404",handler="static_response",host="a.wild.example.com",method="GET",server="https",le="+Inf"} %d
caddy_http_request_duration_seconds_sum{code="404",handler="static_response",host="a.wild.example.com",method="GET",server="https"} 0
caddy_http_request_duration_seconds_count{code="404",handler="static_response",host="a.wild.example.com",method="GET",server="https"} %d
caddy_http_request_duration_seconds_bucket{code="200",handler="static_response",host="_other",method="GET",server="http",le="+Inf"} 7
caddy_http_request_duration_seconds_sum{code="200",handler="static_response",host="_other",method="GET",server="http"} 1
caddy_http_request_duration_seconds_count{code="200",handler="static_response",host="_other",method="GET",server="http"} 7
`

func caddyAt(t *testing.T, ok int, sum string, bad, wild int) map[string]hostCount {
	t.Helper()
	body := fmt.Sprintf(caddyText, ok, sum, ok, bad, bad, wild, wild)
	hs, err := parseCaddy(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return hs
}

// Caddy's per host histogram becomes requests, 4xx, 5xx and seconds per
// host (port and case dropped, _other left out), and two samples become each
// tile's rates summed over hosts and the average latency.
func TestCaddyTraffic(t *testing.T) {
	a := caddyAt(t, 100, "10", 0, 5)
	if len(a) != 2 {
		t.Fatalf("hosts = %v", a)
	}
	if c := a["shop.example.com"]; c.n != 100 || c.sum != 10 || c.e4 != 0 || c.e5 != 0 {
		t.Fatalf("shop = %+v", c)
	}
	if c := a["a.wild.example.com"]; c.n != 5 || c.e4 != 5 {
		t.Fatalf("wild = %+v", c)
	}
	b := caddyAt(t, 130, "13", 10, 25)
	tileOf := tileOfHost([]store.Domain{
		{TileID: "shop", Host: "shop.example.com"},
		{TileID: "wild", Host: "*.wild.example.com"},
		{TileID: "x", Host: "split.example.com", Path: "/a"},
		{TileID: "y", Host: "split.example.com", Path: "/b"},
	})
	if tileOf("split.example.com") != "" || tileOf("other.example.com") != "" {
		t.Fatal("a shared or unknown host maps to a tile")
	}
	t0 := time.Unix(2_000_000, 0)
	acc := map[string]*tileTraffic{}
	addTraffic(acc, caddySample{at: t0, hosts: a}, caddySample{at: t0.Add(10 * time.Second), hosts: b}, tileOf)
	// A second ingress node with the same host adds its rates.
	addTraffic(acc, caddySample{at: t0, hosts: map[string]hostCount{"shop.example.com": {n: 0}}},
		caddySample{at: t0.Add(20 * time.Second), hosts: map[string]hostCount{"shop.example.com": {n: 20, sum: 1}}}, tileOf)
	got := map[string]float64{}
	for _, p := range trafficOut(acc, t0) {
		got[strings.TrimPrefix(p.Key, "tile:")+" "+p.Name] = p.V
	}
	want := map[string]float64{
		"shop req_s": 4 + 1, "shop 4xx_s": 0, "shop 5xx_s": 1,
		"shop latency_ms": 1000 * (3 + 1) / 60.0, // 40 + 20 new requests, 4 s between them
		"wild req_s":      2, "wild 4xx_s": 2, "wild 5xx_s": 0,
		"wild latency_ms": 0,
	}
	for k, v := range want {
		if g, ok := got[k]; !ok || g != v {
			t.Errorf("%s = %v (ok %v), want %v", k, g, ok, v)
		}
	}
	// A config load reset the counters: no point for the host.
	acc = map[string]*tileTraffic{}
	addTraffic(acc, caddySample{at: t0, hosts: b}, caddySample{at: t0.Add(10 * time.Second), hosts: a}, tileOf)
	if acc["shop"] != nil {
		t.Fatalf("reset made %+v", acc["shop"])
	}
}

// A tunnel's rates come from the lower node id's wg counters, else the other
// end's swapped; the handshake age is the fresher end's.
func TestWGPoints(t *testing.T) {
	t0 := time.Unix(2_000_000, 0)
	smp := func(at time.Time, key string, rx, tx, hs int64) worker.Metrics {
		return worker.Metrics{At: at, Peers: map[string]worker.PeerSample{key: {Rx: rx, Tx: tx, Handshake: hs}}}
	}
	peers := func(self string) map[string]string {
		return map[string]map[string]string{"a": {"b": "KB"}, "b": {"a": "KA"}}[self]
	}
	got := map[string][2]worker.Metrics{
		"a": {smp(t0, "KB", 1000, 500, 0), smp(t0.Add(10*time.Second), "KB", 3000, 1500, 2_000_005)},
		"b": {smp(t0, "KA", 0, 0, 0), smp(t0.Add(10*time.Second), "KA", 9000, 9000, 2_000_002)},
	}
	vals := func(ps []metrics.Point) map[string]float64 {
		m := map[string]float64{}
		for _, p := range ps {
			if p.Key != LinkKey("a", "b") {
				t.Fatalf("key %s", p.Key)
			}
			m[p.Name] = p.V
		}
		return m
	}
	v := vals(wgPoints(got, peers, t0))
	if v["rx_bps"] != 200 || v["tx_bps"] != 100 || v["handshake_age_s"] != 5 || len(v) != 3 {
		t.Fatalf("both ends: %v", v)
	}
	delete(got, "a") // a did not report: b's counters, rx and tx swapped
	v = vals(wgPoints(got, peers, t0))
	if v["rx_bps"] != 900 || v["tx_bps"] != 900 || v["handshake_age_s"] != 8 {
		t.Fatalf("b only: %v", v)
	}
	got["b"] = [2]worker.Metrics{got["b"][1], smp(t0.Add(20*time.Second), "KA", 5, 5, 0)} // reset, no handshake
	if v = vals(wgPoints(got, peers, t0)); len(v) != 0 {
		t.Fatalf("reset: %v", v)
	}
}

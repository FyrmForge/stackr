package traffic

import (
	"fmt"
	"testing"
	"time"
)

// line is one conntrack entry: original src->dst, reply dst->src.
func line(src, dst string, sport, orig, reply int) string {
	return fmt.Sprintf("ipv4 2 tcp 6 86399 ESTABLISHED src=%s dst=%s sport=%d dport=80 packets=3 bytes=%d "+
		"src=%s dst=%s sport=80 dport=%d packets=3 bytes=%d [ASSURED] mark=0 use=1\n", src, dst, sport, orig, dst, src, sport, reply)
}

var (
	t0    = time.Unix(1000, 0)
	ipMap = map[string]string{"10.0.0.2": "web", "10.0.0.3": "api"}
)

func TestSample(t *testing.T) {
	l := New()

	// First sight: counters seeded, nothing counted (the old code spiked
	// with the connection's whole lifetime here).
	l.Sample(ipMap, []byte(line("10.0.0.2", "10.0.0.3", 5000, 1000, 5000)), t0)
	if s := l.Snapshot(); len(s) != 0 {
		t.Fatalf("first sight = %v, want nothing", s)
	}

	// Two ticks give a rate over the real elapsed time.
	l.Sample(ipMap, []byte(line("10.0.0.2", "10.0.0.3", 5000, 2000, 10000)), t0.Add(10*time.Second))
	s := l.Snapshot()
	if s[Pair{"web", "api"}] != 100 || s[Pair{"api", "web"}] != 500 || len(s) != 2 {
		t.Fatalf("rate = %v, want web->api 100, api->web 500", s)
	}

	// Counter went backwards (a reused tuple): 0 this tick, then a rate again.
	l.Sample(ipMap, []byte(line("10.0.0.2", "10.0.0.3", 5000, 10, 10)), t0.Add(15*time.Second))
	if s := l.Snapshot(); len(s) != 0 {
		t.Fatalf("reset = %v, want nothing", s)
	}
	l.Sample(ipMap, []byte(line("10.0.0.2", "10.0.0.3", 5000, 510, 10)), t0.Add(20*time.Second))
	if s := l.Snapshot(); s[Pair{"web", "api"}] != 100 {
		t.Fatalf("after reset = %v, want web->api 100", s)
	}

	// A closed tuple is forgotten.
	l.Sample(ipMap, nil, t0.Add(25*time.Second))
	if len(l.prev) != 0 {
		t.Fatalf("closed tuple kept: %v", l.prev)
	}
	// After the seed, a tuple first seen counts in full: a short request
	// shows up already closed with its final counters.
	l.Sample(ipMap, []byte(line("10.0.0.2", "10.0.0.3", 5001, 1000, 5000)), t0.Add(30*time.Second))
	if s := l.Snapshot(); s[Pair{"web", "api"}] != 200 || s[Pair{"api", "web"}] != 1000 {
		t.Fatalf("new tuple = %v, want web->api 200, api->web 1000", s)
	}
	// Still there next tick with the same counters: counted once.
	l.Sample(ipMap, []byte(line("10.0.0.2", "10.0.0.3", 5001, 1000, 5000)), t0.Add(35*time.Second))
	if s := l.Snapshot(); len(s) != 0 {
		t.Fatalf("lingering = %v, want nothing", s)
	}
	if l.Seq() != 7 {
		t.Errorf("seq = %d, want 7", l.Seq())
	}
}

// Parallel connections between the same two tiles sum; same-tile and
// host-only lines never count.
func TestSampleSums(t *testing.T) {
	l := New()
	ips := map[string]string{"10.0.0.2": "web", "10.0.0.4": "web", "10.0.0.3": "api"}
	dump := func(n int) []byte {
		return []byte(line("10.0.0.2", "10.0.0.3", 5000, n, 0) + line("10.0.0.4", "10.0.0.3", 5001, n, 0) +
			line("10.0.0.2", "10.0.0.4", 5002, n, n) + line("192.168.1.5", "192.168.1.6", 22, n, n))
	}
	l.Sample(ips, dump(0), t0)
	l.Sample(ips, dump(500), t0.Add(5*time.Second))
	if s := l.Snapshot(); s[Pair{"web", "api"}] != 200 || len(s) != 1 {
		t.Fatalf("sum = %v, want only web->api 200", s)
	}
}

// 3b: two consumers on one instance each get their own slice, both lanes;
// a tile with no binding keeps the instance as its end.
func TestSlices(t *testing.T) {
	pairs := map[Pair]float64{
		{"web", "pg"}: 10, {"pg", "web"}: 100,
		{"jobs", "pg"}: 20, {"pg", "jobs"}: 200,
		{"cron", "pg"}:   30,
		{"proxy", "web"}: 5,
	}
	bind := map[Pair]string{{"web", "pg"}: "slice-web", {"jobs", "pg"}: "slice-jobs"}
	got := Slices(pairs, bind)
	want := map[Pair]float64{
		{"web", "slice-web"}: 10, {"slice-web", "web"}: 100,
		{"jobs", "slice-jobs"}: 20, {"slice-jobs", "jobs"}: 200,
		{"cron", "pg"}:   30,
		{"proxy", "web"}: 5,
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for p, v := range want {
		if got[p] != v {
			t.Errorf("%v = %v, want %v", p, got[p], v)
		}
	}
}

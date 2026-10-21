// Package metrics is the metrics store behind a narrow interface: the panel
// writes points and reads series, and knows nothing of the backend. Memory
// keeps the last 24 h for the live graphs (and is the tests' fake); Victoria
// keeps the history when observability is on.
package metrics

import (
	"context"
	"sort"
	"sync"
	"time"
)

// Point is one value of one series. Key names the thing ("node:<id>",
// "ctr:<tile>/<container>", "tile:<tile>", "link:<a>|<b>"); Name the
// measure ("cpu_pct", "mem", ...).
type Point struct {
	Key, Name string
	T         time.Time
	V         float64
}

// Sample is one value in time.
type Sample struct {
	T time.Time `json:"t"`
	V float64   `json:"v"`
}

// Store keeps points. Retention and downsampling are the backend's own.
type Store interface {
	Write(ctx context.Context, ps []Point) error
	// Query is every measure of key in [from, to], averaged per step.
	Query(ctx context.Context, key string, from, to time.Time, step time.Duration) (map[string][]Sample, error)
}

// Memory keeps Keep of points in memory; lost on restart.
// ponytail: interim store, swapped for the real backend; fine for a few
// hundred series at 30 s.
type Memory struct {
	Keep time.Duration
	mu   sync.Mutex
	m    map[string]map[string][]Sample
}

func (s *Memory) Write(_ context.Context, ps []Point) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]map[string][]Sample{}
	}
	keep := s.Keep
	if keep == 0 {
		keep = 24 * time.Hour
	}
	for _, p := range ps {
		if s.m[p.Key] == nil {
			s.m[p.Key] = map[string][]Sample{}
		}
		s.m[p.Key][p.Name] = append(s.m[p.Key][p.Name], Sample{p.T, p.V})
		xs := s.m[p.Key][p.Name]
		cut := p.T.Add(-keep)
		i := sort.Search(len(xs), func(i int) bool { return !xs[i].T.Before(cut) })
		s.m[p.Key][p.Name] = xs[i:]
	}
	return nil
}

func (s *Memory) Query(_ context.Context, key string, from, to time.Time, step time.Duration) (map[string][]Sample, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string][]Sample{}
	for name, xs := range s.m[key] {
		out[name] = Bucket(xs, from, to, step)
	}
	return out, nil
}

// Bucket averages the samples in [from, to] per step (0 = as stored); a
// bucket is stamped with its start and empty ones are left out.
func Bucket(xs []Sample, from, to time.Time, step time.Duration) []Sample {
	var out []Sample
	var sum float64
	n := 0
	var cur time.Time
	flush := func() {
		if n > 0 {
			out = append(out, Sample{cur, sum / float64(n)})
		}
		sum, n = 0, 0
	}
	for _, x := range xs {
		if x.T.Before(from) || x.T.After(to) {
			continue
		}
		b := x.T
		if step > 0 {
			b = from.Add(x.T.Sub(from) / step * step)
		}
		if n > 0 && !b.Equal(cur) {
			flush()
		}
		cur = b
		sum += x.V
		n++
	}
	flush()
	return out
}

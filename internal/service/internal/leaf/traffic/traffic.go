// Package traffic turns the host conntrack table into bytes per second per
// ordered pair of tiles. Memory only: no table, no history, no Docker. The
// flow hands in the IP-to-tile map and the raw file each tick; the latest
// snapshot is all this keeps.
package traffic

import (
	"strconv"
	"strings"
	"sync"
	"time"
)

// Pseudo ends: Proxy is the ingress side (the proxy container and every
// stackr network's gateway), Internet a tile-side connection's far end that
// maps to nothing.
const (
	Proxy    = "proxy"
	Internet = "internet"
)

// Every is the sampling tick.
const Every = 5 * time.Second

// Pair is one lane: bytes moving From -> To.
type Pair struct{ From, To string }

// Edge is one lane as the API and the graph see it: tile, slice or pseudo
// ids, bytes per second.
type Edge struct {
	From     string  `json:"from"`
	To       string  `json:"to"`
	FromName string  `json:"from_name"` // slug here, stack/env/slug elsewhere, the id when unknown
	ToName   string  `json:"to_name"`
	BPS      float64 `json:"bps"`
}

// Leaf holds the counters between ticks and the latest snapshot.
type Leaf struct {
	mu    sync.Mutex
	prev  map[string][2]uint64 // 4-tuple -> [orig, reply] cumulative bytes
	last  time.Time
	pairs map[Pair]float64
	seq   int64
}

func New() *Leaf { return &Leaf{prev: map[string][2]uint64{}, pairs: map[Pair]float64{}} }

// Sample reads one conntrack dump. ipTile maps a container IP to its tile
// id, or to Proxy. The first sample only seeds counters (a connection's
// lifetime bytes are not this tick's). After that a tuple seen for the first
// time counts in full: a short request opens and closes between two ticks,
// so its first sight is its whole life.
// ponytail: a long-lived tuple whose end only now maps to a tile (a new
// container) also counts in full once; track unmapped tuples if that spike
// shows.
func (l *Leaf) Sample(ipTile map[string]string, conntrack []byte, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	interval := Every.Seconds()
	seed := l.last.IsZero()
	if !seed {
		if dt := now.Sub(l.last).Seconds(); dt > 0 {
			interval = dt
		}
	}
	l.last = now
	pairs := map[Pair]float64{}
	seen := map[string]bool{}
	for _, line := range strings.Split(string(conntrack), "\n") {
		src1, sport1, dst1, dport1, b1 := flowFields(line, 0)
		_, _, _, _, b2 := flowFields(line, 1)
		if src1 == "" {
			continue
		}
		from, to, ok := ends(ipTile, src1, dst1)
		if !ok {
			continue
		}
		// full 4-tuple: the source port is what separates concurrent
		// connections between the same two containers
		tuple := src1 + ":" + sport1 + ">" + dst1 + ":" + dport1
		cur := [2]uint64{b1, b2}
		prev, existed := l.prev[tuple]
		l.prev[tuple] = cur
		seen[tuple] = true
		var dOrig, dReply uint64
		switch {
		case !existed && !seed:
			dOrig, dReply = b1, b2
		case existed && b1 >= prev[0] && b2 >= prev[1]:
			dOrig, dReply = b1-prev[0], b2-prev[1]
		}
		// requests ride from->to, responses ride the return lane
		pairs[Pair{from, to}] += float64(dOrig) / interval
		pairs[Pair{to, from}] += float64(dReply) / interval
	}
	for k := range l.prev {
		if !seen[k] {
			delete(l.prev, k)
		}
	}
	for p, v := range pairs {
		if v <= 0 {
			delete(pairs, p)
		}
	}
	l.pairs = pairs
	l.seq++
}

// ends names both ends of a connection by its original direction. Both
// known: as mapped. One a tile, the other unknown: that other end is
// Internet. Anything else (host, proxy-to-outside) is not ours.
func ends(ipTile map[string]string, src, dst string) (from, to string, ok bool) {
	from, okF := ipTile[src]
	to, okT := ipTile[dst]
	switch {
	case okF && okT:
	case okF && from != Proxy:
		to = Internet
	case okT && to != Proxy:
		from = Internet
	default:
		return "", "", false
	}
	return from, to, from != to
}

// Snapshot is a copy of the latest tick's lanes; a lane at 0 is absent.
func (l *Leaf) Snapshot() map[Pair]float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[Pair]float64, len(l.pairs))
	for p, v := range l.pairs {
		out[p] = v
	}
	return out
}

// Seq counts ticks; a stream sends when it moves.
func (l *Leaf) Seq() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seq
}

// Slices renames consumer <-> managed instance lanes to consumer <-> slice.
// bind maps {consumer tile, instance tile} to the consumer's provision id on
// that instance; a pair without a binding keeps the instance as its end.
func Slices(pairs map[Pair]float64, bind map[Pair]string) map[Pair]float64 {
	out := make(map[Pair]float64, len(pairs))
	for p, v := range pairs {
		if s, ok := bind[p]; ok {
			p.To = s
		} else if s, ok := bind[Pair{p.To, p.From}]; ok {
			p.From = s
		}
		out[p] += v
	}
	return out
}

// flowFields pulls the nth (0=original, 1=reply) src/sport/dst/dport/bytes
// group out of one conntrack line.
func flowFields(line string, n int) (src, sport, dst, dport string, bytes uint64) {
	var srcs, sports, dsts, dports []string
	var bytesv []uint64
	for _, f := range strings.Fields(line) {
		switch {
		case strings.HasPrefix(f, "src="):
			srcs = append(srcs, f[4:])
		case strings.HasPrefix(f, "sport="):
			sports = append(sports, f[6:])
		case strings.HasPrefix(f, "dst="):
			dsts = append(dsts, f[4:])
		case strings.HasPrefix(f, "dport="):
			dports = append(dports, f[6:])
		case strings.HasPrefix(f, "bytes="):
			v, _ := strconv.ParseUint(f[6:], 10, 64)
			bytesv = append(bytesv, v)
		}
	}
	if n < len(srcs) && n < len(dsts) {
		src, dst = srcs[n], dsts[n]
	}
	if n < len(sports) {
		sport = sports[n]
	}
	if n < len(dports) {
		dport = dports[n]
	}
	if n < len(bytesv) {
		bytes = bytesv[n]
	}
	return
}

package wgapply

import (
	"net/netip"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/mesh"
)

var ap = netip.MustParseAddrPort

// rig is a failover over three candidates (private, public, relay) on a
// fake clock; at advances the clock and ticks, returning where the peer is.
type rig struct {
	t   *testing.T
	now time.Time
	rec *Recorder
	f   *Failover
}

var cands = []netip.AddrPort{ap("10.0.0.2:1"), ap("1.2.3.4:1"), ap("5.6.7.8:9")}

func newRig(t *testing.T) *rig {
	r := &rig{t: t, now: time.Unix(1000, 0), rec: &Recorder{}}
	r.f = &Failover{A: r.rec, Iface: "stackr0", Now: func() time.Time { return r.now }}
	r.f.Reset([]mesh.Peer{{PublicKey: "k", Candidates: cands}})
	return r
}

func (r *rig) at(d time.Duration) netip.AddrPort {
	r.t.Helper()
	r.now = r.now.Add(d)
	if _, err := r.f.Tick(); err != nil {
		r.t.Fatal(err)
	}
	return r.rec.Moves[len(r.rec.Moves)-1].Endpoint
}

// live makes the peer answer: a fresh handshake and some bytes.
func (r *rig) live() { r.rec.SetHandshake("k", r.now); r.rec.AddRx("k", 100) }

func TestProbeIsThirtySeconds(t *testing.T) {
	r := newRig(t)
	if got := r.at(29 * time.Second); got != cands[0] {
		t.Fatalf("moved inside the probe window: %v", got)
	}
	if got := r.at(1 * time.Second); got != cands[1] {
		t.Fatalf("a silent candidate gets 30 s: %v", got)
	}
	r.at(10 * time.Second)
	r.live() // handshake after the switch proves the public path
	r.at(1 * time.Second)
	// Settled: traffic keeps it there well past the probe window.
	for range 10 {
		r.rec.AddRx("k", 50)
		if got := r.at(20 * time.Second); got != cands[1] {
			t.Fatalf("moved while traffic flows: %v", got)
		}
	}
}

func TestSettledPathDeadAfterSixtySecondsSilence(t *testing.T) {
	r := newRig(t)
	r.live()
	r.at(time.Second) // proven on the private path
	r.rec.AddRx("k", 1)
	r.at(10 * time.Second)
	if got := r.at(59 * time.Second); got != cands[0] {
		t.Fatalf("moved before 60 s of silence: %v", got)
	}
	if got := r.at(1 * time.Second); got != cands[1] {
		t.Fatalf("60 s of silence moves on: %v", got)
	}
}

func TestFailedRoundsPauseLonger(t *testing.T) {
	r := newRig(t)
	r.at(30 * time.Second) // -> public
	r.at(30 * time.Second) // -> relay
	if got := r.at(30 * time.Second); got != cands[0] {
		t.Fatalf("round 1 wraps to the first: %v", got)
	}
	// Each wrap gives the first candidate 30 s plus the round pause.
	for i, pause := range []time.Duration{30 * time.Second, 2 * time.Minute, 5 * time.Minute, 5 * time.Minute} {
		if got := r.at(Probe + pause - time.Second); got != cands[0] {
			t.Fatalf("round %d: left the first before its pause: %v", i+1, got)
		}
		if got := r.at(time.Second); got != cands[1] {
			t.Fatalf("round %d: did not move after the pause: %v", i+1, got)
		}
		r.at(30 * time.Second) // -> relay
		r.at(30 * time.Second) // -> first again
	}
	// Success resets the pauses.
	r.live()
	r.at(time.Second)
	r.at(Dead) // silent for 60 s: -> public, a plain probe again
	r.at(30 * time.Second)
	if got := r.at(30 * time.Second); got != cands[0] {
		t.Fatal(got)
	}
	if got := r.at(Probe + 30*time.Second); got != cands[1] {
		t.Fatalf("after a success the first pause is 30 s again: %v", got)
	}
}

func TestClimbBackFromTheRelay(t *testing.T) {
	r := newRig(t)
	r.at(30 * time.Second) // -> public
	r.at(30 * time.Second) // -> relay
	r.live()
	r.at(time.Second) // settled on the relay
	keep := func(d time.Duration) netip.AddrPort { r.rec.AddRx("k", 10); return r.at(d) }
	if got := keep(Climb - 2*time.Second); got != cands[2] {
		t.Fatalf("climbed early: %v", got)
	}
	if got := keep(2 * time.Second); got != cands[0] {
		t.Fatalf("climb starts at the best candidate: %v", got)
	}
	// The relay session keeps sending, but from the relay address: the
	// kernel roams the endpoint back, so that traffic proves nothing.
	r.rec.Moves = append(r.rec.Moves, Move{"stackr0", "k", cands[2]})
	if got := keep(Probe); got != cands[1] {
		t.Fatalf("a failed climb step tries the next better one: %v", got)
	}
	r.rec.Moves = append(r.rec.Moves, Move{"stackr0", "k", cands[2]})
	if got := keep(Probe); got != cands[2] {
		t.Fatalf("no better path: back to the working relay: %v", got)
	}
	if got := keep(Probe); got != cands[2] {
		t.Fatalf("settled back home, no walk: %v", got)
	}
	// Next climb 5 min after returning, and this time the private path
	// answers: traffic at that endpoint, no new handshake yet.
	if got := keep(Climb - Probe); got != cands[0] {
		t.Fatalf("second climb: %v", got)
	}
	if got := keep(5 * time.Second); got != cands[0] {
		t.Fatal(got)
	}
	if got := keep(Probe * 4); got != cands[0] {
		t.Fatalf("stays on the better path it climbed to: %v", got)
	}
}

func TestSingleCandidateNeverMoves(t *testing.T) {
	rec := &Recorder{}
	f := &Failover{A: rec, Iface: "stackr0", Now: func() time.Time { return time.Unix(9999, 0) }}
	f.Reset([]mesh.Peer{{PublicKey: "k", Candidates: []netip.AddrPort{ap("1.1.1.1:1")}}})
	f.Now = func() time.Time { return time.Unix(99999, 0) }
	if m, _ := f.Tick(); len(m) != 0 {
		t.Fatal(m)
	}
}

// A node that rotated its key comes back as a new peer on the candidate it
// was on, not the top of the walk.
func TestRotatedKeyKeepsCandidate(t *testing.T) {
	r := &rig{t: t, now: time.Unix(1000, 0), rec: &Recorder{}}
	r.f = &Failover{A: r.rec, Iface: "stackr0", Now: func() time.Time { return r.now }}
	r.f.Reset([]mesh.Peer{{NodeID: "n1", PublicKey: "old", Candidates: cands}})
	r.at(30 * time.Second) // -> public
	r.f.Reset([]mesh.Peer{{NodeID: "n1", PublicKey: "new", Candidates: cands}})
	if last := r.rec.Moves[len(r.rec.Moves)-1]; last.PublicKey != "new" || last.Endpoint != cands[1] {
		t.Fatalf("rotated peer set to %v", last)
	}
	r.f.Reset([]mesh.Peer{{NodeID: "n2", PublicKey: "other", Candidates: cands}})
	if last := r.rec.Moves[len(r.rec.Moves)-1]; last.Endpoint != cands[0] {
		t.Fatalf("a different node starts at the top: %v", last)
	}
}

// A node reached only through a hop has no candidates; rotating its key
// once crashed every node that applied the next push.
func TestRotatedKeyNoCandidates(t *testing.T) {
	r := &rig{t: t, now: time.Unix(1000, 0), rec: &Recorder{}}
	r.f = &Failover{A: r.rec, Iface: "stackr0", Now: func() time.Time { return r.now }}
	r.f.Reset([]mesh.Peer{{NodeID: "n1", PublicKey: "old"}})
	r.f.Reset([]mesh.Peer{{NodeID: "n1", PublicKey: "new"}})
	if len(r.rec.Moves) != 0 {
		t.Fatalf("set an endpoint for a peer with none: %v", r.rec.Moves)
	}
}

func TestSamePushKeepsPosition(t *testing.T) {
	r := newRig(t)
	r.at(30 * time.Second) // -> public
	r.f.Reset([]mesh.Peer{{PublicKey: "k", Candidates: cands}})
	if got := r.at(time.Second); got != cands[1] {
		t.Fatalf("a repeated push restarted the walk: %v", got)
	}
	r.f.Reset([]mesh.Peer{{PublicKey: "k", Candidates: cands[1:]}})
	if got := r.at(time.Second); got != cands[1] {
		t.Fatalf("a changed push starts on its first: %v", got)
	}
}

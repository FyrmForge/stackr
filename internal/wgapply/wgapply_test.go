package wgapply

import (
	"net/netip"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/mesh"
)

func TestFailoverWalksCandidates(t *testing.T) {
	ap := netip.MustParseAddrPort
	now := time.Unix(1000, 0)
	rec := &Recorder{}
	f := &Failover{A: rec, Iface: "wg0", Stale: 100 * time.Second, Now: func() time.Time { return now }}
	peers := []mesh.Peer{{PublicKey: "k", Candidates: []netip.AddrPort{ap("10.0.0.2:1"), ap("1.2.3.4:1"), ap("5.6.7.8:9")}}}
	f.Reset(peers)
	if len(rec.Moves) != 1 || rec.Moves[0].Endpoint != ap("10.0.0.2:1") {
		t.Fatalf("a new peer starts on its first candidate: %v", rec.Moves)
	}
	rec.Moves = nil

	moved := func() int { m, _ := f.Tick(); return len(m) }
	if moved() != 0 { // inside the window of the first candidate
		t.Fatal("moved early")
	}
	now = now.Add(101 * time.Second)
	if moved() != 1 || rec.Moves[0].Endpoint != ap("1.2.3.4:1") {
		t.Fatal(rec.Moves)
	}
	if moved() != 0 { // new candidate gets its own window
		t.Fatal("moved twice")
	}
	// healthy handshake keeps it in place
	now = now.Add(101 * time.Second)
	rec.SetHandshake("k", now.Add(-10*time.Second))
	if moved() != 0 {
		t.Fatal("moved while healthy")
	}
	// stale again: next, then wraps to the first
	now = now.Add(200 * time.Second)
	if moved() != 1 || rec.Moves[1].Endpoint != ap("5.6.7.8:9") {
		t.Fatal(rec.Moves)
	}
	now = now.Add(101 * time.Second)
	if moved() != 1 || rec.Moves[2].Endpoint != ap("10.0.0.2:1") {
		t.Fatal(rec.Moves)
	}
	// same push keeps position, changed push restarts
	f.Reset(peers)
	now = now.Add(101 * time.Second)
	if moved() != 1 || rec.Moves[3].Endpoint != ap("1.2.3.4:1") {
		t.Fatal(rec.Moves)
	}
}

func TestSingleCandidateNeverMoves(t *testing.T) {
	rec := &Recorder{}
	f := &Failover{A: rec, Iface: "wg0", Now: func() time.Time { return time.Unix(9999, 0) }}
	f.Reset([]mesh.Peer{{PublicKey: "k", Candidates: []netip.AddrPort{netip.MustParseAddrPort("1.1.1.1:1")}}})
	f.Now = func() time.Time { return time.Unix(99999, 0) }
	if m, _ := f.Tick(); len(m) != 0 {
		t.Fatal(m)
	}
}

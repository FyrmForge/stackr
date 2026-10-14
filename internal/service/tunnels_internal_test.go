package service

import (
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/mesh"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/worker"
)

func peerTo(id, key string, endpoint string) mesh.Peer {
	p := mesh.Peer{NodeID: id, PublicKey: key}
	if endpoint != "" {
		p.Candidates = []netip.AddrPort{netip.MustParseAddrPort(endpoint)}
	}
	return p
}

// The rig: 109, 110 and 106 share a LAN, h4 is public, h3 sits behind h4 in
// the Hetzner network. Who dials follows the specs' endpoint candidates.
func TestFillEndsRig(t *testing.T) {
	specs := map[string]worker.MeshSpec{
		"109": {Peers: []mesh.Peer{peerTo("110", "k110", "192.168.1.110:51820"), peerTo("106", "k106", "192.168.1.106:51820"), peerTo("h4", "kh4", "203.0.113.4:51820")}},
		"110": {Peers: []mesh.Peer{peerTo("109", "k109", "192.168.1.109:51820"), peerTo("106", "k106", "192.168.1.106:51820"), peerTo("h4", "kh4", "203.0.113.4:51820")}},
		"106": {Peers: []mesh.Peer{peerTo("109", "k109", "192.168.1.109:51820"), peerTo("110", "k110", "192.168.1.110:51820"), peerTo("h4", "kh4", "203.0.113.4:51820")}},
		"h4":  {Peers: []mesh.Peer{peerTo("109", "k109", ""), peerTo("110", "k110", ""), peerTo("106", "k106", ""), peerTo("h3", "kh3", "10.0.0.3:51820")}},
		"h3":  {Peers: []mesh.Peer{peerTo("h4", "kh4", "10.0.0.2:51820")}}, // h3 and 109 are routed via h4: no peer entry
	}
	hs := time.Now().Add(-2 * time.Minute).Unix()
	in := []clusterNodeIn{
		{Node: store.Node{ID: "109"}, Facts: &worker.Facts{
			Seen: map[string]string{"kh4": "203.0.113.4:51820"}, Handshake: map[string]int64{"kh4": hs},
			Rx: map[string]int64{"kh4": 100}, Tx: map[string]int64{"kh4": 200},
		}},
		{Node: store.Node{ID: "110"}}, {Node: store.Node{ID: "106"}},
		{Node: store.Node{ID: "h4"}, Facts: &worker.Facts{
			Seen: map[string]string{"k109": "198.51.100.9:4000"}, Rx: map[string]int64{"k109": 7},
		}},
		{Node: store.Node{ID: "h3"}},
	}
	link := func(a, b string) ClusterLink { return ClusterLink{A: a, B: b, MS: -1} }
	m := ClusterMap{Links: []ClusterLink{link("109", "110"), link("109", "106"), link("110", "106"), link("109", "h4"), link("h4", "h3")}}
	m.Pairs = append(slices.Clone(m.Links), link("109", "h3"))
	fillEnds(&m, in, func(id string) (worker.MeshSpec, bool) { sp, ok := specs[id]; return sp, ok })

	dials := func(l ClusterLink) [2]bool { return [2]bool{l.Ends[0].Dials, l.Ends[1].Dials} }
	for i, want := range [][2]bool{{true, true}, {true, true}, {true, true}, {true, false}, {true, true}} {
		if got := dials(m.Links[i]); got != want {
			t.Errorf("%s-%s dials %v, want %v", m.Links[i].A, m.Links[i].B, got, want)
		}
	}
	// 109 dials h4 and h4 has no endpoint for 109; h3 and 109 are routed:
	// neither dials.
	if got := dials(m.Pairs[5]); got != [2]bool{} {
		t.Errorf("routed pair dials %v, want neither", got)
	}
	e := m.Links[3].Ends
	if e[0].Endpoint != "203.0.113.4:51820" || e[1].Endpoint != "" {
		t.Errorf("endpoints = %q, %q", e[0].Endpoint, e[1].Endpoint)
	}
	if e[0].SeenFrom != "203.0.113.4:51820" || e[1].SeenFrom != "198.51.100.9:4000" {
		t.Errorf("seen from = %q, %q", e[0].SeenFrom, e[1].SeenFrom)
	}
	if e[0].Handshake.Unix() != hs || !e[1].Handshake.IsZero() {
		t.Errorf("handshakes = %v, %v", e[0].Handshake, e[1].Handshake)
	}
	// Counters: reported ones as is, anything not reported is -1.
	if e[0].Rx != 100 || e[0].Tx != 200 || e[1].Rx != 7 || e[1].Tx != -1 {
		t.Errorf("rx/tx = %d/%d and %d/%d", e[0].Rx, e[0].Tx, e[1].Rx, e[1].Tx)
	}
	if x := m.Links[0].Ends[0]; x.Rx != -1 || x.Tx != -1 {
		t.Errorf("a node with no facts reports %d/%d, want -1/-1", x.Rx, x.Tx)
	}
}

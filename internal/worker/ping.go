package worker

import (
	"context"
	"regexp"
	"strconv"
	"sync"

	"github.com/FyrmForge/stackr/internal/mesh"
)

// PeerPing is the ICMP round trip to a mesh peer's transit address.
type PeerPing struct {
	RTTms float64 `json:"rtt_ms"` // 0 when no reply came back
	Loss  float64 `json:"loss"`   // 0 to 1
}

var (
	pingLoss = regexp.MustCompile(`([\d.]+)% packet loss`)
	// iputils "rtt min/avg/max/mdev = a/b/c/d ms", busybox "round-trip
	// min/avg/max = a/b/c ms": the second number is the average.
	pingAvg = regexp.MustCompile(`= [\d.]+/([\d.]+)/`)
)

// parsePing reads ping's summary; ok is false when it printed none.
func parsePing(out string) (PeerPing, bool) {
	m := pingLoss.FindStringSubmatch(out)
	if m == nil {
		return PeerPing{}, false
	}
	loss, _ := strconv.ParseFloat(m[1], 64)
	p := PeerPing{Loss: loss / 100}
	if a := pingAvg.FindStringSubmatch(out); a != nil {
		p.RTTms, _ = strconv.ParseFloat(a[1], 64)
	}
	return p, true
}

// pingPeers pings every transit address the applied mesh reaches (a routed
// node's sits on its next hop's entry, so its time is the routed one), keyed
// by address. Three probes 200 ms apart, 1 s wait, 8 at a time: about 1.5 s
// when a peer is dead.
// ponytail: average of 3, not a median; enough for a link label.
func (s *Server) pingPeers(ctx context.Context) map[string]PeerPing {
	s.mu.Lock()
	a := s.mesh.applied
	s.mu.Unlock()
	if a == nil || s.cfg.Runner == nil {
		return nil
	}
	var addrs []string
	for _, p := range a.Peers {
		for _, ip := range p.AllowedIPs {
			if ip.Bits() == 32 && mesh.Transit.Contains(ip.Addr()) {
				addrs = append(addrs, ip.Addr().String())
			}
		}
	}
	out := map[string]PeerPing{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for _, addr := range addrs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			// A non-zero exit is 100% loss; the summary still says so.
			b, _ := s.cfg.Runner.Run(ctx, nil, "ping", "-q", "-c3", "-i0.2", "-W1", addr)
			if p, ok := parsePing(string(b)); ok {
				mu.Lock()
				out[addr] = p
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return out
}

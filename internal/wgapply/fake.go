package wgapply

import (
	"net/netip"
	"sync"
	"time"
)

// Recorder is the fake Applier: it keeps every applied Config, the endpoint
// moves and settable handshake times. Used by fake mode and tests.
type Recorder struct {
	mu         sync.Mutex
	Applied    []Config
	Moves      []Move
	handshakes map[string]time.Time
	rx         map[string]int64
}

// Move is one SetEndpoint call.
type Move struct {
	Iface, PublicKey string
	Endpoint         netip.AddrPort
}

var _ Applier = (*Recorder)(nil)

func (r *Recorder) Apply(cfg Config) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Applied = append(r.Applied, cfg)
	return nil
}

// Stats reports the set handshakes and bytes, and each peer's last move as
// its endpoint.
func (r *Recorder) Stats(string) (map[string]PeerStat, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]PeerStat{}
	for k, v := range r.handshakes {
		st := out[k]
		st.Handshake = v
		out[k] = st
	}
	for k, v := range r.rx {
		st := out[k]
		st.Rx = v
		out[k] = st
	}
	for _, m := range r.Moves {
		st := out[m.PublicKey]
		st.Endpoint = m.Endpoint
		out[m.PublicKey] = st
	}
	return out, nil
}

func (r *Recorder) SetEndpoint(iface, key string, ep netip.AddrPort) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Moves = append(r.Moves, Move{iface, key, ep})
	return nil
}

// Seen reports the last endpoint each peer was moved to.
func (r *Recorder) Seen(string) (map[string]netip.AddrPort, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]netip.AddrPort{}
	for _, m := range r.Moves {
		out[m.PublicKey] = m.Endpoint
	}
	return out, nil
}

// SetHandshake sets what Handshakes reports for a peer.
func (r *Recorder) SetHandshake(key string, t time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.handshakes == nil {
		r.handshakes = map[string]time.Time{}
	}
	r.handshakes[key] = t
}

// AddRx adds received bytes for a peer, as traffic from it would.
func (r *Recorder) AddRx(key string, n int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rx == nil {
		r.rx = map[string]int64{}
	}
	r.rx[key] += n
}

// Last returns the most recent applied config.
func (r *Recorder) Last() (Config, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.Applied) == 0 {
		return Config{}, false
	}
	return r.Applied[len(r.Applied)-1], true
}

// RemoveOwned records nothing: the fake has no stale interfaces.
func (r *Recorder) RemoveOwned(string, string) (bool, error) { return false, nil }

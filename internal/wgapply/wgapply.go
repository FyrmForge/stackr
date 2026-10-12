// Package wgapply is the node side of the mesh: it makes the local wg
// interface match a Config and moves peers to their next endpoint candidate
// when the handshake goes stale. The kernel work sits behind Applier so the
// logic runs without root; Recorder is the fake for fake mode and tests.
package wgapply

import (
	"context"
	"net/netip"
	"sync"
	"time"

	"github.com/FyrmForge/stackr/internal/mesh"
)

// Config is the full desired state of the node's mesh interface.
type Config struct {
	Iface      string // e.g. wg0
	PrivateKey string
	ListenPort int
	Address    netip.Prefix // node transit address with the transit mask
	Peers      []mesh.Peer  // Candidates[0] is the endpoint to start with
}

// Applier is the kernel boundary.
type Applier interface {
	// Apply creates the interface if missing, sets address, peers and one
	// route per peer range, and removes peers and routes not in cfg.
	Apply(cfg Config) error
	// Handshakes returns the last handshake time per public key; the zero
	// time means never.
	Handshakes(iface string) (map[string]time.Time, error)
	// SetEndpoint moves one existing peer to another endpoint.
	SetEndpoint(iface, publicKey string, ep netip.AddrPort) error
}

// StaleAfter is how old a handshake may get before the next candidate is
// tried. Kernel WireGuard rehandshakes about every 2 minutes.
const StaleAfter = 180 * time.Second

// Failover walks each peer through its candidate list.
type Failover struct {
	A     Applier
	Iface string
	Stale time.Duration    // zero means StaleAfter
	Now   func() time.Time // zero means time.Now
	mu    sync.Mutex
	peers map[string]*state // by public key
}

type state struct {
	cands []netip.AddrPort
	idx   int
	since time.Time // when the current candidate was set
}

func (f *Failover) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

// Reset takes a new peer set. A peer whose candidates did not change keeps
// its position, so a repeated push does not restart the walk. A new or
// changed one starts over on its first candidate, the best path: the kernel
// keeps an existing peer's endpoint, so without this a peer that gained a
// private path (or a worker that restarted) would stay where it was.
func (f *Failover) Reset(peers []mesh.Peer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	next := map[string]*state{}
	for _, p := range peers {
		if old, ok := f.peers[p.PublicKey]; ok && sameCands(old.cands, p.Candidates) {
			next[p.PublicKey] = old
			continue
		}
		next[p.PublicKey] = &state{cands: p.Candidates, since: f.now()}
		if len(p.Candidates) > 0 {
			_ = f.A.SetEndpoint(f.Iface, p.PublicKey, p.Candidates[0]) // best effort: the walk moves on if it does not hold
		}
	}
	f.peers = next
}

func sameCands(a, b []netip.AddrPort) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Tick checks every peer once and moves stale ones to the next candidate,
// wrapping after the last. A peer gets a full stale window on each candidate
// before the next move. It returns the keys that moved.
func (f *Failover) Tick() ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	hs, err := f.A.Handshakes(f.Iface)
	if err != nil {
		return nil, err
	}
	stale := f.Stale
	if stale == 0 {
		stale = StaleAfter
	}
	now := f.now()
	var moved []string
	for key, st := range f.peers {
		if len(st.cands) < 2 {
			continue
		}
		last := hs[key]
		if now.Sub(st.since) < stale || (!last.IsZero() && now.Sub(last) < stale) {
			continue
		}
		next := (st.idx + 1) % len(st.cands)
		if err := f.A.SetEndpoint(f.Iface, key, st.cands[next]); err != nil {
			return moved, err
		}
		st.idx, st.since = next, now
		moved = append(moved, key)
	}
	return moved, nil
}

// Run ticks until ctx ends. Errors are handed to onErr (may be nil).
func (f *Failover) Run(ctx context.Context, every time.Duration, onErr func(error)) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := f.Tick(); err != nil && onErr != nil {
				onErr(err)
			}
		}
	}
}

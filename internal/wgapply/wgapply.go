// Package wgapply is the node side of the mesh: it makes the local wg
// interface match a Config and moves peers to their next endpoint candidate
// when the handshake goes stale. The kernel work sits behind Applier so the
// logic runs without root; Recorder is the fake for fake mode and tests.
package wgapply

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/FyrmForge/stackr/internal/mesh"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// Config is the full desired state of the node's mesh interface.
type Config struct {
	Iface      string // e.g. stackr0
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
	// Stats returns each peer's last handshake (zero: never), bytes
	// received and current endpoint, by public key.
	Stats(iface string) (map[string]PeerStat, error)
	// SetEndpoint moves one existing peer to another endpoint.
	SetEndpoint(iface, publicKey string, ep netip.AddrPort) error
	// Seen returns, per public key, where a peer's packets come from, for
	// peers with a handshake younger than StaleAfter: a peer behind a NAT
	// shows up as the NAT's public address.
	Seen(iface string) (map[string]netip.AddrPort, error)
	// RemoveOwned deletes iface only when it is a WireGuard interface
	// holding privateKey (one stackr made); anything else, or no such
	// interface, is left alone and is not an error.
	RemoveOwned(iface, privateKey string) (bool, error)
}

// PeerStat is what the kernel reports for one peer.
type PeerStat struct {
	Handshake time.Time
	Rx        int64
	Tx        int64
	Endpoint  netip.AddrPort
}

// StaleAfter is the age past which a handshake no longer proves a path:
// kernel WireGuard rehandshakes about every 2 minutes.
const StaleAfter = 180 * time.Second

// Failover timings. Every peer runs a 25 s persistent keepalive, so a live
// path delivers something well inside Probe.
const (
	// Probe is how long a candidate just switched to gets to show a
	// handshake or traffic from the peer at that endpoint.
	Probe = 30 * time.Second
	// Dead is how long a settled peer may go without receiving anything
	// before its path counts as down (two keepalives missed).
	Dead = 60 * time.Second
	// Climb is how often a peer settled on a later candidate (the relay)
	// retries the earlier, better ones.
	Climb = 5 * time.Minute
)

// roundPause is the extra wait before the next round once every candidate
// failed: 30 s, then 2 min, then 5 min from there on; success resets it.
var roundPause = []time.Duration{30 * time.Second, 2 * time.Minute, 5 * time.Minute}

// Failover walks each peer through its candidate list: Probe per candidate,
// a growing pause between failed rounds, and a climb back to better
// candidates every Climb while it sits on a later one.
type Failover struct {
	A     Applier
	Iface string
	Now   func() time.Time // zero means time.Now
	mu    sync.Mutex
	peers map[string]*state // by public key
}

type state struct {
	node    string // the peer's node, so a rotated key keeps its place
	cands   []netip.AddrPort
	idx     int
	since   time.Time // when the current candidate was set
	probing bool      // the current candidate has not proven itself yet
	home    int       // while climbing: the working candidate to return to; -1 otherwise
	rounds  int       // full rounds failed in a row
	rx      int64     // bytes received at the last tick
	rxAt    time.Time // when rx last grew
	climbAt time.Time // next climb, for a peer settled past its first candidate
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
	byNode := map[string]*state{}
	for _, st := range f.peers {
		if st.node != "" {
			byNode[st.node] = st
		}
	}
	for _, p := range peers {
		if old, ok := f.peers[p.PublicKey]; ok && sameCands(old.cands, p.Candidates) {
			next[p.PublicKey] = old
			continue
		}
		now := f.now()
		// A node that rotated its key is a new peer to the kernel: it keeps
		// the candidate it was on, so a NAT'd pair does not walk from the top.
		if old, ok := byNode[p.NodeID]; ok && p.NodeID != "" && sameCands(old.cands, p.Candidates) {
			st := *old
			st.since, st.rxAt, st.probing, st.rx = now, now, true, 0
			next[p.PublicKey] = &st
			if st.idx < len(p.Candidates) { // a node reached only through a hop has none
				_ = f.A.SetEndpoint(f.Iface, p.PublicKey, p.Candidates[st.idx])
			}
			continue
		}
		next[p.PublicKey] = &state{node: p.NodeID, cands: p.Candidates, since: now, rxAt: now, probing: true, home: -1}
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

// window is how long the current candidate gets: Probe, plus the round
// pause when a failed round has just wrapped back to the first.
func (st *state) window() time.Duration {
	if st.idx != 0 || st.rounds == 0 {
		return Probe
	}
	return Probe + roundPause[min(st.rounds, len(roundPause))-1]
}

// Tick checks every peer once and moves it when its path failed or a climb
// is due. It returns the keys that moved.
func (f *Failover) Tick() ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	stats, err := f.A.Stats(f.Iface)
	if err != nil {
		return nil, err
	}
	now := f.now()
	var moved []string
	move := func(key string, st *state, i int) error {
		if err := f.A.SetEndpoint(f.Iface, key, st.cands[i]); err != nil {
			return err
		}
		st.idx, st.since, st.probing = i, now, true
		moved = append(moved, key)
		return nil
	}
	for key, st := range f.peers {
		if len(st.cands) < 2 {
			continue
		}
		ps := stats[key]
		if ps.Rx != st.rx {
			st.rx, st.rxAt = ps.Rx, now
		}
		if st.probing {
			// Proven by a handshake since the switch, or by traffic from the
			// peer at this endpoint (a live session moved to a new endpoint
			// only rehandshakes after ~2 min; the kernel roams the endpoint
			// to wherever the peer's packets really come from).
			if ps.Handshake.After(st.since) || st.rxAt.After(st.since) && ps.Endpoint == st.cands[st.idx] {
				st.probing, st.rounds, st.home = false, 0, -1
				st.climbAt = now.Add(Climb)
				continue
			}
			if now.Sub(st.since) < st.window() {
				continue
			}
			if st.home >= 0 { // a climb candidate failed: next better one, or back home
				next := st.idx + 1
				if next < st.home {
					if err := move(key, st, next); err != nil {
						return moved, err
					}
					continue
				}
				home := st.home
				if err := move(key, st, home); err != nil {
					return moved, err
				}
				st.probing, st.home, st.rxAt = false, -1, now // it worked a moment ago
				st.climbAt = now.Add(Climb)
				continue
			}
			next := (st.idx + 1) % len(st.cands)
			if next == 0 {
				st.rounds++
			}
			if err := move(key, st, next); err != nil {
				return moved, err
			}
			continue
		}
		// Settled.
		if now.Sub(st.rxAt) >= Dead {
			next := (st.idx + 1) % len(st.cands)
			if next == 0 {
				st.rounds++
			}
			if err := move(key, st, next); err != nil {
				return moved, err
			}
			continue
		}
		if st.idx > 0 && !now.Before(st.climbAt) {
			st.home = st.idx
			if err := move(key, st, 0); err != nil {
				return moved, err
			}
		}
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

// parseSet reads the argv of `wg set <iface> ...` that the exit link plans
// emit (listen-port, private-key <file>, one peer with endpoint, allowed-ips,
// persistent-keepalive) into a wgctrl config. The private key file is read
// here; it is never echoed in errors.
func parseSet(args []string, readFile func(string) ([]byte, error)) (string, wgtypes.Config, error) {
	var cfg wgtypes.Config
	if len(args) < 1 {
		return "", cfg, fmt.Errorf("wg set: no interface")
	}
	iface, rest := args[0], args[1:]
	var peer *wgtypes.PeerConfig
	for i := 0; i < len(rest); i++ {
		if i+1 >= len(rest) {
			return "", cfg, fmt.Errorf("wg set: %s needs a value", rest[i])
		}
		k, v := rest[i], rest[i+1]
		i++
		switch k {
		case "listen-port":
			p, err := strconv.Atoi(v)
			if err != nil {
				return "", cfg, fmt.Errorf("wg set: listen-port %q", v)
			}
			cfg.ListenPort = &p
		case "private-key":
			b, err := readFile(v)
			if err != nil {
				return "", cfg, fmt.Errorf("wg set: private key %s: %w", v, err)
			}
			key, err := wgtypes.ParseKey(strings.TrimSpace(string(b)))
			if err != nil {
				return "", cfg, fmt.Errorf("wg set: private key %s is not a key", v)
			}
			cfg.PrivateKey = &key
		case "peer":
			pk, err := wgtypes.ParseKey(v)
			if err != nil {
				return "", cfg, fmt.Errorf("wg set: peer key %q", v)
			}
			cfg.Peers = append(cfg.Peers, wgtypes.PeerConfig{PublicKey: pk, ReplaceAllowedIPs: true})
			peer = &cfg.Peers[len(cfg.Peers)-1]
		case "endpoint", "allowed-ips", "persistent-keepalive":
			if peer == nil {
				return "", cfg, fmt.Errorf("wg set: %s before peer", k)
			}
			switch k {
			case "endpoint":
				ep, err := net.ResolveUDPAddr("udp", v)
				if err != nil {
					return "", cfg, fmt.Errorf("wg set: endpoint %q: %w", v, err)
				}
				peer.Endpoint = ep
			case "allowed-ips":
				for _, s := range strings.Split(v, ",") {
					_, n, err := net.ParseCIDR(strings.TrimSpace(s))
					if err != nil {
						return "", cfg, fmt.Errorf("wg set: allowed-ips %q", s)
					}
					peer.AllowedIPs = append(peer.AllowedIPs, *n)
				}
			case "persistent-keepalive":
				sec, err := strconv.Atoi(v)
				if err != nil {
					return "", cfg, fmt.Errorf("wg set: persistent-keepalive %q", v)
				}
				d := time.Duration(sec) * time.Second
				peer.PersistentKeepaliveInterval = &d
			}
		default:
			return "", cfg, fmt.Errorf("wg set: unsupported %q", k)
		}
	}
	return iface, cfg, nil
}

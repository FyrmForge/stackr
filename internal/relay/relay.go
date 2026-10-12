// Package relay is a UDP forwarder for node pairs that cannot reach each
// other directly. One port per pair; the controller supplies the two public
// addresses. The relay only moves WireGuard ciphertext and never sees keys.
//
// A side's port is learned only from a WireGuard handshake initiation or
// response (type 1 or 2) sent from that side's IP, so junk sent from the same
// NAT cannot steal the mapping. Every other packet is forwarded only from a
// learned endpoint to the other learned endpoint. Packets from other IPs are
// dropped.
package relay

import (
	"errors"
	"net"
	"net/netip"
	"sync"
)

// Pair is one relayed node pair.
type Pair struct {
	Port int // 0 picks a free port (tests)
	A, B netip.Addr
}

// Relay forwards one pair on one UDP socket.
type Relay struct {
	conn *net.UDPConn
	ips  [2]netip.Addr

	mu   sync.Mutex
	seen [2]netip.AddrPort // learned endpoints, zero until a handshake
	done chan struct{}
}

// Start listens on all addresses and serves until Close.
func Start(p Pair) (*Relay, error) {
	if !p.A.IsValid() || !p.B.IsValid() || p.A == p.B {
		return nil, errors.New("relay: pair needs two distinct addresses")
	}
	c, err := net.ListenUDP("udp", &net.UDPAddr{Port: p.Port})
	if err != nil {
		return nil, err
	}
	r := &Relay{conn: c, ips: [2]netip.Addr{p.A.Unmap(), p.B.Unmap()}, done: make(chan struct{})}
	go r.serve()
	return r, nil
}

// Addr is the socket's local address.
func (r *Relay) Addr() *net.UDPAddr { return r.conn.LocalAddr().(*net.UDPAddr) }

// Close stops the relay and waits for the serve loop.
func (r *Relay) Close() error {
	err := r.conn.Close()
	<-r.done
	return err
}

// isHandshake reports a WireGuard initiation (type 1, 148 bytes) or response
// (type 2, 92 bytes): little-endian type word with zero reserved bytes.
func isHandshake(b []byte) bool {
	if len(b) < 4 || b[1] != 0 || b[2] != 0 || b[3] != 0 {
		return false
	}
	return (b[0] == 1 && len(b) == 148) || (b[0] == 2 && len(b) == 92)
}

func (r *Relay) serve() {
	defer close(r.done)
	buf := make([]byte, 65535)
	for {
		n, src, err := r.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			return
		}
		r.handle(buf[:n], netip.AddrPortFrom(src.Addr().Unmap(), src.Port()))
	}
}

func (r *Relay) handle(data []byte, src netip.AddrPort) {
	side := -1
	for i, ip := range r.ips {
		if src.Addr() == ip {
			side = i
		}
	}
	if side < 0 {
		return
	}
	r.mu.Lock()
	if isHandshake(data) {
		r.seen[side] = src
	}
	from, to := r.seen[side], r.seen[1-side]
	r.mu.Unlock()
	if from != src || !to.IsValid() {
		return
	}
	r.conn.WriteToUDPAddrPort(data, to)
}

// Manager keeps the set of running relays equal to the pairs the controller
// last sent.
type Manager struct {
	mu     sync.Mutex
	relays map[Pair]*Relay
}

// Sync starts relays for new pairs and stops those no longer listed. A pair
// that fails to start is reported and skipped; the rest still run.
func (m *Manager) Sync(pairs []Pair) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.relays == nil {
		m.relays = map[Pair]*Relay{}
	}
	want := map[Pair]bool{}
	var errs []error
	for _, p := range pairs {
		want[p] = true
		if m.relays[p] != nil {
			continue
		}
		r, err := Start(p)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		m.relays[p] = r
	}
	for p, r := range m.relays {
		if !want[p] {
			r.Close()
			delete(m.relays, p)
		}
	}
	return errors.Join(errs...)
}

// Close stops every relay.
func (m *Manager) Close() { m.Sync(nil) }

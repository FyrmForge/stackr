package domain

import "net/netip"

// Requested is how many Sync calls have registered, for the coalescing test.
func (s *Syncer) Requested() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.want
}

// SetPublicAddr swaps the forward-auth public-address check; the returned func
// puts the old one back.
func SetPublicAddr(f func(string) (netip.Addr, bool)) func() {
	old := publicAddr
	publicAddr = f
	return func() { publicAddr = old }
}

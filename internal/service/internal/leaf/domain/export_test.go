package domain

// Requested is how many Sync calls have registered, for the coalescing test.
func (s *Syncer) Requested() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.want
}

// SetPublicURL swaps the forward-auth public-address check; the returned func
// puts the old one back.
func SetPublicURL(f func(string) bool) func() {
	old := publicURL
	publicURL = f
	return func() { publicURL = old }
}

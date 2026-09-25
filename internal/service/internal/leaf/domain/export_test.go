package domain

// Requested is how many Sync calls have registered, for the coalescing test.
func (s *Syncer) Requested() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.want
}

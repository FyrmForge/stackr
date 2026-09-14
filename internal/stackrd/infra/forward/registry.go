// Package forward tracks live `stackr forward` tunnels in memory. A dropped
// websocket IS a closed forward, so there is nothing to persist and nothing to
// reconcile after a restart, the canvas cards themselves come from the relay
// containers; this registry only answers "who".
package forward

import (
	"sync"
	"time"
)

// Session is one open forward: a user with a tunnel into a tile's port.
type Session struct {
	TileID     string
	Port       int
	UserID     string
	UserName   string
	AvatarPath string // storage path, "" = initials fallback
	Role       string // server role, for the card's hover line
	StartedAt  time.Time
}

// Registry is a mutex-guarded session set. The zero value is not usable; New.
type Registry struct {
	mu   sync.Mutex
	next int
	m    map[int]Session
}

func New() *Registry { return &Registry{m: map[int]Session{}} }

// Add records a session and returns its remover, for the same defer that
// writes the close audit line, so the two can never disagree.
func (r *Registry) Add(s Session) (remove func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := r.next
	r.next++
	r.m[id] = s
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		delete(r.m, id)
	}
}

// Sessions returns a snapshot of every open session.
func (r *Registry) Sessions() []Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Session, 0, len(r.m))
	for _, s := range r.m {
		out = append(out, s)
	}
	return out
}

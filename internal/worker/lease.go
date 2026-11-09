package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// The lease: a Postgres primary that has a standby stops itself when the
// panel has been silent for Seconds. The panel is the only thing that may
// promote the standby, and it waits out the lease on a node it cannot reach
// before it does, so a primary cut off from the panel is down by the time its
// standby is promoted: never two writers. The panel names the containers the
// lease binds (PUT /v1/lease, every few seconds, which is also the contact
// that keeps the lease alive); the list is kept in the state dir so a worker
// that restarts while the panel is away still stops them. An empty list, or
// no lease, binds nothing: single instances never get one.

// LeasePath is PUT: the containers the lease binds, and its length.
const LeasePath = "/v1/lease"

// Lease is the panel's PUT /v1/lease body.
type Lease struct {
	Seconds    int      `json:"seconds"`
	Containers []string `json:"containers"`
}

type leaseState struct {
	mu    sync.Mutex
	lease Lease
	// stopContainers stops the lease's containers; nil = through the local
	// Docker socket. Tests swap it.
	stop func(ctx context.Context, ids []string)
}

func (s *Server) leasePath() string { return filepath.Join(s.cfg.StateDir, "lease.json") }

func (s *Server) putLease(w http.ResponseWriter, r *http.Request) {
	b, ok := readBody(w, r)
	if !ok {
		return
	}
	var l Lease
	if err := json.Unmarshal(b, &l); err != nil || l.Seconds < 0 {
		fail(w, 400, fmt.Errorf("lease: bad body"))
		return
	}
	s.lease.mu.Lock()
	defer s.lease.mu.Unlock()
	if err := writeAtomic(s.leasePath(), b); err != nil {
		fail(w, 500, err)
		return
	}
	s.lease.lease = l
	w.WriteHeader(204)
}

// loadLease reads the lease kept by an earlier run.
func (s *Server) loadLease() {
	b, err := os.ReadFile(s.leasePath())
	if err != nil {
		return
	}
	var l Lease
	if json.Unmarshal(b, &l) == nil {
		s.lease.mu.Lock()
		s.lease.lease = l
		s.lease.mu.Unlock()
	}
}

// leaseLoop checks the lease every tick until the worker stops.
func (s *Server) leaseLoop(every time.Duration) {
	s.loadLease()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case now := <-t.C:
			s.leaseStep(now)
		}
	}
}

// leaseStep stops the lease's containers when the controller has been silent
// for the lease. Once stopped they are not asked again until the panel is
// back and names them again (a contact resets the clock, and the panel puts
// the list again).
func (s *Server) leaseStep(now time.Time) {
	s.lease.mu.Lock()
	l := s.lease.lease
	stop := s.lease.stop
	s.lease.mu.Unlock()
	if l.Seconds <= 0 || len(l.Containers) == 0 {
		return
	}
	if now.Sub(time.Unix(0, s.lastCtl.Load())) < time.Duration(l.Seconds)*time.Second {
		return
	}
	slog.Warn("worker: the panel is silent, stopping the primaries bound to the lease", "containers", len(l.Containers), "seconds", l.Seconds)
	if stop == nil {
		stop = s.stopViaDocker
	}
	stop(s.ctx, l.Containers)
	s.lease.mu.Lock()
	s.lease.lease.Containers = nil // stopped: the panel names them again when it is back
	s.lease.mu.Unlock()
}

// stopViaDocker stops each container through the local socket: SIGINT first
// (a fast shutdown, so a standby that is promoted later can follow the
// timeline), then a stop with a long grace.
func (s *Server) stopViaDocker(ctx context.Context, ids []string) {
	hc := &http.Client{Timeout: 3 * time.Minute, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", s.cfg.DockerSock)
		},
	}}
	for _, id := range ids {
		for _, p := range []string{"/containers/" + url.PathEscape(id) + "/kill?signal=SIGINT", "/containers/" + url.PathEscape(id) + "/stop?t=120"} {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://local"+p, nil)
			if err != nil {
				continue
			}
			resp, err := hc.Do(req)
			if err != nil {
				slog.Warn("worker: lease stop", "container", id, "err", err)
				break
			}
			_ = resp.Body.Close()
		}
	}
}

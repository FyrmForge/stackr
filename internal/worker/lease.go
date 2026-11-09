package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
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
//
// Clocks. Silence is measured on the process's monotonic clock (lastCtl is an
// offset from born), so a wall-clock jump neither stops a primary early nor
// keeps it alive. A binding found on disk at start is expired: nothing in
// this process has seen the panel, so the bound containers are stopped (and
// verified stopped) before the worker serves, unless the panel's PUT renews
// it first. A stop that fails or is not seen to finish keeps the container
// bound and is retried every tick; a binding is only dropped once its
// container is verified not running.

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
	// restored: the lease came off disk and the panel has not renewed it
	// since this process started; it counts as expired.
	restored bool
	// stop stops the lease's containers and returns the ones not verified
	// stopped; nil = through the local Docker socket. Tests swap it.
	stop func(ctx context.Context, ids []string) (failed []string)
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
	s.lease.lease, s.lease.restored = l, false
	w.WriteHeader(204)
}

// loadLease reads the lease kept by an earlier run; it is expired until the
// panel renews it.
func (s *Server) loadLease() {
	b, err := os.ReadFile(s.leasePath())
	if err != nil {
		return
	}
	var l Lease
	if json.Unmarshal(b, &l) == nil {
		s.lease.mu.Lock()
		s.lease.lease, s.lease.restored = l, len(l.Containers) > 0
		s.lease.mu.Unlock()
	}
}

// leaseBoot is run before the worker serves: a binding left by an earlier run
// is stopped now.
func (s *Server) leaseBoot() {
	s.loadLease()
	s.leaseStep(time.Now())
}

// leaseLoop checks the lease every tick until the worker stops.
func (s *Server) leaseLoop(every time.Duration) {
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
// for the lease (or the lease came off disk and is not renewed). A container
// leaves the list only once it is verified stopped; the rest are asked again
// next tick.
func (s *Server) leaseStep(now time.Time) {
	s.lease.mu.Lock()
	l, restored, stop := s.lease.lease, s.lease.restored, s.lease.stop
	s.lease.mu.Unlock()
	if len(l.Containers) == 0 {
		return
	}
	if !restored && (l.Seconds <= 0 || s.ctlAge(now) < time.Duration(l.Seconds)*time.Second) {
		return
	}
	slog.Warn("worker: the panel is silent, stopping the primaries bound to the lease", "containers", len(l.Containers), "seconds", l.Seconds)
	if stop == nil {
		stop = s.stopViaDocker
	}
	failed := stop(s.ctx, l.Containers)
	s.lease.mu.Lock()
	defer s.lease.mu.Unlock()
	left := s.lease.lease.Containers[:0:0]
	for _, id := range s.lease.lease.Containers {
		if !slices.Contains(l.Containers, id) || slices.Contains(failed, id) {
			left = append(left, id) // not asked about, or not verified stopped
		}
	}
	s.lease.lease.Containers = left
	if b, err := json.Marshal(s.lease.lease); err == nil {
		_ = writeAtomic(s.leasePath(), b) // a restart must not stop what the panel starts again
	}
}

// dockerPoll is how often a stopped container is inspected.
var dockerPoll = 500 * time.Millisecond

// stopViaDocker stops each container through the local socket and returns the
// ones it could not verify stopped.
func (s *Server) stopViaDocker(ctx context.Context, ids []string) (failed []string) {
	hc := &http.Client{Timeout: 3 * time.Minute, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", s.cfg.DockerSock)
		},
	}}
	for _, id := range ids {
		if err := dockerStop(ctx, hc, id); err != nil {
			slog.Warn("worker: lease stop", "container", id, "err", err)
			failed = append(failed, id)
		}
	}
	return failed
}

// dockerStop is SIGINT first (a fast shutdown, so a standby that is promoted
// later can follow the timeline), then a stop with a long grace, then an
// inspect until the container is not running. 404 (gone) and 409 on the kill
// (not running) are fine; any other status is a failure.
func dockerStop(ctx context.Context, hc *http.Client, id string) error {
	base := "http://local/containers/" + url.PathEscape(id)
	call := func(method, p string, ok ...int) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, method, base+p, nil)
		if err != nil {
			return nil, err
		}
		resp, err := hc.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode/100 != 2 && !slices.Contains(ok, resp.StatusCode) {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("docker %s %s: status %d", method, p, resp.StatusCode)
		}
		return resp, nil
	}
	for _, c := range []struct {
		p  string
		ok []int
	}{{"/kill?signal=SIGINT", []int{404, 409}}, {"/stop?t=120", []int{304, 404}}} {
		resp, err := call(http.MethodPost, c.p, c.ok...)
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
	}
	for deadline := time.Now().Add(time.Minute); ; {
		resp, err := call(http.MethodGet, "/json", 404)
		if err != nil {
			return err
		}
		var st struct {
			State struct{ Running, Restarting bool }
		}
		gone := resp.StatusCode == 404
		err = json.NewDecoder(resp.Body).Decode(&st)
		_ = resp.Body.Close()
		if gone || (err == nil && !st.State.Running && !st.State.Restarting) {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("still running")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(dockerPoll):
		}
	}
}

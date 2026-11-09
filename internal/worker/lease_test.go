package worker

import (
	"context"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

func leaseServer(t *testing.T, body string) (*Server, *[]string) {
	t.Helper()
	s := New(Config{StateDir: t.TempDir()})
	t.Cleanup(s.Stop)
	var stopped []string
	s.lease.stop = func(_ context.Context, ids []string) { stopped = append(stopped, ids...) }
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("PUT", LeasePath, strings.NewReader(body)))
	if w.Code != 204 {
		t.Fatalf("PUT lease = %d %s", w.Code, w.Body)
	}
	return s, &stopped
}

// A primary bound to the lease is stopped once the panel has been silent for
// the lease, and not before; once stopped it is not stopped again.
func TestLeaseStopsBoundPrimaryWhenPanelSilent(t *testing.T) {
	t.Parallel()
	s, stopped := leaseServer(t, `{"seconds":30,"containers":["pg1"]}`)
	s.leaseStep(time.Now().Add(20 * time.Second)) // the panel's last word was 20 s ago
	if len(*stopped) != 0 {
		t.Fatalf("stopped %v inside the lease", *stopped)
	}
	s.leaseStep(time.Now().Add(31 * time.Second))
	if !slices.Equal(*stopped, []string{"pg1"}) {
		t.Fatalf("stopped %v after the lease, want [pg1]", *stopped)
	}
	s.leaseStep(time.Now().Add(90 * time.Second))
	if len(*stopped) != 1 {
		t.Errorf("stopped again: %v", *stopped)
	}
}

// Contact from the panel keeps the lease alive, and an instance with no
// standby (the panel binds nothing) is never stopped however silent it is.
func TestLeaseNeverStopsWithoutContainersOrWhileInContact(t *testing.T) {
	t.Parallel()
	s, stopped := leaseServer(t, `{"seconds":30,"containers":[]}`)
	s.leaseStep(time.Now().Add(time.Hour))
	if len(*stopped) != 0 {
		t.Errorf("a lease that binds nothing stopped %v", *stopped)
	}
	s2, stopped2 := leaseServer(t, `{"seconds":30,"containers":["pg1"]}`)
	s2.lastCtl.Store(time.Now().Add(45 * time.Second).UnixNano()) // the panel called 'just now' (as of the check)
	s2.leaseStep(time.Now().Add(60 * time.Second))
	if len(*stopped2) != 0 {
		t.Errorf("stopped %v with the panel in contact", *stopped2)
	}
}

// The lease survives a restart of the worker while the panel is away.
func TestLeaseSurvivesRestart(t *testing.T) {
	t.Parallel()
	s, _ := leaseServer(t, `{"seconds":30,"containers":["pg1"]}`)
	s2 := New(Config{StateDir: s.cfg.StateDir})
	t.Cleanup(s2.Stop)
	s2.loadLease()
	if got := s2.lease.lease; got.Seconds != 30 || !slices.Equal(got.Containers, []string{"pg1"}) {
		t.Errorf("lease after a restart = %+v", got)
	}
}

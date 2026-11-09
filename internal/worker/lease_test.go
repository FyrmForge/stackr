package worker

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	s.lease.stop = func(_ context.Context, ids []string) []string { stopped = append(stopped, ids...); return nil }
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
	s2.lastCtl.Store(int64(time.Now().Add(45 * time.Second).Sub(s2.born))) // the panel called 'just now' (as of the check)
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

// A stop that fails keeps the container bound and is asked again; only a
// verified stop drops it.
func TestLeaseKeepsFailedStopBound(t *testing.T) {
	t.Parallel()
	s, _ := leaseServer(t, `{"seconds":30,"containers":["a","b"]}`)
	var calls [][]string
	fails := true
	s.lease.stop = func(_ context.Context, ids []string) []string {
		calls = append(calls, slices.Clone(ids))
		if fails {
			return []string{"b"}
		}
		return nil
	}
	s.leaseStep(time.Now().Add(31 * time.Second))
	if got := s.lease.lease.Containers; !slices.Equal(got, []string{"b"}) {
		t.Fatalf("after a partial stop the lease binds %v, want [b]", got)
	}
	fails = false
	s.leaseStep(time.Now().Add(33 * time.Second))
	if len(s.lease.lease.Containers) != 0 || !slices.Equal(calls[1], []string{"b"}) {
		t.Errorf("retry: calls %v, lease %v", calls, s.lease.lease.Containers)
	}
	b, _ := os.ReadFile(s.leasePath())
	if strings.Contains(string(b), `"b"`) {
		t.Errorf("the lease file still binds b: %s", b)
	}
}

// A binding found on disk at start is expired: stopped at once, with no
// grace, unless the panel renews it first.
func TestLeaseBootStopsRestoredBinding(t *testing.T) {
	t.Parallel()
	s, _ := leaseServer(t, `{"seconds":30,"containers":["pg1"]}`)
	run := func(renew bool) []string {
		s2 := New(Config{StateDir: s.cfg.StateDir})
		t.Cleanup(s2.Stop)
		var stopped []string
		s2.lease.stop = func(_ context.Context, ids []string) []string { stopped = append(stopped, ids...); return nil }
		s2.loadLease()
		if renew {
			w := httptest.NewRecorder()
			s2.Handler().ServeHTTP(w, httptest.NewRequest("PUT", LeasePath, strings.NewReader(`{"seconds":30,"containers":["pg1"]}`)))
		}
		s2.leaseStep(time.Now()) // no clock has run out
		return stopped
	}
	if got := run(false); !slices.Equal(got, []string{"pg1"}) {
		t.Errorf("restored binding stopped %v, want [pg1]", got)
	}
	if got := run(true); len(got) != 0 {
		t.Errorf("renewed binding stopped %v", got)
	}
}

// The docker stop checks statuses and inspects until the container is not
// running; a failing socket or a container that stays up is a failure.
func TestDockerStopVerifies(t *testing.T) {
	t.Parallel()
	old := dockerPoll
	dockerPoll = time.Millisecond
	t.Cleanup(func() { dockerPoll = old })
	run := func(h http.HandlerFunc) []string {
		sock := filepath.Join(t.TempDir(), "d.sock")
		ln, err := net.Listen("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		srv := &http.Server{Handler: h}
		go func() { _ = srv.Serve(ln) }()
		t.Cleanup(func() { _ = srv.Close() })
		s := New(Config{StateDir: t.TempDir(), DockerSock: sock})
		t.Cleanup(s.Stop)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		return s.stopViaDocker(ctx, []string{"pg1"})
	}
	polls := 0
	ok := run(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			polls++
			_, _ = w.Write([]byte(`{"State":{"Running":` + map[bool]string{true: "true", false: "false"}[polls < 3] + `}}`))
			return
		}
		w.WriteHeader(204)
	})
	if len(ok) != 0 || polls < 3 {
		t.Errorf("a stop that completes failed %v (polls %d)", ok, polls)
	}
	if got := run(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }); !slices.Equal(got, []string{"pg1"}) {
		t.Errorf("a 500 was taken as stopped: %v", got)
	}
	if got := run(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			_, _ = w.Write([]byte(`{"State":{"Running":true}}`))
			return
		}
		w.WriteHeader(204)
	}); !slices.Equal(got, []string{"pg1"}) {
		t.Errorf("a container still running was taken as stopped: %v", got)
	}
}

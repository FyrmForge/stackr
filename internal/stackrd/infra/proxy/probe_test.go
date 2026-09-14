package proxy

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// fakeTraefik answers with whatever status the pointer holds, so a test can
// flip it the way a restart would.
func fakeTraefik(t *testing.T, status *atomic.Int64) *Proxy {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(status.Load()))
	}))
	t.Cleanup(srv.Close)
	return &Proxy{probeURL: srv.URL, probeInterval: time.Millisecond}
}

func TestVerifyLoadedRoute(t *testing.T) {
	for _, status := range []int64{200, 302, 502} {
		var st atomic.Int64
		st.Store(status)
		p := fakeTraefik(t, &st)
		var restarts atomic.Int64
		p.restart = func() error { restarts.Add(1); return nil }
		p.verify("app.example.com")
		assert.Zero(t, restarts.Load(), "status %d is a loaded route", status)
	}
}

func TestVerifyRestartsOnMissingRoute(t *testing.T) {
	var st atomic.Int64
	st.Store(404)
	p := fakeTraefik(t, &st)
	var restarts atomic.Int64
	p.restart = func() error {
		restarts.Add(1)
		st.Store(200) // a restart reloads the dynamic dir
		return nil
	}
	p.verify("app.example.com")
	assert.EqualValues(t, 1, restarts.Load())
}

func TestVerifyRestartsOnce(t *testing.T) {
	var st atomic.Int64
	st.Store(404)
	p := fakeTraefik(t, &st)
	var restarts atomic.Int64
	p.restart = func() error { restarts.Add(1); return nil }
	p.verify("app.example.com")
	assert.EqualValues(t, 1, restarts.Load(), "never loops on a route that stays missing")
}

func TestVerifyUnreachableTraefikDoesNotRestart(t *testing.T) {
	// Nothing listening: the panel is outside docker, or Traefik is being
	// recreated. Neither means the route failed to load.
	p := &Proxy{probeURL: "http://127.0.0.1:1/", probeInterval: time.Millisecond}
	var restarts atomic.Int64
	p.restart = func() error { restarts.Add(1); return nil }
	p.verify("app.example.com")
	assert.Zero(t, restarts.Load())
}

func TestProbeHostWildcard(t *testing.T) {
	assert.Equal(t, "stackr-probe.example.com", probeHost("*.example.com"))
	assert.Equal(t, "app.example.com", probeHost("app.example.com"))
}

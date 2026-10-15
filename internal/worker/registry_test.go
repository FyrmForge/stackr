package worker

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRegistryProxy(t *testing.T) {
	var host string
	var certs int
	front := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, certs = r.Host, len(r.TLS.PeerCertificates)
		w.Header().Set("WWW-Authenticate", `Bearer realm="https://registry.example.com/token",service="stackr"`)
		w.Header().Set("Location", "https://registry.example.com/v2/a/b/c/blobs/uploads/u1?_state=x")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, "no")
	}))
	front.TLS = &tls.Config{ClientAuth: tls.RequireAnyClientCert}
	front.StartTLS()
	defer front.Close()

	// The node's client cert: any self-signed pair does; the front under test
	// only checks that one arrives (the real front verifies the chain).
	cert := front.TLS.Certificates[0]
	h, err := registryProxy(RegistryConfig{
		URL: front.URL, Host: "registry.example.com",
		TLS: &tls.Config{Certificates: []tls.Certificate{cert}, InsecureSkipVerify: true}, // test server cert has no such name
	}, RegistryListen)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://localhost:5000/v2/", nil))
	if host != "registry.example.com" || certs != 1 {
		t.Errorf("front saw host %q, %d client certs", host, certs)
	}
	if rec.Code != 401 || rec.Body.String() != "no" {
		t.Errorf("got %d %q", rec.Code, rec.Body)
	}
	// Docker sends its pass only to the host string it used: keep it.
	if got := rec.Header().Get("Location"); got != "http://localhost:5000/v2/a/b/c/blobs/uploads/u1?_state=x" {
		t.Errorf("upload location = %s", got)
	}

	// Nothing but the registry API goes through: the proxy carries the
	// node's cert, and a host-network tile shares this loopback.
	for _, p := range []string{"/token?scope=repository:acme/shop/api:pull", "/stackr/v1/cert/renew", "/v2/../stackr/v1/cert/renew"} {
		host = ""
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "http://localhost:5000"+p, nil))
		if rec.Code != 404 || host != "" {
			t.Errorf("%s: got %d, front saw %q", p, rec.Code, host)
		}
	}
}

// A port someone else holds is reported in Facts, not just logged.
func TestRegistryBindFailureReported(t *testing.T) {
	squat, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = squat.Close() }()
	s := New(Config{StateDir: t.TempDir()})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.runRegistry(ctx, RegistryConfig{URL: "https://registry.example.com", Host: "registry.example.com", TLS: &tls.Config{}, Listen: squat.Addr().String()})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if f := s.Facts(ctx); f.RegistryError != "" {
			if *f.RegistryUp {
				t.Fatal("up with the port taken")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the bind failure never showed in Facts")
}

// Up only while the proxy holds the port.
func TestRegistryUpWhileBound(t *testing.T) {
	s := New(Config{StateDir: t.TempDir()})
	if *s.Facts(context.Background()).RegistryUp {
		t.Fatal("up before it started")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.runRegistry(ctx, RegistryConfig{URL: "https://registry.example.com", Host: "registry.example.com", TLS: &tls.Config{}, Listen: "127.0.0.1:0"})
		close(done)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for !*s.Facts(ctx).RegistryUp {
		if time.Now().After(deadline) {
			t.Fatal("never up")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if *s.Facts(context.Background()).RegistryUp {
		t.Fatal("still up after it stopped")
	}
}

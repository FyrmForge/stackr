package worker

import (
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRegistryProxy(t *testing.T) {
	var host string
	var certs int
	front := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, certs = r.Host, len(r.TLS.PeerCertificates)
		w.Header().Set("WWW-Authenticate", `Bearer realm="https://registry.example.com/token",service="stackr"`)
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
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://127.0.0.1:5000/v2/", nil))
	if host != "registry.example.com" || certs != 1 {
		t.Errorf("front saw host %q, %d client certs", host, certs)
	}
	if rec.Code != 401 || rec.Body.String() != "no" {
		t.Errorf("got %d %q", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != `Bearer realm="http://127.0.0.1:5000/token",service="stackr"` {
		t.Errorf("challenge = %s", got)
	}
}

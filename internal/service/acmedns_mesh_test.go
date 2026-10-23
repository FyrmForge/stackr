package service

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/libdns/libdns"

	"github.com/FyrmForge/stackr/internal/worker"
)

// An ingress worker's DNS calls reach the controller's DNS handler on the
// mesh listener with its node cert, and never without one.
func TestMeshDNS(t *testing.T) {
	o, own := renewRig(t)
	leaf, err := x509.ParseCertificate(own.Certificate[0])
	must(t, err)
	body, _ := json.Marshal(worker.DNSRequest{Zone: "acme.dev", Records: []libdns.RR{{Name: "_acme-challenge", Type: "TXT", Data: "x"}}})
	call := func(peer []*x509.Certificate) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, worker.DNSAppendPath, strings.NewReader(string(body)))
		if peer != nil {
			r.TLS = &tls.ConnectionState{PeerCertificates: peer}
		}
		w := httptest.NewRecorder()
		o.meshHandler().ServeHTTP(w, r)
		return w
	}
	if w := call(nil); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "worker cert") {
		t.Errorf("no cert: %d %s", w.Code, w.Body)
	}
	// With the cert the call is the DNS handler's: acme.dev is not managed here.
	if w := call([]*x509.Certificate{leaf}); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "not a managed domain") {
		t.Errorf("worker cert: %d %s", w.Code, w.Body)
	}
}

package service

import (
	"context"
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
// mesh listener with its node cert; never without one, nor from a worker
// that is not an ingress node.
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
	// A worker that is not an ingress node is refused before any DNS call.
	if w := call([]*x509.Certificate{leaf}); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "only an ingress node") {
		t.Errorf("untagged worker: %d %s", w.Code, w.Body)
	}
	n, err := o.store.Nodes.Get(context.Background(), "w1")
	must(t, err)
	n.Tags = `["ingress"]`
	must(t, o.store.Nodes.Update(context.Background(), n))
	// With the cert the call is the DNS handler's: acme.dev is not managed here.
	if w := call([]*x509.Certificate{leaf}); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "not a managed domain") {
		t.Errorf("worker cert: %d %s", w.Code, w.Body)
	}
}

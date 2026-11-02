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
	"time"

	"github.com/libdns/libdns"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
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

// An ingress worker may answer a challenge only for a host routed to it.
func TestACMEHostsServed(t *testing.T) {
	o, _ := renewRig(t)
	ctx := context.Background()
	n, err := o.store.Nodes.Get(ctx, "w1")
	must(t, err)
	n.Tags = `["ingress"]`
	must(t, o.store.Nodes.Update(ctx, n))
	must(t, o.store.Settings.Set(ctx, settingDefaultIngress, "w1"))
	must(t, o.store.Routes.Create(ctx, store.Route{ID: "r1", Host: "app.acme.dev", Mode: "https", Target: "http://x", CreatedAt: time.Now()}))
	must(t, o.store.Routes.Create(ctx, store.Route{ID: "r2", Host: "*.wild.dev", Mode: "https", Target: "http://x", CreatedAt: time.Now()}))
	rr := func(name string) []libdns.RR { return []libdns.RR{{Name: name, Type: "TXT", Data: "x"}} }
	for _, c := range []struct {
		zone, name string
		ok         bool
	}{
		{"acme.dev", "_acme-challenge.app", true},
		{"acme.dev", "_acme-challenge.other", false},
		{"acme.dev", "_acme-challenge", false},
		{"wild.dev", "_acme-challenge", true},
		{"wild.dev", "_acme-challenge.sub", false},
	} {
		if err := o.acmeHostsServed(ctx, "w1", c.zone, rr(c.name)); (err == nil) != c.ok {
			t.Errorf("%s in %s: err = %v, want ok=%v", c.name, c.zone, err, c.ok)
		}
	}
}

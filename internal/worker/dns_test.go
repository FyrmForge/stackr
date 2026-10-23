package worker

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/clusterca"
)

// An ingress worker's proxy posts its DNS-01 calls to the DNS socket; the
// worker passes exactly the two DNS paths on to the controller's mesh
// listener under its node cert and hands the answer back.
func TestDNSForwardToController(t *testing.T) {
	ca, s, _ := certRig(t, time.Hour)
	ctlKey, _ := clusterca.NewKey()
	csr, _ := clusterca.NewCSR(ctlKey)
	ci, _ := ca.IssueController(csr, time.Now())
	kb, _ := clusterca.KeyPEM(ctlKey)
	ctl, _ := tls.X509KeyPair(ci.CertPEM, kb)
	var got []string
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.TLS.PeerCertificates) == 0 || r.TLS.PeerCertificates[0].Subject.CommonName == "" {
			http.Error(w, "no client cert", http.StatusForbidden)
			return
		}
		got = append(got, r.Method+" "+r.URL.Path)
		_, _ = w.Write([]byte(`{"zone":"acme.dev","records":[]}`))
	}))
	srv.TLS = clusterca.WorkerServerConfig(clusterca.StaticRoots(ca.Cert), func() tls.Certificate { return ctl }, nil)
	srv.TLS.Certificates = []tls.Certificate{ctl}
	srv.StartTLS()
	defer srv.Close()

	dir, err := os.MkdirTemp("", "wdns") // short: a unix socket path is capped
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "sub", DNSSocket)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.serveDNS(ctx, sock) }()
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
	post := func(method, path string) (int, string) {
		var resp *http.Response
		var err error
		for range 50 { // the listener comes up in the goroutine
			req, _ := http.NewRequest(method, "http://w"+path, strings.NewReader(`{"zone":"acme.dev"}`))
			if resp, err = hc.Do(req); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	if code, _ := post(http.MethodPost, DNSAppendPath); code != http.StatusServiceUnavailable {
		t.Errorf("no controller known: %d", code)
	}
	s.mesh.applied = &MeshSpec{Controller: srv.URL}
	if code, body := post(http.MethodPost, DNSAppendPath); code != http.StatusOK || !strings.Contains(body, "acme.dev") {
		t.Errorf("append: %d %s", code, body)
	}
	if code, _ := post(http.MethodPost, DNSDeletePath); code != http.StatusOK {
		t.Errorf("delete: %d", code)
	}
	for _, c := range [][2]string{{http.MethodGet, DNSAppendPath}, {http.MethodPost, "/v1/state"}, {http.MethodPost, RenewTunnelPath}} {
		if code, _ := post(c[0], c[1]); code != http.StatusNotFound {
			t.Errorf("%s %s: %d, want 404", c[0], c[1], code)
		}
	}
	if strings.Join(got, ",") != "POST "+DNSAppendPath+",POST "+DNSDeletePath {
		t.Errorf("controller saw %v", got)
	}
}

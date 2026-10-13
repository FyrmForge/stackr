package worker

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/clusterca"
)

// certRig is a cluster CA, a worker state dir holding a node cert issued
// issuedAgo in the past, and a server for that dir.
func certRig(t *testing.T, issuedAgo time.Duration) (*clusterca.CA, *Server, string) {
	t.Helper()
	now := time.Now()
	ca, err := clusterca.NewCA(now.Add(-200 * 24 * time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Cert.Raw})
	if err := os.WriteFile(filepath.Join(dir, "cluster-ca.crt"), caPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	own := issue(t, ca, "w1", now.Add(-issuedAgo), filepath.Join(dir, certFile), filepath.Join(dir, keyFile))
	s, _, _ := newMeshServer(t, dir, &pingFake{})
	s.own.set(own)
	return ca, s, dir
}

// issue makes a node key and cert for id at t0 and writes them when paths
// are given.
func issue(t *testing.T, ca *clusterca.CA, id string, t0 time.Time, paths ...string) tls.Certificate {
	t.Helper()
	key, _ := clusterca.NewKey()
	csr, _ := clusterca.NewCSR(key)
	iss, err := ca.IssueNode(csr, id, t0)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := clusterca.KeyPEM(key)
	if len(paths) == 2 {
		_ = os.WriteFile(paths[0], iss.CertPEM, 0o644)
		_ = os.WriteFile(paths[1], kb, 0o600)
	}
	c, err := tls.X509KeyPair(iss.CertPEM, kb)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// signer answers a CSR the way the controller does.
func signer(t *testing.T, ca *clusterca.CA, csr string) []byte {
	iss, err := ca.IssueNode([]byte(csr), "w1", time.Now())
	if err != nil {
		t.Error(err)
	}
	b, _ := json.Marshal(RenewAnswer{Cert: string(iss.CertPEM)})
	return b
}

func leaf(t *testing.T, c tls.Certificate) *x509.Certificate {
	x, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return x
}

// A cert in its last third is renewed over the tunnel with the current cert
// as the client cert: the new pair is in use and on disk, the old one kept
// as .prev. A cert not due is left alone.
func TestRenewOverTunnel(t *testing.T) {
	ca, s, dir := certRig(t, 70*24*time.Hour) // 90 day life, 20 left
	old := leaf(t, s.own.get())
	ctlKey, _ := clusterca.NewKey()
	csr, _ := clusterca.NewCSR(ctlKey)
	ci, _ := ca.IssueController(csr, time.Now())
	kb, _ := clusterca.KeyPEM(ctlKey)
	ctl, _ := tls.X509KeyPair(ci.CertPEM, kb)
	calls := 0
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != RenewTunnelPath || len(r.TLS.PeerCertificates) == 0 ||
			r.TLS.PeerCertificates[0].SerialNumber.Cmp(old.SerialNumber) != 0 {
			http.Error(w, "no client cert", 403)
			return
		}
		var in RenewRequest
		_ = json.NewDecoder(r.Body).Decode(&in)
		_, _ = w.Write(signer(t, ca, in.CSR))
	}))
	srv.TLS = clusterca.WorkerServerConfig(ca.Cert, func() tls.Certificate { return ctl }, nil)
	srv.TLS.Certificates = []tls.Certificate{ctl} // httptest adds its own otherwise
	srv.StartTLS()
	defer srv.Close()
	s.cfg.Registry.URL = srv.URL

	if err := s.renewOnce(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	now := leaf(t, s.own.get())
	if calls != 1 || now.SerialNumber.Cmp(old.SerialNumber) == 0 || !now.NotAfter.After(old.NotAfter) {
		t.Fatalf("calls %d, serial %v -> %v", calls, old.SerialNumber, now.SerialNumber)
	}
	disk, err := LoadNodeCert(filepath.Join(dir, certFile), filepath.Join(dir, keyFile))
	if err != nil || leaf(t, disk).SerialNumber.Cmp(now.SerialNumber) != 0 {
		t.Fatalf("on disk: %v", err)
	}
	prev, err := tls.LoadX509KeyPair(filepath.Join(dir, certFile+".prev"), filepath.Join(dir, keyFile+".prev"))
	if err != nil || leaf(t, prev).SerialNumber.Cmp(old.SerialNumber) != 0 {
		t.Fatalf(".prev: %v", err)
	}
	// The fresh cert is not due: no call.
	if err := s.renewOnce(context.Background(), time.Now()); err != nil || calls != 1 {
		t.Fatalf("second check: %v, calls %d", err, calls)
	}
}

// With no way over the tunnel the CSR goes to the panel, signed with the
// current key.
func TestRenewThroughPanel(t *testing.T) {
	ca, s, _ := certRig(t, 70*24*time.Hour)
	old := leaf(t, s.own.get())
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req RecoverRequest
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &req)
		c, p, err := OpenRecover(req)
		if r.URL.Path != RenewPanelPath || err != nil || p.CSR == "" || c.SerialNumber.Cmp(old.SerialNumber) != 0 {
			http.Error(w, "refused", 403)
			return
		}
		_, _ = w.Write(signer(t, ca, p.CSR))
	}))
	defer panel.Close()
	s.cfg.Registry.URL = "https://127.0.0.1:1" // nothing listens
	s.cfg.Recover.Panel = panel.URL
	if err := s.renewOnce(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if leaf(t, s.own.get()).SerialNumber.Cmp(old.SerialNumber) == 0 {
		t.Fatal("cert not swapped")
	}
}

// The force flag renews a cert that is not due, once.
func TestRenewNow(t *testing.T) {
	ca, s, _ := certRig(t, time.Hour)
	old := leaf(t, s.own.get())
	calls := 0
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var req RecoverRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		_, p, _ := OpenRecover(req)
		_, _ = w.Write(signer(t, ca, p.CSR))
	}))
	defer panel.Close()
	s.cfg.Recover.Panel = panel.URL
	s.renewNow.Store(true)
	for range 2 {
		if err := s.renewOnce(context.Background(), time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 || leaf(t, s.own.get()).SerialNumber.Cmp(old.SerialNumber) == 0 {
		t.Fatalf("calls = %d", calls)
	}
}

// An expired cert cannot renew: the error says to rejoin, nothing is sent.
func TestRenewExpired(t *testing.T) {
	_, s, _ := certRig(t, 95*24*time.Hour)
	s.cfg.Recover.Panel = "http://127.0.0.1:1"
	err := s.renewOnce(context.Background(), time.Now())
	if err == nil || !strings.Contains(err.Error(), "rejoin") {
		t.Fatalf("err = %v", err)
	}
}

// A crash between the two renames leaves a mismatched pair; the loader falls
// back to the previous one.
func TestLoadNodeCertFallsBack(t *testing.T) {
	ca, _, dir := certRig(t, time.Hour)
	c, k := filepath.Join(dir, certFile), filepath.Join(dir, keyFile)
	b, _ := os.ReadFile(c)
	kb, _ := os.ReadFile(k)
	_ = os.WriteFile(c+".prev", b, 0o600)
	_ = os.WriteFile(k+".prev", kb, 0o600)
	issue(t, ca, "w1", time.Now(), c, filepath.Join(t.TempDir(), "other.key")) // new cert, old key
	got, err := LoadNodeCert(c, k)
	if err != nil || string(got.Certificate[0]) == "" {
		t.Fatalf("fallback: %v", err)
	}
}

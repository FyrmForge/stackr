package service

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/clusterca"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/securitylog"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/worker"
)

func renewRig(t *testing.T) (*Orchestrator, tls.Certificate) {
	t.Helper()
	o := newClusterPanel(t)
	must(t, o.store.Nodes.Create(context.Background(), store.Node{
		ID: "w1", Name: "w1", Slug: "w1", Role: node.Worker, Tags: "[]", RangeCIDR: "10.209.0.0/16", WGAddress: "10.207.255.2",
		WGPublicKey: "wg-w1", Status: node.StatusOnline, CreatedAt: time.Now(),
	}))
	return o, nodeCert(t, o, "w1")
}

func newCSR(t *testing.T) string {
	key, err := clusterca.NewKey()
	must(t, err)
	csr, err := clusterca.NewCSR(key)
	must(t, err)
	return string(csr)
}

// Over the tunnel: the listener's verified client cert names the node; the
// answer is a cert for that node, recorded and audited.
func TestRenewTunnel(t *testing.T) {
	o, own := renewRig(t)
	leaf, err := x509.ParseCertificate(own.Certificate[0])
	must(t, err)
	body, _ := json.Marshal(worker.RenewRequest{CSR: newCSR(t)})
	r := httptest.NewRequest(http.MethodPost, worker.RenewTunnelPath, strings.NewReader(string(body)))
	r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}
	w := httptest.NewRecorder()
	o.meshHandler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var a worker.RenewAnswer
	must(t, json.Unmarshal(w.Body.Bytes(), &a))
	c, err := clusterca.ParseCertPEM([]byte(a.Cert))
	must(t, err)
	if p, err := clusterca.Verify(o.cluster.ca.Cert, []*x509.Certificate{c}, clusterca.RoleWorker, "w1", time.Now()); err != nil || p.NodeID != "w1" {
		t.Fatalf("renewed cert: %v", err)
	}
	es, err := o.SecurityLog(context.Background(), SecurityFilter{Action: securitylog.NodeCertRenew})
	must(t, err)
	if len(es) != 1 || !strings.Contains(es[0].Detail, "via tunnel") {
		t.Fatalf("audit = %+v", es)
	}
	// No client cert: refused.
	r = httptest.NewRequest(http.MethodPost, worker.RenewTunnelPath, strings.NewReader(string(body)))
	w = httptest.NewRecorder()
	o.meshHandler().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("no cert: status %d", w.Code)
	}
}

// Through the panel: signed with the current key, checked like recovery; a
// replay and a call without a CSR are refused.
func TestRenewThroughPanel(t *testing.T) {
	o, own := renewRig(t)
	ctx := context.Background()
	p := worker.RecoverPayload{Time: time.Now().Unix(), Nonce: "n1", CSR: newCSR(t)}
	req, err := worker.SignRecover(own, p)
	must(t, err)
	a, err := o.RenewNodeCert(ctx, req)
	if err != nil || a.Cert == "" {
		t.Fatalf("renew: %v", err)
	}
	if _, err := o.RenewNodeCert(ctx, req); err == nil {
		t.Fatal("a replay was accepted")
	}
	req, err = worker.SignRecover(own, worker.RecoverPayload{Time: time.Now().Unix(), Nonce: "n2"})
	must(t, err)
	if _, err := o.RenewNodeCert(ctx, req); err == nil {
		t.Fatal("a call without a CSR was accepted")
	}
}

// A worker whose newest cert ran out shows "cert-expired" in the node list
// and on the map; a renewed one does not.
func TestCertExpiredStatus(t *testing.T) {
	o, _ := renewRig(t)
	ctx := context.Background()
	if o.certExpired("w1") {
		t.Fatal("a fresh cert counts as expired")
	}
	_, err := o.store.NodeCerts.RevokeNode(ctx, "w1", time.Now())
	must(t, err)
	must(t, o.store.NodeCerts.Create(ctx, store.NodeCert{Serial: "old", NodeID: "w1", Role: clusterca.RoleWorker,
		NotAfter: time.Now().Add(-48 * time.Hour), CreatedAt: time.Now().Add(-92 * 24 * time.Hour)}))
	n, err := o.store.Nodes.Get(ctx, "w1")
	must(t, err)
	if got := o.shownStatus(n); got != NodeCertExpired {
		t.Fatalf("status = %q", got)
	}
	m := buildClusterMap([]clusterNodeIn{{Node: n, Expired: true}})
	if m.Nodes[0].Status != NodeCertExpired {
		t.Fatalf("map status = %q", m.Nodes[0].Status)
	}
}

// The controller renews its own cert once it is in its last third.
func TestControllerCertRenewsWhenDue(t *testing.T) {
	o := newClusterPanel(t)
	key, err := clusterca.NewKey()
	must(t, err)
	csr, err := clusterca.NewCSR(key)
	must(t, err)
	iss, err := o.cluster.ca.IssueController(csr, time.Now().Add(-70*24*time.Hour))
	must(t, err)
	kb, err := clusterca.KeyPEM(key)
	must(t, err)
	must(t, os.WriteFile(filepath.Join(o.cfg.DataDir, controllerCertFile), iss.CertPEM, 0o644))
	must(t, os.WriteFile(filepath.Join(o.cfg.DataDir, controllerKeyFile), kb, 0o600))
	changed, err := o.cluster.renew(context.Background(), o.cfg.DataDir)
	must(t, err)
	c, err := x509.ParseCertificate(o.cluster.cert().Certificate[0])
	must(t, err)
	if !changed || clusterca.NeedsRenewal(c, time.Now()) {
		t.Fatalf("changed %v, due %v", changed, clusterca.NeedsRenewal(c, time.Now()))
	}
}

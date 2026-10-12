package clusterca

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

type leaf struct {
	Issued
	cert *x509.Certificate
	tls  tls.Certificate
}

func mustCA(t *testing.T) *CA {
	t.Helper()
	ca, err := NewCA(now)
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

// issue makes a key, CSR and cert; role controller when nodeID is empty.
func issue(t *testing.T, ca *CA, nodeID string, at time.Time) leaf {
	t.Helper()
	key, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	csr, err := NewCSR(key)
	if err != nil {
		t.Fatal(err)
	}
	var is Issued
	if nodeID == "" {
		is, err = ca.IssueController(csr, at)
	} else {
		is, err = ca.IssueNode(csr, nodeID, at)
	}
	if err != nil {
		t.Fatal(err)
	}
	kp, err := KeyPEM(key)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(is.CertPEM, kp)
	if err != nil {
		t.Fatal(err)
	}
	c, err := ParseCertPEM(is.CertPEM)
	if err != nil {
		t.Fatal(err)
	}
	return leaf{is, c, pair}
}

func TestIssueAndVerify(t *testing.T) {
	ca := mustCA(t)
	ctl := issue(t, ca, "", now)
	node := issue(t, ca, "n1", now)

	if ctl.Role != RoleController || ctl.NodeID != "" || node.Role != RoleWorker || node.NodeID != "n1" {
		t.Fatalf("issued: %+v %+v", ctl.Issued, node.Issued)
	}
	if !node.NotAfter.Equal(now.Add(NodeLifetime)) || !node.cert.NotAfter.Equal(node.NotAfter) {
		t.Fatalf("not after %v", node.NotAfter)
	}
	p, err := Verify(ca.Cert, []*x509.Certificate{node.cert}, RoleWorker, "", now)
	if err != nil || p.NodeID != "n1" || p.Role != RoleWorker || p.Serial != node.Serial {
		t.Fatalf("verify node: %+v %v", p, err)
	}
	if _, err := Verify(ca.Cert, []*x509.Certificate{node.cert}, RoleWorker, "n1", now); err != nil {
		t.Fatalf("matching node id: %v", err)
	}
	p, err = Verify(ca.Cert, []*x509.Certificate{ctl.cert}, RoleController, "", now)
	if err != nil || p.Role != RoleController || p.NodeID != "" {
		t.Fatalf("verify controller: %+v %v", p, err)
	}
	if ctl.Serial == node.Serial {
		t.Fatal("serials repeat")
	}
}

func TestVerifyRejects(t *testing.T) {
	ca := mustCA(t)
	other := mustCA(t)
	ctl := issue(t, ca, "", now)
	node := issue(t, ca, "n1", now)
	foreign := issue(t, other, "n1", now)

	cases := []struct {
		name  string
		chain []*x509.Certificate
		role  string
		id    string
		at    time.Time
		want  string
	}{
		{"empty", nil, RoleWorker, "", now, "no certificate"},
		{"worker as controller", []*x509.Certificate{node.cert}, RoleController, "", now, "role is"},
		{"controller as worker", []*x509.Certificate{ctl.cert}, RoleWorker, "", now, "role is"},
		{"wrong node id", []*x509.Certificate{node.cert}, RoleWorker, "n2", now, "node id is"},
		{"wrong CA", []*x509.Certificate{foreign.cert}, RoleWorker, "", now, "not signed"},
		{"expired", []*x509.Certificate{node.cert}, RoleWorker, "", now.Add(NodeLifetime + ClockSlack + time.Minute), "expired"},
		{"not yet valid", []*x509.Certificate{node.cert}, RoleWorker, "", now.Add(-2*ClockSlack - time.Minute), "not valid yet"},
		{"CA cert as leaf", []*x509.Certificate{ca.Cert}, RoleWorker, "", now, "leaf is a CA"},
	}
	for _, c := range cases {
		_, err := Verify(ca.Cert, c.chain, c.role, c.id, c.at)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want %q", c.name, err, c.want)
		}
	}
}

func TestClockSlack(t *testing.T) {
	ca := mustCA(t)
	node := issue(t, ca, "n1", now)
	end := node.cert.NotAfter
	start := node.cert.NotBefore

	ok := []time.Time{end, end.Add(ClockSlack - time.Second), start.Add(-ClockSlack + time.Second)}
	for _, at := range ok {
		if _, err := Verify(ca.Cert, []*x509.Certificate{node.cert}, RoleWorker, "", at); err != nil {
			t.Errorf("at %v should pass: %v", at, err)
		}
	}
	bad := []time.Time{end.Add(ClockSlack + time.Second), start.Add(-ClockSlack - time.Second)}
	for _, at := range bad {
		if _, err := Verify(ca.Cert, []*x509.Certificate{node.cert}, RoleWorker, "", at); err == nil {
			t.Errorf("at %v should fail", at)
		}
	}
}

func TestNeedsRenewal(t *testing.T) {
	ca := mustCA(t)
	c := issue(t, ca, "n1", now).cert
	life := c.NotAfter.Sub(c.NotBefore)
	third := life / 3
	if NeedsRenewal(c, c.NotAfter.Add(-third-time.Second)) {
		t.Error("just over a third left should not renew")
	}
	if !NeedsRenewal(c, c.NotAfter.Add(-third)) {
		t.Error("exactly a third left should renew")
	}
	if !NeedsRenewal(c, c.NotAfter.Add(time.Hour)) {
		t.Error("expired should renew")
	}
	if NeedsRenewal(c, c.NotBefore) {
		t.Error("fresh should not renew")
	}
}

func TestIssueErrors(t *testing.T) {
	ca := mustCA(t)
	if _, err := ca.IssueNode([]byte("junk"), "n1", now); err == nil {
		t.Error("junk CSR accepted")
	}
	key, _ := NewKey()
	csr, _ := NewCSR(key)
	if _, err := ca.IssueNode(csr, "", now); err == nil {
		t.Error("empty node id accepted")
	}
	// A CSR with a flipped byte in the signature must fail.
	bad := []byte(string(csr))
	i := strings.Index(string(bad), "\n") + 5
	bad[i] ^= 1
	if _, err := ca.IssueNode(bad, "n1", now); err == nil {
		t.Error("tampered CSR accepted")
	}
}

func TestSaveLoadCA(t *testing.T) {
	dir := t.TempDir()
	ca := mustCA(t)
	if _, err := LoadCA(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty dir: %v", err)
	}
	if err := SaveCA(dir, ca); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dir, CAKeyFile))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", st, err)
	}
	got, err := LoadCA(dir)
	if err != nil || !got.Cert.Equal(ca.Cert) {
		t.Fatalf("load: %v", err)
	}
	// The loaded CA signs certs the original CA cert accepts.
	n := issue(t, got, "n1", now)
	if _, err := Verify(ca.Cert, []*x509.Certificate{n.cert}, RoleWorker, "n1", now); err != nil {
		t.Fatal(err)
	}
}

// handshake runs a real TLS exchange and returns the client and server errors.
func handshake(t *testing.T, srv, cli *tls.Config) (cerr, serr error) {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", srv)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			done <- err
			return
		}
		defer c.Close()
		buf := make([]byte, 1)
		_, err = c.Read(buf) // drives the handshake and, in TLS 1.3, the client cert check
		if err == nil {
			_, err = c.Write(buf)
		}
		done <- err
	}()
	c, err := tls.Dial("tcp", ln.Addr().String(), cli)
	if err == nil {
		defer c.Close()
		if _, err = c.Write([]byte{1}); err == nil {
			_, err = c.Read(make([]byte, 1))
		}
	}
	return err, <-done
}

func TestTLS(t *testing.T) {
	ca := mustCA(t)
	other := mustCA(t)
	real := time.Now()
	ctl := issue(t, ca, "", real)
	node := issue(t, ca, "n1", real)
	node2 := issue(t, ca, "n2", real)

	srv := ServerConfig(ca.Cert, node.tls, nil)

	if cerr, serr := handshake(t, srv, ClientConfig(ca.Cert, ctl.tls, "", nil)); cerr != nil || serr != nil {
		t.Fatalf("happy path: %v / %v", cerr, serr)
	}
	if cerr, serr := handshake(t, srv, ClientConfig(ca.Cert, ctl.tls, "n1", nil)); cerr != nil || serr != nil {
		t.Fatalf("pinned node: %v / %v", cerr, serr)
	}
	if cerr, _ := handshake(t, srv, ClientConfig(ca.Cert, ctl.tls, "n2", nil)); cerr == nil {
		t.Error("wrong node id accepted by client")
	}
	// A worker cert cannot dial a worker.
	if cerr, serr := handshake(t, srv, ClientConfig(ca.Cert, node2.tls, "", nil)); cerr == nil && serr == nil {
		t.Error("worker client cert accepted by server")
	}
	// No client cert at all.
	if cerr, serr := handshake(t, srv, ClientConfig(ca.Cert, tls.Certificate{}, "", nil)); cerr == nil && serr == nil {
		t.Error("anonymous client accepted")
	}
	// Controller signed by a different CA.
	fctl := issue(t, other, "", real)
	if cerr, serr := handshake(t, srv, ClientConfig(ca.Cert, fctl.tls, "", nil)); cerr == nil && serr == nil {
		t.Error("foreign controller accepted")
	}
	// Server presenting a cert from a different CA.
	fsrv := ServerConfig(ca.Cert, issue(t, other, "n1", real).tls, nil)
	if cerr, _ := handshake(t, fsrv, ClientConfig(ca.Cert, ctl.tls, "", nil)); cerr == nil {
		t.Error("foreign worker accepted")
	}
	// Expired worker cert.
	old := issue(t, ca, "n1", real.Add(-NodeLifetime-2*ClockSlack))
	osrv := ServerConfig(ca.Cert, old.tls, nil)
	if cerr, _ := handshake(t, osrv, ClientConfig(ca.Cert, ctl.tls, "", nil)); cerr == nil {
		t.Error("expired worker accepted")
	}
	// Revoked controller.
	revoked := func(s string) bool { return s == ctl.Serial }
	rsrv := ServerConfig(ca.Cert, node.tls, revoked)
	if cerr, serr := handshake(t, rsrv, ClientConfig(ca.Cert, ctl.tls, "", nil)); cerr == nil && serr == nil {
		t.Error("revoked controller accepted")
	}
	// Revoked worker.
	rcli := ClientConfig(ca.Cert, ctl.tls, "", func(s string) bool { return s == node.Serial })
	if cerr, _ := handshake(t, srv, rcli); cerr == nil {
		t.Error("revoked worker accepted")
	}
}

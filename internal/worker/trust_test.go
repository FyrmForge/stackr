package worker

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/clusterca"
)

// trustRig is a worker listener built the way cmd/stackrd builds it (live
// trust list, allow list as the revoked hook) with a join-time CA on disk.
type trustRig struct {
	ca1, ca2 *clusterca.CA
	trust    *Trust
	caFile   string
	url      string
}

func newTrustRig(t *testing.T) *trustRig {
	t.Helper()
	now := time.Now()
	r := &trustRig{}
	var err error
	if r.ca1, err = clusterca.NewCA(now); err != nil {
		t.Fatal(err)
	}
	if r.ca2, err = clusterca.NewCA(now); err != nil {
		t.Fatal(err)
	}
	r.caFile = filepath.Join(t.TempDir(), "cluster-ca.crt")
	if err := os.WriteFile(r.caFile, clusterca.BundlePEM(clusterca.StaticRoots(r.ca1.Cert)()), 0o644); err != nil {
		t.Fatal(err)
	}
	r.trust = NewTrust(r.caFile)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	srv.TLS = clusterca.ServerConfig(r.trust.Roots, issue(t, r.ca1, "w1", now), r.trust.Refuses)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	r.url = srv.URL
	return r
}

// controller is a controller cert from ca and its serial (hex).
func controller(t *testing.T, ca *clusterca.CA) (tls.Certificate, string) {
	t.Helper()
	key, _ := clusterca.NewKey()
	csr, _ := clusterca.NewCSR(key)
	iss, err := ca.IssueController(csr, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := clusterca.KeyPEM(key)
	c, err := tls.X509KeyPair(iss.CertPEM, kb)
	if err != nil {
		t.Fatal(err)
	}
	return c, iss.Serial
}

// call is a request to the worker presenting c; the verdict is the error.
func (r *trustRig) call(c tls.Certificate) error {
	// Safe: the test only cares whether the worker accepts the client cert.
	cl := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true, Certificates: []tls.Certificate{c}}}}
	resp, err := cl.Get(r.url)
	if err == nil {
		_ = resp.Body.Close()
	}
	return err
}

func (r *trustRig) pems(cas ...*clusterca.CA) []string {
	var l []string
	for _, c := range cas {
		l = append(l, string(clusterca.BundlePEM(clusterca.StaticRoots(c.Cert)())))
	}
	return l
}

// A trust list pushed while the listener runs takes effect on the next
// handshake: a controller from the new root is refused before, accepted
// after. A spec that omits the list keeps the one held.
func TestTrustListAppliedLive(t *testing.T) {
	r := newTrustRig(t)
	c2, _ := controller(t, r.ca2)
	if err := r.call(c2); err == nil {
		t.Fatal("a new-root controller was accepted before the list arrived")
	}
	if err := r.trust.Apply(r.pems(r.ca1, r.ca2), nil); err != nil {
		t.Fatal(err)
	}
	if err := r.call(c2); err != nil {
		t.Fatalf("after the list: %v", err)
	}
	c1, _ := controller(t, r.ca1)
	if err := r.call(c1); err != nil {
		t.Fatalf("old root during the overlap: %v", err)
	}
	// Omitted: nothing is wiped.
	if err := r.trust.Apply(nil, nil); err != nil || len(r.trust.Roots()) != 2 {
		t.Fatalf("omitted list: %v, %d roots", err, len(r.trust.Roots()))
	}
	if err := r.call(c2); err != nil {
		t.Fatalf("after an omitting spec: %v", err)
	}
	// The old root leaves the list.
	if err := r.trust.Apply(r.pems(r.ca2), nil); err != nil {
		t.Fatal(err)
	}
	if err := r.call(c1); err == nil {
		t.Fatal("a dropped root still vouches")
	}
	// A bad root changes nothing.
	if err := r.trust.Apply([]string{"junk"}, nil); err == nil || len(r.trust.Roots()) != 1 {
		t.Fatalf("junk list: %v", err)
	}
}

// With an allow list the worker accepts a controller only when its serial is
// on it; without one any controller from a trusted root passes.
func TestControllerAllowList(t *testing.T) {
	r := newTrustRig(t)
	a, aSerial := controller(t, r.ca1)
	b, _ := controller(t, r.ca1)
	if err := r.call(b); err != nil {
		t.Fatalf("no list: %v", err)
	}
	if err := r.trust.Apply(nil, []string{aSerial}); err != nil {
		t.Fatal(err)
	}
	if err := r.call(a); err != nil {
		t.Fatalf("listed: %v", err)
	}
	if err := r.call(b); err == nil {
		t.Fatal("a controller off the list was accepted")
	}
	// An empty list does not lift the restriction once one is set.
	if err := r.trust.Apply(r.pems(r.ca1), nil); err == nil {
		t.Fatal("an empty list replaced the set one")
	}
	if err := r.call(b); err == nil {
		t.Fatal("a controller off the list was accepted after an empty list")
	}
}

// A list of only blank or zero serials is no list: it never replaces a set
// one, so a restart cannot read it back as "any controller".
func TestBlankSerialsDoNotLiftAllowList(t *testing.T) {
	r := newTrustRig(t)
	_, serial := controller(t, r.ca1)
	if err := r.trust.Apply(nil, []string{serial}); err != nil {
		t.Fatal(err)
	}
	if err := r.trust.Apply(r.pems(r.ca1), []string{"0", " "}); err == nil {
		t.Fatal("a blank allow list replaced the set one")
	}
	if !NewTrust(r.caFile).Refuses("abcdef") {
		t.Fatal("the allow list is gone after a restart")
	}
}

// The list and the allow list survive a restart of the TLS setup, and the
// trust file wins over the join-time CA.
func TestTrustPersists(t *testing.T) {
	r := newTrustRig(t)
	_, serial := controller(t, r.ca1)
	if err := r.trust.Apply(r.pems(r.ca2), []string{serial}); err != nil {
		t.Fatal(err)
	}
	again := NewTrust(r.caFile)
	roots := again.Roots()
	if len(roots) != 1 || clusterca.Fingerprint(roots[0]) != clusterca.Fingerprint(r.ca2.Cert) {
		t.Fatalf("roots after restart: %d", len(roots))
	}
	if again.Refuses(serial) || !again.Refuses("abcdef") {
		t.Fatal("allow list lost over a restart")
	}
	// A serial written with other case or leading zeros still matches.
	if again.Refuses("00"+hex.EncodeToString([]byte{0xAB})) != again.Refuses("ab") {
		t.Fatal("serial not normalised")
	}
}

// A worker that missed a controller cert switch still recovers through the
// panel: the call is signed with the node key over the public URL, not the
// refused mTLS, and the previous cert, still on its list, co-signs the answer
// that brings the list admitting the new one. Signed by the new cert alone,
// or by a cert on no list (a revoked one), the answer is refused.
func TestRecoverWhileControllerRefused(t *testing.T) {
	r := newTrustRig(t)
	prev, prevSerial := controller(t, r.ca1)
	ctl, serial := controller(t, r.ca1)
	if err := r.trust.Apply(nil, []string{prevSerial}); err != nil {
		t.Fatal(err)
	}
	if err := r.call(ctl); err == nil {
		t.Fatal("controller should be refused")
	}
	spec := meshSpec()
	spec.TrustRoots, spec.ControllerSerials = r.pems(r.ca1, r.ca2), []string{serial}

	s, _, _ := newMeshServer(t, t.TempDir(), &pingFake{})
	s.cfg.Trust = r.trust
	s.cfg.Recover = RecoverConfig{Own: selfCert(t), Panel: signedPanel(t, ctl, spec).URL}
	if err := s.recoverOnce(context.Background()); err == nil {
		t.Fatal("an answer from an unlisted controller cert was applied")
	}
	s.cfg.Recover.Panel = signedPanel(t, ctl, spec, prev).URL
	if err := s.recoverOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.call(ctl); err != nil {
		t.Fatalf("after recovery: %v", err)
	}
	if len(r.trust.Roots()) != 2 {
		t.Fatalf("roots after recovery: %d", len(r.trust.Roots()))
	}
}

// The worker's own dial to the controller (renewal, registry) holds the same
// allow list.
func TestGuardRefusesUnlistedController(t *testing.T) {
	r := newTrustRig(t)
	a, aSerial := controller(t, r.ca1)
	b, _ := controller(t, r.ca1)
	tc := r.trust.Guard(clusterca.ControllerClientConfig(r.trust.Roots, tls.Certificate{}))
	if err := r.trust.Apply(nil, []string{aSerial}); err != nil {
		t.Fatal(err)
	}
	if err := tc.VerifyPeerCertificate(a.Certificate, nil); err != nil {
		t.Fatalf("listed: %v", err)
	}
	if err := tc.VerifyPeerCertificate(b.Certificate, nil); err == nil {
		t.Fatal("unlisted controller accepted")
	}
}

// During a rollover a cert from the older root is renewed at once, though it
// is far from due, and not again once it comes from the first root.
func TestRenewOffSigningRoot(t *testing.T) {
	ca, s, _ := certRig(t, time.Hour) // fresh cert from ca
	next, err := clusterca.NewCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	pem := func(c *x509.Certificate) string { return string(clusterca.BundlePEM([]*x509.Certificate{c})) }
	if err := s.cfg.Trust.Apply([]string{pem(next.Cert), pem(ca.Cert)}, nil); err != nil {
		t.Fatal(err)
	}
	calls := 0
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var req RecoverRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		_, p, _ := OpenRecover(req)
		_, _ = w.Write(signer(t, next, p.CSR))
	}))
	defer panel.Close()
	s.cfg.Recover.Panel = panel.URL
	if err := s.renewOnce(context.Background(), time.Now()); err != nil || calls != 1 {
		t.Fatalf("off the signing root: %v, calls %d", err, calls)
	}
	if err := s.renewOnce(context.Background(), time.Now()); err != nil || calls != 1 {
		t.Fatalf("on the signing root: %v, calls %d", err, calls)
	}
}

// A rejoin resets the trust files: the old roots and allow list go, the join
// answer's take their place; with none, the new CA file alone is trusted.
func TestResetTrust(t *testing.T) {
	r := newTrustRig(t)
	_, old := controller(t, r.ca1)
	if err := r.trust.Apply(r.pems(r.ca1), []string{old}); err != nil {
		t.Fatal(err)
	}
	// The join wrote the new root as the CA file.
	if err := os.WriteFile(r.caFile, []byte(r.pems(r.ca2)[0]), 0o644); err != nil {
		t.Fatal(err)
	}
	_, fresh := controller(t, r.ca2)
	if err := ResetTrust(r.caFile, r.pems(r.ca2), []string{fresh}); err != nil {
		t.Fatal(err)
	}
	got := NewTrust(r.caFile)
	if roots := got.Roots(); len(roots) != 1 || clusterca.Fingerprint(roots[0]) != clusterca.Fingerprint(r.ca2.Cert) {
		t.Fatalf("roots after rejoin: %d, want the new root alone", len(roots))
	}
	if got.Refuses(fresh) || !got.Refuses(old) {
		t.Fatal("allow list after rejoin: want the new serial only")
	}
	if err := ResetTrust(r.caFile, nil, nil); err != nil {
		t.Fatal(err)
	}
	got = NewTrust(r.caFile)
	if roots := got.Roots(); len(roots) != 1 || clusterca.Fingerprint(roots[0]) != clusterca.Fingerprint(r.ca2.Cert) || got.Refuses(old) {
		t.Fatal("an empty answer must leave the CA file alone trusted, no allow list")
	}
}

// A spec bringing a new signing root wakes the cert check at once; the same
// list again does not.
func TestNewSigningRootKicksRenewal(t *testing.T) {
	ca, s, _ := certRig(t, time.Hour)
	next, err := clusterca.NewCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	pem := func(c *x509.Certificate) string { return string(clusterca.BundlePEM([]*x509.Certificate{c})) }
	kicked := func() bool {
		select {
		case <-s.renewKick:
			return true
		default:
			return false
		}
	}
	if err := s.applyTrust([]string{pem(ca.Cert)}, nil); err != nil || kicked() {
		t.Fatalf("same root kicked a renewal (err %v)", err)
	}
	if err := s.applyTrust([]string{pem(next.Cert), pem(ca.Cert)}, nil); err != nil || !kicked() {
		t.Fatalf("new signing root did not kick a renewal (err %v)", err)
	}
	if err := s.applyTrust([]string{pem(next.Cert), pem(ca.Cert)}, nil); err != nil || kicked() {
		t.Fatalf("an unchanged list kicked again (err %v)", err)
	}
}

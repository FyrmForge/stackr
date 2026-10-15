package worker

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/clusterca"
)

// ctlCert is a controller cert (and key) from ca.
func ctlCert(t *testing.T, ca *clusterca.CA) tls.Certificate {
	t.Helper()
	c, _ := controller(t, ca)
	return c
}

// signedPanel answers recovery calls with spec, signed by ctl.
// prev, when given, co-signs as the controller's previous cert.
func signedPanel(t *testing.T, ctl tls.Certificate, spec MeshSpec, prev ...tls.Certificate) *httptest.Server {
	t.Helper()
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rr RecoverRequest
		if json.NewDecoder(r.Body).Decode(&rr) != nil {
			http.Error(w, "bad", 400)
			return
		}
		_, p, err := OpenRecover(rr)
		if err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		a, err := SignAnswer(ctl, p.Nonce, spec)
		if err == nil && len(prev) > 0 {
			err = a.CoSign(prev[0], p.Nonce)
		}
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		_ = json.NewEncoder(w).Encode(a)
	}))
	t.Cleanup(panel.Close)
	return panel
}

// H2: an answer signed by a controller cert of the worker's own roots is
// applied, trust fields and all; one signed by anyone else, or not signed, is
// applied in no part.
func TestRecoverAppliesOnlySignedAnswers(t *testing.T) {
	r := newTrustRig(t)
	ctl, serial := controller(t, r.ca1)

	spec := meshSpec()
	spec.TrustRoots, spec.ControllerSerials = r.pems(r.ca1, r.ca2), []string{serial}
	good := signedPanel(t, ctl, spec)

	s, _, _ := newMeshServer(t, t.TempDir(), &pingFake{})
	s.cfg.Trust = r.trust
	s.cfg.Recover = RecoverConfig{Own: selfCert(t), Panel: good.URL}
	if err := s.recoverOnce(context.Background()); err != nil {
		t.Fatalf("signed answer: %v", err)
	}
	if len(r.trust.Roots()) != 2 || !r.trust.Refuses("abcdef") || r.trust.Refuses(serial) {
		t.Fatalf("signed trust not applied: %d roots", len(r.trust.Roots()))
	}

	// A controller cert from a root the worker does not trust.
	other, err := clusterca.NewCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	evilSpec := meshSpec()
	evilSpec.TrustRoots, evilSpec.ControllerSerials, evilSpec.Panel = r.pems(other), nil, "https://evil.example"
	evil := signedPanel(t, ctlCert(t, other), evilSpec)

	s2, _, rec2 := newMeshServer(t, t.TempDir(), &pingFake{})
	s2.cfg.Trust = r.trust
	s2.cfg.Recover = RecoverConfig{Own: selfCert(t), Panel: evil.URL}
	if err := s2.recoverOnce(context.Background()); err == nil {
		t.Fatal("an answer signed by an untrusted controller was accepted")
	}
	if _, ok := rec2.Last(); ok {
		t.Fatal("a mesh from an untrusted answer was applied")
	}
	if len(r.trust.Roots()) != 2 || !r.trust.Refuses("abcdef") {
		t.Fatal("an untrusted answer changed the trust")
	}
	if got := s2.panelURL(); got == "https://evil.example" {
		t.Fatal("an untrusted answer moved the panel URL")
	}

	// A bare, unsigned spec, as an on-path attacker would send it.
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(evilSpec)
	}))
	defer plain.Close()
	s2.cfg.Recover.Panel = plain.URL
	if err := s2.recoverOnce(context.Background()); err == nil {
		t.Fatal("an unsigned answer was accepted")
	}
}

// An answer signed for one request is no good for the next.
func TestRecoverAnswerIsBoundToTheRequest(t *testing.T) {
	r := newTrustRig(t)
	ctl := ctlCert(t, r.ca1)
	a, err := SignAnswer(ctl, "nonce-1", meshSpec())
	if err != nil {
		t.Fatal(err)
	}
	roots := []*x509.Certificate{r.ca1.Cert}
	if _, err := a.Open(roots, nil, "nonce-1", time.Now()); err != nil {
		t.Fatalf("right nonce: %v", err)
	}
	if _, err := a.Open(roots, nil, "nonce-2", time.Now()); err == nil {
		t.Fatal("an answer to another request was accepted")
	}
	a.Spec = append([]byte(nil), a.Spec...)
	a.Spec[len(a.Spec)/2] ^= 1
	if _, err := a.Open(roots, nil, "nonce-1", time.Now()); err == nil {
		t.Fatal("a tampered spec was accepted")
	}
}

// M8: once a controller allow list is set, a spec that carries trust but an
// empty list is refused; a spec with no trust fields at all leaves it alone.
func TestTrustRefusesEmptyAllowListOnceSet(t *testing.T) {
	r := newTrustRig(t)
	_, serial := controller(t, r.ca1)
	if err := r.trust.Apply(r.pems(r.ca1), []string{serial}); err != nil {
		t.Fatal(err)
	}
	if err := r.trust.Apply(r.pems(r.ca1), nil); err == nil {
		t.Fatal("an empty allow list replaced a set one")
	}
	if r.trust.Refuses(serial) {
		t.Fatal("the allow list was lost")
	}
	if err := r.trust.Apply(nil, nil); err != nil || r.trust.Refuses(serial) || !r.trust.Refuses("abcdef") {
		t.Fatalf("a spec with no trust fields touched the list: %v", err)
	}
}

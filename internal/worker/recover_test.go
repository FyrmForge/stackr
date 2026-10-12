package worker

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func selfCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{SerialNumber: big.NewInt(1),
		Subject: pkix.Name{CommonName: "w1"}, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)},
		&x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "w1"}}, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// The panel gets a request whose signature checks out and the node's facts;
// the worker applies the spec it answers. A tampered payload fails.
func TestRecoverThroughPanel(t *testing.T) {
	own := selfCert(t)
	var got RecoverPayload
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req RecoverRequest
		if r.URL.Path != RecoverPath || json.NewDecoder(r.Body).Decode(&req) != nil {
			http.Error(w, "bad", 400)
			return
		}
		_, p, err := OpenRecover(req)
		if err != nil {
			http.Error(w, err.Error(), 403)
			return
		}
		got = p
		_ = json.NewEncoder(w).Encode(meshSpec())
	}))
	defer panel.Close()

	s, _, rec := newMeshServer(t, t.TempDir(), &pingFake{})
	s.cfg.Recover = RecoverConfig{Own: own, Panel: panel.URL}
	if err := s.recoverOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got.Nonce == "" || time.Since(time.Unix(got.Time, 0)) > time.Minute {
		t.Fatalf("payload = %+v", got)
	}
	if cfg, ok := rec.Last(); !ok || len(cfg.Peers) != 1 {
		t.Fatalf("spec not applied: %+v", cfg)
	}

	req, err := SignRecover(own, RecoverPayload{Time: 1, Nonce: "a"})
	if err != nil {
		t.Fatal(err)
	}
	req.Payload = []byte(`{"time":2,"nonce":"a"}`)
	if _, _, err := OpenRecover(req); err == nil {
		t.Fatal("tampered payload accepted")
	}
}

// No flag: the panel URL comes from the last applied spec.
func TestRecoverPanelFromSpec(t *testing.T) {
	s, _, _ := newMeshServer(t, t.TempDir(), &pingFake{})
	sp := meshSpec()
	sp.Panel = "https://stkr.example.com"
	if err := s.ApplyMesh(context.Background(), sp); err != nil {
		t.Fatal(err)
	}
	s.mesh.applied = nil // a restart: only the saved spec knows
	if got := s.panelURL(); got != "https://stkr.example.com" {
		t.Fatalf("panel = %q", got)
	}
}

package worker

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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
			http.Error(w, err.Error(), http.StatusForbidden)
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

// logCapture swaps slog's default for a buffer for one test.
func logCapture(t *testing.T) *bytes.Buffer {
	t.Helper()
	var b bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&b, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &b
}

// panelAnswering is a panel whose recover endpoint answers the status last set.
func panelAnswering(t *testing.T, status *atomic.Int32) string {
	t.Helper()
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if code := int(status.Load()); code != http.StatusOK {
			http.Error(w, "no", code)
			return
		}
		_ = json.NewEncoder(w).Encode(meshSpec())
	}))
	t.Cleanup(panel.Close)
	return panel.URL
}

// A worker the cluster removed is refused (403) on every call. After 24 hours
// of that it logs once that it was removed and stops asking. A 500 is the
// panel's trouble and never counts; a 200 starts the count over.
func TestRecoverGivesUpWhenRefusedForADay(t *testing.T) {
	logs := logCapture(t)
	var status atomic.Int32
	status.Store(http.StatusForbidden)
	s, _, _ := newMeshServer(t, t.TempDir(), &pingFake{})
	s.cfg.Recover = RecoverConfig{Own: selfCert(t), Panel: panelAnswering(t, &status)}

	var st recoverState
	t0 := time.Now().Add(2 * RecoverAfter) // the controller has been quiet since the server started
	// every 6 minutes, past the longest pause, for 23 h 54 min: still asking
	for at := time.Duration(0); at < 24*time.Hour-5*time.Minute; at += 6 * time.Minute {
		if s.recoverStep(t0.Add(at), &st) {
			t.Fatalf("gave up after %s", at)
		}
	}
	if !s.recoverStep(t0.Add(24*time.Hour+time.Minute), &st) {
		t.Fatalf("still asking after a day of refusals: %+v", st)
	}
	if n := strings.Count(logs.String(), "removed from cluster"); n != 1 || !strings.Contains(logs.String(), "stackr-install uninstall") {
		t.Fatalf("log lines about removal = %d:\n%s", n, logs)
	}

	// Server errors alone never end the loop, however long they last.
	status.Store(http.StatusInternalServerError)
	st = recoverState{}
	for at := time.Duration(0); at < 72*time.Hour; at += 6 * time.Minute {
		if s.recoverStep(t0.Add(at), &st) {
			t.Fatalf("gave up on 500s after %s", at)
		}
	}

	// A 200 resets the first-refusal time: 20 h refused, answered, 20 h refused is not a day.
	st = recoverState{}
	status.Store(http.StatusForbidden)
	for at := time.Duration(0); at < 20*time.Hour; at += 6 * time.Minute {
		s.recoverStep(t0.Add(at), &st)
	}
	status.Store(http.StatusOK)
	s.recoverStep(t0.Add(20*time.Hour+10*time.Minute), &st)
	status.Store(http.StatusForbidden)
	for at := 20*time.Hour + 20*time.Minute; at < 40*time.Hour; at += 6 * time.Minute {
		if s.recoverStep(t0.Add(at), &st) {
			t.Fatalf("a 200 did not reset the count: gave up at %s", at)
		}
	}
}

// The refusal is typed: the status decides, never the words.
func TestRecoverRefusalIsTyped(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusForbidden)
	s, _, _ := newMeshServer(t, t.TempDir(), &pingFake{})
	s.cfg.Recover = RecoverConfig{Own: selfCert(t), Panel: panelAnswering(t, &status)}
	if err := s.recoverOnce(context.Background()); !errors.Is(err, errPanelRefused) {
		t.Fatalf("403 = %v, want errPanelRefused", err)
	}
	status.Store(http.StatusInternalServerError)
	if err := s.recoverOnce(context.Background()); err == nil || errors.Is(err, errPanelRefused) {
		t.Fatalf("500 = %v, want a plain error", err)
	}
}

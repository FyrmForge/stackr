package service

import (
	"context"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/clusterca"
	"github.com/FyrmForge/stackr/internal/worker"
)

// A worker offline through one controller cert renewal still opens the
// recovery answer (the previous cert co-signs); through two it does not. A
// revoke and break glass drop the previous cert, so the revoked one never
// vouches again.
func TestRecoverAnswerAllowListAndPreviousCert(t *testing.T) {
	ctx := context.Background()
	o, _ := ctlRig(t)
	own := nodeCert(t, o, "w1")
	roots := []*x509.Certificate{o.cluster.ca.Cert}
	n := 0
	open := func(refuses func(string) bool) error {
		t.Helper()
		n++
		nonce := "p" + string(rune('a'+n))
		req, err := worker.SignRecover(own, worker.RecoverPayload{Time: time.Now().Unix(), Nonce: nonce, Purpose: worker.PurposeRecover})
		must(t, err)
		a, err := o.RecoverNode(ctx, req)
		must(t, err)
		_, err = a.Open(roots, refuses, nonce, time.Now())
		return err
	}
	renew := func() string {
		t.Helper()
		ca, err := clusterca.NewCA(time.Now()) // the CA no longer vouches: due
		must(t, err)
		o.cluster.ca = ca
		roots = append(roots, ca.Cert)
		must(t, o.announceControllerCert(ctx))
		return ctlSerial(t, o)
	}
	prevFile := filepath.Join(o.cfg.DataDir, controllerPrevCert)

	s0 := ctlSerial(t, o)
	if err := open(allowOnly(s0)); err != nil {
		t.Fatalf("current cert: %v", err)
	}
	if err := open(allowOnly("ff")); err == nil {
		t.Fatal("an answer from an unlisted controller cert opened")
	}
	s1 := renew()
	if err := open(allowOnly(s0)); err != nil {
		t.Fatalf("offline through one renewal: %v", err)
	}
	s2 := renew()
	if err := open(allowOnly(s0)); err == nil {
		t.Fatal("offline through two renewals still opened")
	}
	if err := open(allowOnly(s1)); err != nil {
		t.Fatalf("previous cert: %v", err)
	}

	must(t, o.RevokeControllerCert(ctx, adminP()))
	o.trustAsync.Wait()
	if len(o.cluster.prevCert().Certificate) != 0 {
		t.Fatal("revoke kept a previous cert")
	}
	if _, err := os.Stat(prevFile); !os.IsNotExist(err) {
		t.Fatalf("previous cert file after revoke: %v", err)
	}
	if err := open(allowOnly(s2)); err == nil {
		t.Fatal("a worker listing only the revoked cert opened an answer")
	}
	if err := open(allowOnly(s1)); err == nil {
		t.Fatal("the cert before the revoked one still co-signed")
	}
	if err := open(allowOnly(ctlSerial(t, o))); err != nil {
		t.Fatalf("after revoke: %v", err)
	}

	renew()
	if len(o.cluster.prevCert().Certificate) == 0 {
		t.Fatal("renewal kept no previous cert")
	}
	must(t, o.BreakGlass(ctx, adminP()))
	if len(o.cluster.prevCert().Certificate) != 0 {
		t.Fatal("break glass kept a previous cert")
	}
	if _, err := os.Stat(prevFile); !os.IsNotExist(err) {
		t.Fatalf("previous cert file after break glass: %v", err)
	}
}

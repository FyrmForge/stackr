package service

import (
	"context"
	"crypto/x509"
	"slices"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/worker"
)

// A worker that trusts only the old root can verify the recover answer while
// the rollover is open (the new root is cross-signed by the old one) and
// learns both roots from it; once the old root is dropped it cannot.
func TestRecoverAnswerVerifiesDuringRolloverForOldRootWorker(t *testing.T) {
	ctx := context.Background()
	old := rolloverSettle
	rolloverSettle = 0
	defer func() { rolloverSettle = old }()
	o, _ := ctlRig(t)
	adm := adminP()
	oldRoots := []*x509.Certificate{o.cluster.ca.Cert}
	oldOwn := nodeCert(t, o, "w1") // from the old root, as the worker holds it
	// The worker missed the switch: its allow list holds the old serial only.
	refuses := allowOnly(ctlSerial(t, o))

	workerAsks(t, o, "w1") // a recorded old-root cert keeps the rollover open
	must(t, o.RotateRoot(ctx, adm))
	o.trustAsync.Wait()

	recoverAs := func(nonce string) worker.RecoverAnswer {
		t.Helper()
		req, err := worker.SignRecover(oldOwn, worker.RecoverPayload{Time: time.Now().Unix(), Nonce: nonce, Purpose: worker.PurposeRecover})
		must(t, err)
		a, err := o.RecoverNode(ctx, req)
		must(t, err)
		return a
	}
	a := recoverAs("c1")
	spec, err := a.Open(oldRoots, refuses, "c1", time.Now())
	if err != nil {
		t.Fatalf("an old-root worker cannot verify during the rollover: %v", err)
	}
	if len(spec.TrustRoots) != 2 {
		t.Fatalf("answer carries %d roots, want old and new", len(spec.TrustRoots))
	}
	// Without the cross cert or the previous cert's signature it is not a
	// way in: an attacker's own root is not vouched for by the old one.
	a.Cross, a.PrevCert, a.PrevSig = nil, "", nil
	if _, err := a.Open(oldRoots, nil, "c1", time.Now()); err == nil {
		t.Fatal("verified a new-root controller cert with no cross cert")
	}

	// Move w1 over and finish: the cross cert goes with the old root.
	workerAsks(t, o, "w1")
	must(t, o.FinishRollover(ctx, adm))
	oldOwn = nodeCert(t, o, "w1") // now from the new root
	b := recoverAs("c2")
	if len(b.Cross) != 0 || b.PrevCert != "" {
		t.Fatal("the cross cert outlived the old root")
	}
	if _, err := b.Open(oldRoots, refuses, "c2", time.Now()); err == nil {
		t.Fatal("an old-root worker verified an answer after the finish")
	}
}

// allowOnly is a worker's allow list holding serials.
func allowOnly(serials ...string) func(string) bool {
	return func(s string) bool { return !slices.Contains(serials, s) }
}

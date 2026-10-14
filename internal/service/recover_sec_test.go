package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/worker"
)

// A signature made for a renewal is no good as a recovery, and the other way
// round.
func TestSignedRequestsKeepTheirPurpose(t *testing.T) {
	o, own := renewRig(t)
	ctx := context.Background()
	renew, err := worker.SignRecover(own, worker.RecoverPayload{Time: time.Now().Unix(), Nonce: "p1", Purpose: worker.PurposeRenew, CSR: newCSR(t)})
	must(t, err)
	if _, err := o.RecoverNode(ctx, renew); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("a renewal replayed as a recovery: %v", err)
	}
	rec, err := worker.SignRecover(own, worker.RecoverPayload{Time: time.Now().Unix(), Nonce: "p2", Purpose: worker.PurposeRecover, CSR: newCSR(t)})
	must(t, err)
	if _, err := o.RenewNodeCert(ctx, rec); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("a recovery replayed as a renewal: %v", err)
	}
	none, err := worker.SignRecover(own, worker.RecoverPayload{Time: time.Now().Unix(), Nonce: "p3", CSR: newCSR(t)})
	must(t, err)
	if _, err := o.RenewNodeCert(ctx, none); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("a request with no purpose was accepted: %v", err)
	}
}

// A node waiting for its rejoin neither renews nor recovers.
func TestNeedsRejoinNodeCannotRenewOrRecover(t *testing.T) {
	o, own := renewRig(t)
	ctx := context.Background()
	n, err := o.store.Nodes.Get(ctx, "w1")
	must(t, err)
	n.Status = StateNeedsRejoin
	must(t, o.store.Nodes.Update(ctx, n))

	renew, err := worker.SignRecover(own, worker.RecoverPayload{Time: time.Now().Unix(), Nonce: "q1", Purpose: worker.PurposeRenew, CSR: newCSR(t)})
	must(t, err)
	if _, err := o.RenewNodeCert(ctx, renew); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("renew: %v", err)
	}
	if _, err := o.renewNodeCert(ctx, "w1", []byte(newCSR(t)), "tunnel"); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("renew over the tunnel: %v", err)
	}
	rec, err := worker.SignRecover(own, worker.RecoverPayload{Time: time.Now().Unix(), Nonce: "q2", Purpose: worker.PurposeRecover})
	must(t, err)
	if _, err := o.RecoverNode(ctx, rec); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("recover: %v", err)
	}
}

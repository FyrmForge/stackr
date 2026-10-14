package service

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/FyrmForge/stackr/internal/clusterca"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/securitylog"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// The controller cert allow list (docs/rewrite/tasks/ca-rollover.md, C3).
// Workers accept only the controller serials in their mesh spec. A renewal
// announces current and next first, switches the controller to the next cert
// once every reachable worker took the list, then announces the next alone.
// Revoking is an immediate renewal: the old serial leaves the list.

var (
	// controllerAckBound is how long a switch waits for workers to take the
	// list that holds the next serial; a worker that never does cannot block
	// a renewal longer (it recovers through the panel fetch). Vars for tests.
	controllerAckBound = 5 * time.Minute
	controllerAckEvery = 5 * time.Second
)

// announceControllerCert keeps the announced controller serials true: it
// renews the controller cert when due (or issued by a root that is no longer
// the current one) and otherwise re-announces the current serial. Called at
// boot and daily from dailyChores; a repeat changes and pushes nothing.
func (o *Orchestrator) announceControllerCert(ctx context.Context) error {
	if o.cluster == nil {
		return nil
	}
	o.cluster.rollMu.Lock()
	defer o.cluster.rollMu.Unlock()
	return o.rollController(ctx, nil, false)
}

// RevokeControllerCert replaces the controller cert now: the next serial is
// announced beside the old one, the controller switches, and the old serial
// leaves the list and is revoked, so every worker refuses it. Audited to by.
// Returns once started; the switch lands within controllerAckBound.
func (o *Orchestrator) RevokeControllerCert(ctx context.Context, by *Principal) error {
	if err := nodeAdmin(by); err != nil {
		return err
	}
	if o.cluster == nil {
		return errors.ErrUnsupported
	}
	if !o.cluster.revoking.CompareAndSwap(false, true) {
		return errs.Conflictf("The controller certificate is being replaced. Try again in a few minutes.")
	}
	// Waits up to controllerAckBound for every worker: past the request.
	o.detach(ctx, func(ctx context.Context) {
		defer o.cluster.revoking.Store(false)
		o.cluster.rollMu.Lock()
		defer o.cluster.rollMu.Unlock()
		if err := o.rollController(ctx, by, true); err != nil {
			slog.Warn("cluster: revoke controller cert", "err", err)
		}
	})
	return nil
}

// rollController holds rollMu. revoke forces a renewal and revokes the old
// cert; otherwise a renewal runs when the cert is due or not from the
// current root (C2 swaps o.cluster.ca, then calls announceControllerCert).
func (o *Orchestrator) rollController(ctx context.Context, by *Principal, revoke bool) error {
	c := o.cluster
	cur := c.cert()
	curSerial, err := certSerial(cur)
	if err != nil {
		return err
	}
	leaf, err := x509.ParseCertificate(cur.Certificate[0])
	if err != nil {
		return err
	}
	if !revoke && !clusterca.NeedsRenewal(leaf, time.Now()) && leaf.CheckSignatureFrom(c.ca.Cert) == nil {
		o.announce(ctx, curSerial)
		return nil
	}
	next, err := issueController(ctx, c.st, c.ca)
	if err != nil {
		return fmt.Errorf("controller cert: %w", err)
	}
	// Current and next, until every reachable worker has the list.
	o.announceAcked(ctx, curSerial, next.serial)
	if err := next.save(o.cfg.DataDir); err != nil {
		return err
	}
	c.mu.Lock()
	c.own = next.pair
	// Clients are rebuilt with the new cert on the next refresh; the router
	// keeps serving through the old ones until then.
	clear(c.addr)
	clear(c.ext)
	clear(c.probe)
	c.mu.Unlock()
	o.announce(ctx, next.serial)

	ev := securitylog.Event{
		Action: securitylog.ControllerCertRenew, TargetKind: "cert", TargetID: curSerial,
		Detail: "serial " + curSerial + " replaced by " + next.serial,
	}
	if revoke {
		ev.Action = securitylog.ControllerCertRevoke
	}
	if by != nil {
		ev.ActorID, ev.KeyID = by.User.ID, by.KeyID
	}
	err = c.st.Tx(ctx, func(tx store.Tx) error {
		if revoke {
			if err := tx.NodeCerts.Revoke(ctx, curSerial, time.Now().UTC()); err != nil && !errors.Is(err, errs.ErrNotFound) {
				return err
			}
		}
		return audit(tx).Append(ctx, ev)
	})
	if err != nil {
		return err
	}
	slog.Info("cluster: controller cert replaced", "revoked", revoke)
	return nil
}

// announce sets the list and tells the workers once, without waiting.
func (o *Orchestrator) announce(ctx context.Context, serials ...string) {
	o.cluster.setSerials(serials)
	if o.mesh == nil {
		return
	}
	if err := o.mesh.Sync(ctx, "", false); err != nil {
		slog.Warn("cluster: controller serials push incomplete, workers catch up on the next sync", "err", err)
	}
}

// announceAcked sets the list and pushes until every reachable worker took
// it (ApplyMesh answered), or controllerAckBound passes. A worker that is
// not online has no client and counts as unreachable.
func (o *Orchestrator) announceAcked(ctx context.Context, serials ...string) {
	o.cluster.setSerials(serials)
	if o.mesh == nil {
		return
	}
	deadline := time.Now().Add(controllerAckBound)
	for {
		err := o.mesh.Sync(ctx, "", false)
		if err == nil {
			return
		}
		if ctx.Err() != nil || !time.Now().Add(controllerAckEvery).Before(deadline) {
			slog.Warn("cluster: switching the controller cert before every worker took the list", "err", err)
			return
		}
		select {
		case <-ctx.Done():
		case <-time.After(controllerAckEvery):
		}
	}
}

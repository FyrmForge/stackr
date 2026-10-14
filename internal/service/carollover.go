package service

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/FyrmForge/stackr/internal/clusterca"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/securitylog"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Planned root rollover (docs/rewrite/tasks/ca-rollover.md, C2).
//
// The controller makes a new root and trusts old and new at once (the trust
// list rides every worker's mesh spec). From then on it signs only with the
// new root. A worker cannot be handed a cert (its key never leaves it), so
// each one asks for a renewal and gets one from the new root (certrenew.go);
// the controller's own cert is reissued at once. When every worker's newest
// cert is from the new root, the old root is dropped from the list and from
// disk. The disk is the truth: a restart resumes from clusterca.LoadPrevCA,
// and reconcileRollover mends cluster_ca when a crash left it behind.

const (
	// rolloverLead is how long before the root runs out the daily tick
	// starts a rollover.
	rolloverLead = 365 * 24 * time.Hour
)

// rolloverSettle is how long a new-root cert must have existed before its
// node counts as moved: a worker that lost the renewal answer still holds
// its old cert and keeps asking, and dropping the old root first would
// strand it. A var so tests need not wait.
var rolloverSettle = 6 * time.Hour

// rolloverMu serializes RotateRoot, the forced finish and the tick.
// ponytail: one process-wide lock; there is one controller and a rollover is
// rare.
var rolloverMu sync.Mutex

// rolloverDetached is set while RotateRoot's pushes run past the request
// (holding rolloverMu): a second RotateRoot is refused, not blocked.
var rolloverDetached atomic.Bool

// swapCA makes ca the signing root and roots the trust list in one step.
func (c *clusterPKI) swapCA(ca *clusterca.CA, roots []*x509.Certificate) {
	c.mu.Lock()
	c.ca, c.trust = ca, roots
	c.mu.Unlock()
}

// RotateRoot starts a planned rollover of the cluster root and returns once
// the first step is durable (the pushes run detached); the daily
// rootRolloverTick resumes the rest.
// Refuses while a rollover runs; node admin only.
func (o *Orchestrator) RotateRoot(ctx context.Context, by *Principal) error {
	if err := nodeAdmin(by); err != nil {
		return err
	}
	if rolloverDetached.Load() {
		return errs.Conflictf("A root rollover is already running.")
	}
	rolloverMu.Lock()
	st, err := o.startRollover(ctx, by)
	if err != nil {
		rolloverMu.Unlock()
		return err
	}
	rolloverDetached.Store(true)
	// The pushes wait up to controllerAckBound for every worker (an offline
	// one never answers): past the request, so they run detached.
	o.detach(ctx, func(ctx context.Context) {
		defer rolloverMu.Unlock()
		defer rolloverDetached.Store(false)
		if err := o.advanceRollover(ctx, by, st, true); err != nil {
			slog.Warn("cluster: root rollover", "err", err)
		}
	})
	return nil
}

// detach runs f past the request that started it: not cancelled with the
// request, cancelled when the panel stops, waited for by Close.
func (o *Orchestrator) detach(ctx context.Context, f func(context.Context)) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	o.trustAsync.Add(1)
	go func() {
		select {
		case <-o.stopNodes:
			cancel()
		case <-ctx.Done():
		}
	}()
	go func() {
		defer o.trustAsync.Done()
		defer cancel()
		f(ctx)
	}()
}

// FinishRollover (node admin) drops the old root now. Workers still
// holding an old-root cert can no longer be reached: they are marked
// needs-rejoin and their certs revoked, each in the security log.
func (o *Orchestrator) FinishRollover(ctx context.Context, by *Principal) error {
	uid, kid := actorOf(by)
	if err := nodeAdmin(by); err != nil {
		return err
	}
	if o.cluster == nil {
		return errs.Conflictf("The cluster CA is not set up on this panel yet.")
	}
	if rolloverDetached.Load() { // its pushes can wait minutes on an offline worker
		return errs.Conflictf("The rollover is still sending the trust list. Try again in a few minutes.")
	}
	rolloverMu.Lock()
	defer rolloverMu.Unlock()
	st, err := o.reconcileRollover(ctx)
	if err != nil {
		return err
	}
	if !st.RollingOver() {
		return errs.Conflictf("No root rollover is running.")
	}
	old, err := o.rootStragglers(ctx, st.CurrentFP, 0)
	if err != nil {
		return err
	}
	for _, n := range old {
		err := o.store.Tx(ctx, func(tx store.Tx) error {
			n.Status = StateNeedsRejoin
			if err := tx.Nodes.Update(ctx, n); err != nil {
				return err
			}
			if _, err := tx.NodeCerts.RevokeNode(ctx, n.ID, time.Now().UTC()); err != nil {
				return err
			}
			return audit(tx).Append(ctx, securitylog.Event{
				ActorID: uid, KeyID: kid,
				Action: securitylog.RootRolloverNode, TargetKind: "node", TargetID: n.ID,
				Detail: n.Name + " still on the old root, needs a rejoin",
			})
		})
		if err != nil {
			return err
		}
	}
	return o.finishRollover(ctx, by, st, len(old) > 0)
}

// rootRolloverTick is the daily root check, run from the same loop that
// renews the controller cert. A rollover running is advanced (this is what
// survives a panel restart); otherwise one starts when the current root has
// under a year left. A tick with nothing to do writes nothing.
func (o *Orchestrator) rootRolloverTick(ctx context.Context) error {
	if o.cluster == nil {
		return nil
	}
	rolloverMu.Lock()
	defer rolloverMu.Unlock()
	st, err := o.reconcileRollover(ctx)
	if err != nil {
		return err
	}
	if st.RollingOver() {
		return o.advanceRollover(ctx, nil, st, false)
	}
	if time.Until(o.cluster.ca.Cert.NotAfter) < rolloverLead {
		st, err := o.startRollover(ctx, nil)
		if err != nil {
			return err
		}
		return o.advanceRollover(ctx, nil, st, true)
	}
	return nil
}

// actorOf is who an event names; nil is the system (the tick).
func actorOf(by *Principal) (userID, keyID string) {
	if by == nil {
		return "", ""
	}
	return by.User.ID, by.KeyID
}

// startRollover is RotateRoot with the lock held and the caller vetted.
func (o *Orchestrator) startRollover(ctx context.Context, by *Principal) (store.ClusterCA, error) {
	uid, kid := actorOf(by)
	if o.cluster == nil {
		return store.ClusterCA{}, errs.Conflictf("The cluster CA is not set up on this panel yet.")
	}
	st, err := o.reconcileRollover(ctx)
	if err != nil {
		return st, err
	}
	if st.RollingOver() {
		return st, errs.Conflictf("A root rollover is already running.")
	}
	next, err := clusterca.NewCA(time.Now())
	if err != nil {
		return st, err
	}
	old := o.cluster.ca
	// The cross cert first: once RotateCA moved the old key, a failure in
	// between could never write it again.
	if err := writeCross(o.cfg.DataDir, old, next); err != nil {
		return st, err
	}
	if err := clusterca.RotateCA(o.cfg.DataDir, next); err != nil {
		_ = dropCross(o.cfg.DataDir)
		return st, err
	}
	// Sign with the new root from here, trust both.
	o.cluster.swapCA(next, []*x509.Certificate{next.Cert, old.Cert})
	now := time.Now().UTC()
	st = store.ClusterCA{
		CurrentFP: clusterca.Fingerprint(next.Cert), PreviousFP: clusterca.Fingerprint(old.Cert),
		RolloverStartedAt: &now, BreakGlassAt: st.BreakGlassAt,
	}
	err = o.store.Tx(ctx, func(tx store.Tx) error {
		if err := tx.ClusterCA.Save(ctx, st); err != nil {
			return err
		}
		return audit(tx).Append(ctx, securitylog.Event{
			ActorID: uid, KeyID: kid,
			Action: securitylog.RootRolloverStart, TargetKind: "cluster_ca", TargetID: st.CurrentFP,
			Detail: "new root " + st.CurrentFP + ", old root " + st.PreviousFP + " stays trusted until every node moved",
		})
	})
	if err != nil {
		return st, err
	}
	slog.Info("cluster: root rollover started", "root", st.CurrentFP)
	return st, nil
}

// advanceRollover does the steps that are safe to repeat: the controller
// cert from the new root, the trust list to the workers, and the finish once
// no worker is left on the old root. first audits the trust push (the tick
// pushes quietly).
func (o *Orchestrator) advanceRollover(ctx context.Context, by *Principal, st store.ClusterCA, first bool) error {
	uid, kid := actorOf(by)
	// Offline workers miss this push and get the list from their recovery
	// fetch; a failed sync is retried by the next tick.
	if o.mesh != nil {
		if err := o.mesh.Sync(ctx, "", true); err != nil {
			slog.Warn("cluster: root rollover trust push", "err", err)
		}
	}
	// Workers trust both roots now; only then does the controller switch to a
	// cert from the new one (announce both, switch, announce next).
	if err := o.announceControllerCert(ctx); err != nil {
		return err
	}
	if first {
		err := o.store.Tx(ctx, func(tx store.Tx) error {
			return audit(tx).Append(ctx, securitylog.Event{
				ActorID: uid, KeyID: kid,
				Action: securitylog.RootRolloverPush, TargetKind: "cluster_ca", TargetID: st.CurrentFP,
				Detail: "trust list old and new sent to the workers",
			})
		})
		if err != nil {
			return err
		}
	}
	left, err := o.rootStragglers(ctx, st.CurrentFP, rolloverSettle)
	if err != nil {
		return err
	}
	if len(left) > 0 {
		return nil
	}
	return o.finishRollover(ctx, by, st, false)
}

// finishRollover drops the old root: from disk first (a crash after it is
// mended by reconcileRollover), then from the trust list and cluster_ca.
func (o *Orchestrator) finishRollover(ctx context.Context, by *Principal, st store.ClusterCA, forced bool) error {
	uid, kid := actorOf(by)
	if err := clusterca.DropPrevCA(o.cfg.DataDir); err != nil {
		return err
	}
	if err := dropCross(o.cfg.DataDir); err != nil {
		return err
	}
	o.cluster.swapCA(o.cluster.ca, []*x509.Certificate{o.cluster.ca.Cert})
	now := time.Now().UTC()
	prev := st.PreviousFP
	st.PreviousFP, st.RolloverFinishedAt = "", &now
	detail := "old root " + prev + " dropped, every node moved"
	if forced {
		detail = "old root " + prev + " dropped by an admin, nodes left on it need a rejoin"
	}
	err := o.store.Tx(ctx, func(tx store.Tx) error {
		if err := tx.ClusterCA.Save(ctx, st); err != nil {
			return err
		}
		return audit(tx).Append(ctx, securitylog.Event{
			ActorID: uid, KeyID: kid,
			Action: securitylog.RootRolloverFinish, TargetKind: "cluster_ca", TargetID: st.CurrentFP, Detail: detail,
		})
	})
	if err != nil {
		return err
	}
	slog.Info("cluster: root rollover finished", "root", st.CurrentFP, "forced", forced)
	if o.mesh != nil {
		if err := o.mesh.Sync(ctx, "", true); err != nil {
			slog.Warn("cluster: root rollover final push", "err", err)
		}
	}
	return nil
}

// reconcileRollover reads cluster_ca and mends it against the key files,
// which are the truth: a previous root on disk with no rollover recorded
// (crash after RotateCA) starts the record; a rollover recorded with no
// previous root on disk (crash after DropPrevCA) finishes it.
func (o *Orchestrator) reconcileRollover(ctx context.Context) (store.ClusterCA, error) {
	st, _, err := o.store.ClusterCA.Get(ctx)
	if err != nil {
		return st, err
	}
	prev, err := clusterca.LoadPrevCA(o.cfg.DataDir)
	hasPrev := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return st, err
	}
	cur := o.cluster.ca.Cert
	switch {
	case hasPrev && !st.RollingOver():
		now := time.Now().UTC()
		st.CurrentFP, st.PreviousFP, st.RolloverStartedAt = clusterca.Fingerprint(cur), clusterca.Fingerprint(prev), &now
		o.cluster.setRoots([]*x509.Certificate{cur, prev})
	case !hasPrev && st.RollingOver():
		now := time.Now().UTC()
		st.PreviousFP, st.RolloverFinishedAt = "", &now
		_ = dropCross(o.cfg.DataDir)
		o.cluster.setRoots([]*x509.Certificate{cur})
	default:
		return st, nil
	}
	return st, o.store.ClusterCA.Save(ctx, st)
}

// rootStragglers are the confirmed workers whose newest live cert is not from
// the root curFP, or is from it but younger than settle. A node waiting for a
// rejoin gets a new-root cert when it is confirmed, and a node whose newest
// cert has run out cannot renew either way: neither holds the rollover up.
func (o *Orchestrator) rootStragglers(ctx context.Context, curFP string, settle time.Duration) ([]store.Node, error) {
	ns, err := o.store.Nodes.List(ctx)
	if err != nil {
		return nil, err
	}
	cs, err := o.store.NodeCerts.ListLive(ctx)
	if err != nil {
		return nil, err
	}
	newest := map[string]store.NodeCert{} // oldest first, so the last wins
	for _, c := range cs {
		if c.NodeID != "" {
			newest[c.NodeID] = c
		}
	}
	now := time.Now()
	var out []store.Node
	for _, n := range ns {
		c, ok := newest[n.ID]
		if n.Role != node.Worker || n.Status == node.StatusPending || n.Status == StateNeedsRejoin || !ok ||
			c.NotAfter.Before(now.Add(-clusterca.ClockSlack)) {
			continue
		}
		if c.Issuer != curFP || now.Sub(c.CreatedAt) < settle {
			out = append(out, n)
		}
	}
	return out, nil
}

// crossFile is the new root signed by the old one, kept for the length of a
// rollover so a recover answer can carry it (worker.RecoverAnswer.Cross).
const crossFile = "keys/cluster-ca.cross.crt"

// writeCross cross-signs next's key with old's, before old's key is gone.
func writeCross(dir string, old, next *clusterca.CA) error {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return err
	}
	tpl := &x509.Certificate{
		SerialNumber: serial, Subject: next.Cert.Subject,
		NotBefore: next.Cert.NotBefore, NotAfter: old.Cert.NotAfter,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign, BasicConstraintsValid: true, IsCA: true, MaxPathLenZero: true,
	}
	if next.Cert.NotAfter.Before(tpl.NotAfter) {
		tpl.NotAfter = next.Cert.NotAfter
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, old.Cert, next.Cert.PublicKey, old.Key)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, crossFile), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}

// readCross is the cross cert as PEM, nil outside a rollover.
func readCross(dir string) []string {
	b, err := os.ReadFile(filepath.Join(dir, crossFile))
	if err != nil {
		return nil
	}
	return []string{string(b)}
}

func dropCross(dir string) error {
	if err := os.Remove(filepath.Join(dir, crossFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

package service

import (
	"context"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/FyrmForge/stackr/internal/clusterca"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/securitylog"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// breakGlassGap is how soon after one break glass another is refused: a
// double click or a retry would wipe the root the rejoin commands name.
// ponytail: no force flag; wait it out, or add one when someone needs it.
const breakGlassGap = 10 * time.Minute

// breakGlassPushBound is how long the last trust push may take in all.
const breakGlassPushBound = 2 * time.Minute

// pushTrust sends every worker its full spec once more with the trust fields
// replaced, whatever the plan says. Break glass uses it, over the old cert,
// just before the controller switches to the new root.
func (m *meshSvc) pushTrust(ctx context.Context, roots, serials []string) error {
	m.syncMu.Lock()
	defer m.syncMu.Unlock()
	in, specs, err := m.plan(ctx)
	if err != nil {
		return err
	}
	var ids []string
	for id, sp := range specs {
		if id != m.ctlID {
			sp.TrustRoots, sp.ControllerSerials = roots, serials
			specs[id] = sp
			ids = append(ids, id)
		}
	}
	return m.push(ctx, in, specs, ids, true)
}

// BreakGlass wipes the cluster's trust and every WireGuard key in one
// action (docs/rewrite/tasks/ca-rollover.md, C4): a new root with no overlap,
// a new controller cert and mesh key, and every worker cut off in
// needs-rejoin until its own rejoin command (RejoinNode) is used on the box.
// The workers keep name, envs and addresses; their apps keep running where
// they are. The panel's sessions and API keys are not touched.
func (o *Orchestrator) BreakGlass(ctx context.Context, by *Principal) error {
	if err := nodeAdmin(by); err != nil {
		return err
	}
	if o.cluster == nil {
		return errs.Conflictf("this install runs no cluster")
	}
	if rolloverDetached.Load() { // its pushes hold rolloverMu for minutes with a worker offline
		return errs.Conflictf("A root rollover is still running. Try again in a few minutes.")
	}
	rolloverMu.Lock() // a rollover tick or finish must not bring the old root back
	defer rolloverMu.Unlock()
	if o.cluster.revoking.Load() { // it holds rollMu for minutes with a worker offline
		return errs.Conflictf("The controller certificate is being replaced. Try again in a few minutes.")
	}
	cur, _, err := o.store.ClusterCA.Get(ctx)
	if err != nil {
		return err
	}
	if cur.BreakGlassAt != nil && time.Since(*cur.BreakGlassAt) < breakGlassGap {
		return errs.Conflictf("Break glass ran %s ago; the rejoin commands name its root. Wait %d minutes.",
			time.Since(*cur.BreakGlassAt).Round(time.Second), int(breakGlassGap.Minutes()))
	}
	o.breakGlassAudit(ctx, by, "started")

	// Files first: a crash after this leaves a root nobody holds a cert for,
	// and running it again finishes the job (the gap only counts a row).
	now := time.Now()
	ca, err := clusterca.NewCA(now)
	if err != nil {
		return err
	}
	if err := clusterca.SaveCA(o.cfg.DataDir, ca); err != nil {
		return err
	}
	if err := clusterca.DropPrevCA(o.cfg.DataDir); err != nil {
		return err
	}
	c := o.cluster
	c.rollMu.Lock() // no controller cert renewal in between
	defer c.rollMu.Unlock()
	next, err := issueController(ctx, c.st, ca) // a new controller cert from the new root
	if err != nil {
		return err
	}
	if err := next.save(o.cfg.DataDir); err != nil {
		return err
	}
	// The workers still trust the old root and the old controller cert, which
	// may be what leaked: tell every reachable one, over the old cert, to
	// trust only the new root and the new controller cert. Unreachable ones
	// are covered by their rejoin.
	if o.mesh != nil {
		pctx, cancel := context.WithTimeout(ctx, breakGlassPushBound)
		err := o.mesh.pushTrust(pctx, clusterca.RootsToPEM([]*x509.Certificate{ca.Cert}), []string{next.serial})
		cancel()
		if err != nil {
			slog.Warn("break glass: some workers did not take the new root; they need their rejoin", "err", err)
		}
	}
	c.mu.Lock()
	c.ca, c.trust, c.own = ca, []*x509.Certificate{ca.Cert}, next.pair
	clear(c.rev)
	clear(c.addr) // clients are rebuilt with the new cert on the next refresh
	clear(c.ext)
	clear(c.probe)
	c.mu.Unlock()
	c.setSerials([]string{next.serial}) // no old controller cert is accepted anywhere

	// One transaction: the row, the cut-off workers, the dead certs and
	// commands. A worker's own mesh key is replaced when it rejoins.
	var cut int
	err = o.store.Tx(ctx, func(tx store.Tx) error {
		// A request spent before the incident may belong to whoever stole the
		// token; it must not be confirmable during the mass rejoin.
		rows, err := o.joinTokens(tx).Used(ctx)
		if err != nil {
			return err
		}
		for _, r := range rows {
			if r.RejectedAt != nil || r.CertPEM != "" || r.NodeID == "" {
				continue
			}
			if _, err := node.New(tx.Nodes).Reject(ctx, r.NodeID, StateNeedsRejoin); err != nil && !errors.Is(err, errs.ErrNotFound) {
				return err
			}
			if err := o.joinTokens(tx).SetRejected(ctx, r, now.UTC()); err != nil {
				return err
			}
		}
		ns, err := tx.Nodes.List(ctx)
		if err != nil {
			return err
		}
		for _, n := range ns {
			if n.Role != node.Worker || n.Status == node.StatusPending {
				continue
			}
			if _, err := tx.NodeCerts.RevokeNode(ctx, n.ID, now.UTC()); err != nil {
				return err
			}
			n.Status = StateNeedsRejoin
			if err := tx.Nodes.Update(ctx, n); err != nil {
				return err
			}
			cut++
		}
		if _, err := tx.JoinTokens.ExpireUnspent(ctx, now.UTC()); err != nil { // commands minted for the old root
			return err
		}
		t := now.UTC()
		if err := tx.ClusterCA.Save(ctx, store.ClusterCA{CurrentFP: clusterca.Fingerprint(ca.Cert), BreakGlassAt: &t}); err != nil {
			return err
		}
		return audit(tx).Append(ctx, securitylog.Event{
			Action: securitylog.CABreakGlass, ActorID: by.User.ID, KeyID: by.KeyID, TargetKind: "cluster_ca",
			TargetID: clusterca.Fingerprint(ca.Cert), Detail: fmt.Sprintf("done: new root, %d workers need a rejoin", cut),
		})
	})
	if err != nil {
		return err
	}

	// The controller's own mesh key. Workers are out of the mesh plan now, so
	// this waits for nobody.
	if o.localWorker != nil && o.mesh != nil {
		ns, err := o.store.Nodes.List(ctx)
		if err != nil {
			return err
		}
		for _, n := range ns {
			if n.Role == node.Controller {
				if err := o.rotateController(ctx, n, "break glass"); err != nil {
					return fmt.Errorf("break glass: controller mesh key: %w", err)
				}
			}
		}
	}
	o.kickMesh()
	return nil
}

func (o *Orchestrator) breakGlassAudit(ctx context.Context, by *Principal, what string) {
	_ = o.store.Tx(ctx, func(tx store.Tx) error {
		return audit(tx).Append(ctx, securitylog.Event{
			Action: securitylog.CABreakGlass, ActorID: by.User.ID, KeyID: by.KeyID, TargetKind: "cluster_ca", Detail: what,
		})
	})
}

// serialOf is a cert's serial as node_certs and the announced list hold it.
func serialOf(c *x509.Certificate) string { return hex.EncodeToString(c.SerialNumber.Bytes()) }

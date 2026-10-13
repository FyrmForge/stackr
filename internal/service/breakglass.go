package service

import (
	"context"
	"crypto/x509"
	"encoding/hex"
	"fmt"
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

// BreakGlassOut is where a break glass stands: when it happened and the
// workers that have not rejoined yet (C5 shows it; RejoinNode mints each
// node's command).
type BreakGlassOut struct {
	At          *time.Time `json:"at"`
	NotRejoined []string   `json:"not_rejoined"`
}

// BreakGlassState answers the last break glass and which workers still wait
// for a rejoin. At is nil when there was none.
func (o *Orchestrator) BreakGlassState(ctx context.Context, by *Principal) (BreakGlassOut, error) {
	if err := nodeAdmin(by); err != nil {
		return BreakGlassOut{}, err
	}
	row, _, err := o.store.ClusterCA.Get(ctx)
	if err != nil {
		return BreakGlassOut{}, err
	}
	ns, err := o.store.Nodes.List(ctx)
	if err != nil {
		return BreakGlassOut{}, err
	}
	out := BreakGlassOut{At: row.BreakGlassAt, NotRejoined: []string{}}
	for _, n := range ns {
		if n.Role == node.Worker && n.Status == StateNeedsRejoin {
			out.NotRejoined = append(out.NotRejoined, n.Name)
		}
	}
	return out, nil
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
	c.mu.Lock()
	c.ca, c.trust = ca, []*x509.Certificate{ca.Cert}
	clear(c.rev)
	c.mu.Unlock()
	if _, err := c.renew(ctx, o.cfg.DataDir); err != nil { // a new controller cert from the new root
		return err
	}
	if own, err := x509.ParseCertificate(c.cert().Certificate[0]); err == nil {
		c.setSerials([]string{serialOf(own)}) // no old controller cert is accepted anywhere
	}

	// One transaction: the row, the cut-off workers, the dead certs and
	// commands. A worker's own mesh key is replaced when it rejoins.
	ns, err := o.store.Nodes.List(ctx)
	if err != nil {
		return err
	}
	var cut int
	err = o.store.Tx(ctx, func(tx store.Tx) error {
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

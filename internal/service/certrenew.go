package service

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/FyrmForge/stackr/internal/clusterca"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/securitylog"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/worker"
)

// Node cert renewal, the controller's half (the worker's is worker/cert.go).
// A worker in its cert's last third sends a CSR over the tunnel, its current
// cert as the client cert; with the tunnel down, the same CSR signed with its
// current key to the panel. Either way a live, unrevoked worker cert of this
// CA is the only credential, and the new cert names the same node. The old
// cert is not revoked: it runs out within its last third, and revoking it
// before the node is known to use the new one could strand it.

// NodeCertExpired is the shown status of a worker whose newest cert has run
// out: it can no longer renew and must be rejoined.
const NodeCertExpired = "cert-expired"

// renewNodeCert issues the renewal for nodeID and audits it with how it came.
func (o *Orchestrator) renewNodeCert(ctx context.Context, nodeID string, csr []byte, via string) ([]byte, error) {
	n, err := o.store.Nodes.Get(ctx, nodeID)
	if err != nil || n.Role != node.Worker || n.Status == node.StatusPending {
		return nil, errs.Refusedf("not a confirmed worker")
	}
	iss, err := o.cluster.ca.IssueNode(csr, nodeID, time.Now())
	if err != nil {
		return nil, errs.Refusedf("certificate request: %v", err)
	}
	err = o.store.Tx(ctx, func(tx store.Tx) error {
		if err := tx.NodeCerts.Create(ctx, store.NodeCert{
			Serial: iss.Serial, NodeID: nodeID, Role: iss.Role, NotAfter: iss.NotAfter, CreatedAt: time.Now().UTC(),
		}); err != nil {
			return err
		}
		return audit(tx).Append(ctx, securitylog.Event{
			Action: securitylog.NodeCertRenew, TargetKind: "node", TargetID: nodeID,
			Detail: n.Name + ": serial " + iss.Serial + " via " + via + ", until " + iss.NotAfter.UTC().Format(time.DateOnly),
		})
	})
	if err != nil {
		return nil, err
	}
	slog.Info("cluster: node cert renewed", "node", n.Name, "via", via)
	return iss.CertPEM, nil
}

// renewTunnel is RenewTunnelPath on the controller's mesh listener: the
// client cert was checked by the listener (CA, worker role, not revoked).
func (o *Orchestrator) renewTunnel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		http.Error(w, "worker cert required", http.StatusForbidden)
		return
	}
	peer, err := clusterca.Verify(o.cluster.ca.Cert, r.TLS.PeerCertificates, clusterca.RoleWorker, "", time.Now())
	if err != nil {
		http.Error(w, "worker cert required", http.StatusForbidden)
		return
	}
	var in worker.RenewRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	cert, err := o.renewNodeCert(r.Context(), peer.NodeID, []byte(in.CSR), "tunnel")
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(worker.RenewAnswer{Cert: string(cert)})
}

// RenewNodeCert (public, rate limited) is a renewal through the panel: the
// request is signed like a recovery call and checked the same way.
func (o *Orchestrator) RenewNodeCert(ctx context.Context, req worker.RecoverRequest) (worker.RenewAnswer, error) {
	peer, p, err := o.openSigned(req, "renew/", nil)
	if err != nil {
		return worker.RenewAnswer{}, err
	}
	if p.CSR == "" {
		return worker.RenewAnswer{}, errs.Refusedf("not a node of this cluster")
	}
	c, err := o.renewNodeCert(ctx, peer.NodeID, []byte(p.CSR), "panel")
	if err != nil {
		return worker.RenewAnswer{}, err
	}
	return worker.RenewAnswer{Cert: string(c)}, nil
}

// certExpired is whether a worker's newest cert has run out (no renewal can
// reach it any more). A read error says no: the status falls back to the row.
func (o *Orchestrator) certExpired(nodeID string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cs, err := o.store.NodeCerts.ListByNode(ctx, nodeID)
	if err != nil || len(cs) == 0 {
		return false
	}
	var newest time.Time
	for _, c := range cs {
		if c.RevokedAt == nil && c.NotAfter.After(newest) {
			newest = c.NotAfter
		}
	}
	return !newest.IsZero() && time.Now().Add(-clusterca.ClockSlack).After(newest)
}

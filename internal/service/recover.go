package service

import (
	"context"
	"crypto/x509"
	"log/slog"
	"sync"
	"time"

	"github.com/FyrmForge/stackr/internal/clusterca"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/securitylog"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/worker"
)

// recoverSkew is how far a recovery call's clock may be off; a nonce is
// remembered twice that long, so a replay inside the window is refused.
const recoverSkew = time.Minute

// recoverNonces are the nonces seen inside the window.
type recoverNonces struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

// fresh records nonce and says whether it was new.
func (n *recoverNonces) fresh(nonce string, now time.Time) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.seen == nil {
		n.seen = map[string]time.Time{}
	}
	for k, at := range n.seen {
		if now.Sub(at) > 2*recoverSkew {
			delete(n.seen, k)
		}
	}
	if _, ok := n.seen[nonce]; ok {
		return false
	}
	n.seen[nonce] = now
	return true
}

// RecoverNode (public, rate limited) is a worker whose controller went quiet
// calling the panel's public URL: the request is signed with its node key,
// its cert must be the cluster CA's, a worker's and not revoked. Its facts
// are taken as if read over the tunnel, the mesh is recomputed and pushed,
// and the answer is the node's own mesh spec.
func (o *Orchestrator) RecoverNode(ctx context.Context, req worker.RecoverRequest) (worker.MeshSpec, error) {
	refused := errs.Refusedf("not a node of this cluster")
	if o.mesh == nil {
		return worker.MeshSpec{}, refused
	}
	peer, p, err := o.openSigned(req, "", func(nodeID, detail string) { o.auditRecover(ctx, nodeID, detail) })
	if err != nil {
		return worker.MeshSpec{}, err
	}
	n, err := o.store.Nodes.Get(ctx, peer.NodeID)
	if err != nil || n.Role != node.Worker || n.Status == node.StatusPending {
		o.auditRecover(ctx, peer.NodeID, "refused: not a confirmed worker")
		return worker.MeshSpec{}, refused
	}
	force := false
	if p.WGPublic != "" && p.WGPublic != n.WGPublicKey {
		if err := o.takeNodeKeys(ctx, n, p.WGPublic, p.ExitPublic); err != nil {
			o.auditRecover(ctx, n.ID, "refused: bad keys")
			return worker.MeshSpec{}, err
		}
		force = true // every peer learns the new key now
	}
	o.mesh.Report(ctx, n.ID, p.Facts)
	if err := o.mesh.Sync(ctx, "", force); err != nil {
		// Pushes to other nodes may fail; the spec below is computed anyway.
		slog.Warn("recover: mesh sync", "node", n.Name, "err", err)
	}
	spec, ok := o.mesh.Spec(n.ID)
	if !ok {
		return worker.MeshSpec{}, errs.ErrBusy
	}
	slog.Info("recover: node fetched its mesh through the panel", "node", n.Name)
	o.auditRecover(ctx, n.ID, "fetched its mesh: "+n.Name)
	return spec, nil
}

// openSigned checks a request a worker signed with its node key outside the
// tunnel (recovery, cert renewal): the cluster CA's worker cert, not revoked,
// a clock within recoverSkew and a fresh nonce (scope keeps the callers'
// nonces apart). refuse, when set, records the clock and nonce refusals.
func (o *Orchestrator) openSigned(req worker.RecoverRequest, scope string, refuse func(nodeID, detail string)) (clusterca.Peer, worker.RecoverPayload, error) {
	refused := errs.Refusedf("not a node of this cluster")
	if o.cluster == nil {
		return clusterca.Peer{}, worker.RecoverPayload{}, refused
	}
	now := time.Now()
	cert, p, err := worker.OpenRecover(req)
	if err != nil {
		return clusterca.Peer{}, worker.RecoverPayload{}, refused
	}
	peer, err := clusterca.Verify(o.cluster.roots(), []*x509.Certificate{cert}, clusterca.RoleWorker, "", now)
	if err != nil || o.cluster.revoked(peer.Serial) {
		return clusterca.Peer{}, worker.RecoverPayload{}, refused
	}
	if refuse == nil {
		refuse = func(string, string) {}
	}
	if d := now.Sub(time.Unix(p.Time, 0)); d > recoverSkew || d < -recoverSkew {
		refuse(peer.NodeID, "refused: clock off by "+d.Round(time.Second).String())
		return clusterca.Peer{}, worker.RecoverPayload{}, errs.Refusedf("the request's clock is off by %s", d.Round(time.Second))
	}
	if p.Nonce == "" || !o.recoverSeen.fresh(scope+peer.NodeID+"/"+p.Nonce, now) {
		refuse(peer.NodeID, "refused: replayed or missing nonce")
		return clusterca.Peer{}, worker.RecoverPayload{}, refused
	}
	return peer, p, nil
}

// auditRecover records a recovery by a node whose certificate checked out;
// best effort, a failed write never turns the node away.
func (o *Orchestrator) auditRecover(ctx context.Context, nodeID, detail string) {
	_ = o.store.Tx(ctx, func(tx store.Tx) error {
		return audit(tx).Append(ctx, securitylog.Event{
			Action: securitylog.NodeRecover, TargetKind: "node", TargetID: nodeID, Detail: detail,
		})
	})
}

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
	if o.cluster == nil || o.mesh == nil {
		return worker.MeshSpec{}, refused
	}
	now := time.Now()
	cert, p, err := worker.OpenRecover(req)
	if err != nil {
		return worker.MeshSpec{}, refused
	}
	peer, err := clusterca.Verify(o.cluster.ca.Cert, []*x509.Certificate{cert}, clusterca.RoleWorker, "", now)
	if err != nil || o.cluster.revoked(peer.Serial) {
		return worker.MeshSpec{}, refused
	}
	if d := now.Sub(time.Unix(p.Time, 0)); d > recoverSkew || d < -recoverSkew {
		return worker.MeshSpec{}, errs.Refusedf("the request's clock is off by %s", d.Round(time.Second))
	}
	if p.Nonce == "" || !o.recoverSeen.fresh(peer.NodeID+"/"+p.Nonce, now) {
		return worker.MeshSpec{}, refused
	}
	n, err := o.store.Nodes.Get(ctx, peer.NodeID)
	if err != nil || n.Role != node.Worker || n.Status == node.StatusPending {
		return worker.MeshSpec{}, refused
	}
	o.mesh.Report(ctx, n.ID, p.Facts)
	if err := o.mesh.Sync(ctx, "", false); err != nil {
		// Pushes to other nodes may fail; the spec below is computed anyway.
		slog.Warn("recover: mesh sync", "node", n.Name, "err", err)
	}
	spec, ok := o.mesh.Spec(n.ID)
	if !ok {
		return worker.MeshSpec{}, errs.ErrBusy
	}
	slog.Info("recover: node fetched its mesh through the panel", "node", n.Name)
	return spec, nil
}

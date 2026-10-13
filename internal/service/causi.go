package service

import (
	"context"
	"crypto/x509"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/clusterca"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// The cluster trust surface (docs/rewrite/tasks/ca-rollover.md, C5): what the
// admin UI, API and CLI read and call. RotateRoot, BreakGlass and the
// rollover itself are C2's and C4's; this file only reads their state and
// holds the two calls no worker has named yet.

// NodeNotRejoined is the shown status of a worker whose certificates were
// all revoked by break glass and that has not used its rejoin command yet.
const NodeNotRejoined = "not-rejoined"

// FinishRollover forces the end of a running root rollover: the old root
// leaves every trust list now, and nodes still holding an old-root cert need
// a rejoin. Stub; filled by C2 (the forced finish its contract names).
//
// Contract: admin only (by); refuses (errs.Conflictf) when no rollover is
// running; audited. Offline nodes that never got a new-root cert end up
// revoked, so they show as "not rejoined" the way break glass leaves them.
func (o *Orchestrator) FinishRollover(ctx context.Context, by *Principal) error {
	return errors.ErrUnsupported
}

// TrustNode is a node named in the trust view.
type TrustNode struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ClusterTrust is the cluster root as the admin sees it.
type ClusterTrust struct {
	Fingerprint string    `json:"fingerprint"` // hex SHA-256 of the current root
	CreatedAt   time.Time `json:"created_at"`
	ExpiresAt   time.Time `json:"expires_at"`
	// RollingOver is true from the start of a rollover until the old root is
	// dropped. OnOldRoot are the workers that still hold an old-root cert.
	RollingOver       bool        `json:"rolling_over"`
	RolloverStartedAt *time.Time  `json:"rollover_started_at"`
	OnOldRoot         []TrustNode `json:"on_old_root"`
	// BreakGlassAt is when the last break glass ran; NotRejoined are the
	// workers that have not used their rejoin command since.
	BreakGlassAt *time.Time  `json:"break_glass_at"`
	NotRejoined  []TrustNode `json:"not_rejoined"`
}

// current is the root new certs are issued from.
func (c *clusterPKI) current() *x509.Certificate {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ca.Cert
}

// ClusterTrust (server admin) reads the root, any rollover in progress and
// the nodes break glass left unjoined.
func (o *Orchestrator) ClusterTrust(ctx context.Context, by *Principal) (ClusterTrust, error) {
	if err := nodeAdmin(by); err != nil {
		return ClusterTrust{}, err
	}
	root := o.cluster.current()
	out := ClusterTrust{
		Fingerprint: clusterca.Fingerprint(root), CreatedAt: root.NotBefore, ExpiresAt: root.NotAfter,
		OnOldRoot: []TrustNode{}, NotRejoined: []TrustNode{},
	}
	row, _, err := o.store.ClusterCA.Get(ctx)
	if err != nil {
		return out, err
	}
	out.RollingOver, out.RolloverStartedAt, out.BreakGlassAt = row.RollingOver(), row.RolloverStartedAt, row.BreakGlassAt
	live, err := o.store.NodeCerts.ListLive(ctx)
	if err != nil {
		return out, err
	}
	hasLive, hasNew := map[string]bool{}, map[string]bool{}
	for _, c := range live {
		hasLive[c.NodeID] = true
		hasNew[c.NodeID] = hasNew[c.NodeID] || c.Issuer == row.CurrentFP
	}
	ns, err := o.nodeLeaf().List(ctx)
	if err != nil {
		return out, err
	}
	slices.SortFunc(ns, func(a, b store.Node) int { return strings.Compare(a.Name, b.Name) })
	for _, n := range ns {
		if n.Role != node.Worker || n.Status == node.StatusPending {
			continue
		}
		switch {
		case !hasLive[n.ID]:
			if row.BreakGlassAt != nil {
				out.NotRejoined = append(out.NotRejoined, TrustNode{n.ID, n.Name})
			}
		case out.RollingOver && !hasNew[n.ID]:
			out.OnOldRoot = append(out.OnOldRoot, TrustNode{n.ID, n.Name})
		}
	}
	return out, nil
}

// notRejoined is whether break glass ran and the worker holds no live cert
// since: it shows as NodeNotRejoined until its rejoin is confirmed.
func (o *Orchestrator) notRejoined(n store.Node) bool {
	if n.Role != node.Worker || n.Status == node.StatusPending {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	row, ok, err := o.store.ClusterCA.Get(ctx)
	if err != nil || !ok || row.BreakGlassAt == nil {
		return false
	}
	cs, err := o.store.NodeCerts.ListByNode(ctx, n.ID)
	if err != nil {
		return false
	}
	return !slices.ContainsFunc(cs, func(c store.NodeCert) bool { return c.RevokedAt == nil })
}

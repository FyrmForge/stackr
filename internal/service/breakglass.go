package service

import (
	"context"
	"errors"
)

// BreakGlass wipes the cluster's trust and every WireGuard key in one
// action (docs/rewrite/tasks/ca-rollover.md, C4). Stub from the contract
// wave; breakglass.go is C4's file.
//
// Contract:
//   - Admin only (by); audited before and after.
//   - New root with no overlap: a fresh CA via clusterca.SaveCA, the previous
//     root file removed (clusterca.DropPrevCA), the old root gone;
//     store.ClusterCA{CurrentFP: new, PreviousFP: "", RolloverStartedAt and
//     RolloverFinishedAt cleared, BreakGlassAt: now}; o.cluster.ca and
//     setRoots(new only).
//   - New controller cert, announced alone (setSerials) so no old controller
//     cert is accepted anywhere.
//   - Every worker cert revoked (NodeCerts.RevokeNode), every node's WireGuard
//     keys replaced (rotate.go), every worker marked "not rejoined" until a
//     rejoin command is used; its envs, name and addresses stay.
//   - The panel's own sessions stay valid: only cluster trust changes.
//   - Rejoin commands are minted on demand per node afterwards (RejoinNode),
//     single use and short lived; this call mints none.
func (o *Orchestrator) BreakGlass(ctx context.Context, by *Principal) error {
	return errors.ErrUnsupported
}

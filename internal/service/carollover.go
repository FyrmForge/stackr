package service

import (
	"context"
	"errors"
)

// Planned root rollover (docs/rewrite/tasks/ca-rollover.md, C2). Stubs from
// the contract wave; carollover.go is C2's file.

// RotateRoot starts a planned rollover of the cluster root and returns once
// the first step is durable; the daily rootRolloverTick resumes the rest.
//
// Contract:
//   - Refuses (errs.Conflictf) while a rollover is running or in a cluster
//     with no CA; admin only (by).
//   - Makes a new CA and calls clusterca.RotateCA(DataDir, next): the old
//     root becomes the previous root (cert only), the new one current. Saves
//     store.ClusterCA{CurrentFP: new, PreviousFP: old, RolloverStartedAt: now,
//     RolloverFinishedAt: nil}.
//   - Sets o.cluster.ca to the new CA and o.cluster.setRoots(old and new), so
//     both are trusted at once, then syncs the mesh (force) so every worker's
//     spec carries both roots in TrustRoots (o.meshTrust reads the list).
//   - Reissues the controller cert and every node cert from the new root
//     without waiting for renewal; each issued cert records Issuer
//     (Issued.Issuer) in node_certs.
//   - When every unrevoked worker cert (NodeCerts.ListLive) has the new
//     Issuer, drops the old root: clusterca.DropPrevCA, setRoots(new only),
//     ClusterCA{PreviousFP: "", RolloverFinishedAt: now}, force-sync the mesh.
//   - Every step is audited (securitylog); a failed step is retried by the
//     tick, never rolled back.
//
// Forced finish (offline nodes marked needs-rejoin) is C2's call; the node
// row state for it is C2's to add and goes in the seams file.
func (o *Orchestrator) RotateRoot(ctx context.Context, by *Principal) error {
	return errors.ErrUnsupported
}

// rootRolloverTick is the daily root check, run from the same loop that
// renews the controller cert.
//
// Contract:
//   - A rollover running (store.ClusterCA.RollingOver) is resumed: reissue
//     nodes still on the old Issuer, finish when none is left. This is what
//     survives a panel restart.
//   - Otherwise, when the current root has under a year left
//     (cert.NotAfter - now < 365 days), calls RotateRoot with a system
//     principal.
//   - A tick with nothing to do returns nil and writes nothing.
func (o *Orchestrator) rootRolloverTick(ctx context.Context) error {
	return errors.ErrUnsupported
}

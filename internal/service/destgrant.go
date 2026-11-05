package service

import (
	"cmp"
	"context"
	"slices"

	"github.com/FyrmForge/stackr/internal/service/errs"
	fbackup "github.com/FyrmForge/stackr/internal/service/internal/flow/backup"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/backup"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/securitylog"
	"github.com/FyrmForge/stackr/internal/service/internal/s3"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// DestGrant is an org's backup destination on a private address: it runs
// only once a server admin approves that host:port. Pending is true until
// then, and again when the endpoint moves.
type DestGrant struct {
	DestID     string `json:"dest_id"`
	Name       string `json:"name"`
	OrgID      string `json:"org_id"`
	Org        string `json:"org"` // the org's name
	Addr       string `json:"addr"`
	Pending    bool   `json:"pending"`
	ApprovedBy string `json:"approved_by"`
}

// needsGrant: an org's S3 destination whose endpoint is a private address.
func needsGrant(d store.BackupDest) bool {
	return d.OrgID != nil && d.Kind == backup.S3 && s3.Private(d.Endpoint)
}

// probeDest is the test write a destination passes before it is saved;
// none when Config.ProbeDests is off (tests with no bucket behind them). A
// private org endpoint nobody approved is not probed (the probe could not
// reach it); a server admin's save is, as if already approved.
func (o *Orchestrator) probeDest(admin bool) backup.Probe {
	if !o.cfg.ProbeDests {
		return nil
	}
	return func(ctx context.Context, d store.BackupDest) error {
		if needsGrant(d) && fbackup.ApprovedAddr(d) == "" {
			if !admin {
				return nil
			}
			d.ApprovedEndpoint = s3.Addr(d.Endpoint)
		}
		return o.backup.Probe(ctx, d)
	}
}

// approvedBySaver approves d for the acting server admin right after a save
// that put it on a private address; anyone else's save leaves it pending.
func (o *Orchestrator) approvedBySaver(ctx context.Context, d BackupDest, err error) (BackupDest, error) {
	if err != nil || !needsGrant(d) || fbackup.ApprovedAddr(d) != "" {
		return d, err
	}
	user, key := store.ActorFrom(ctx)
	if !o.adminActor(ctx, user, key) {
		return d, nil
	}
	return d, o.backups.Approve(ctx, &d, s3.Addr(d.Endpoint), user)
}

// DestGrants (server admin) lists every org destination on a private
// address, pending or approved. Privateness is a lookup cached for a minute
// (s3.PrivateRecent); the dial check at run time is the enforcement.
func (o *Orchestrator) DestGrants(ctx context.Context) ([]DestGrant, error) {
	orgs, err := o.orgs.ListAll(ctx)
	if err != nil {
		return nil, err
	}
	out := []DestGrant{}
	for _, og := range orgs {
		ds, err := o.backups.Visible(ctx, og.ID)
		if err != nil {
			return nil, err
		}
		for _, d := range ds {
			if d.OrgID == nil || d.Kind != backup.S3 || !s3.PrivateRecent(d.Endpoint) {
				continue
			}
			out = append(out, DestGrant{
				DestID: d.ID, Name: d.Name, OrgID: og.ID, Org: og.Name, Addr: s3.Addr(d.Endpoint),
				Pending: fbackup.ApprovedAddr(d) == "", ApprovedBy: d.ApprovedBy,
			})
		}
	}
	slices.SortFunc(out, func(a, b DestGrant) int {
		return cmp.Or(cmp.Compare(a.Org, b.Org), cmp.Compare(a.Name, b.Name))
	})
	return out, nil
}

// DestGrantsWaiting (server admin) is how many destinations wait for approval.
func (o *Orchestrator) DestGrantsWaiting(ctx context.Context) (int, error) {
	gs, err := o.DestGrants(ctx)
	n := 0
	for _, g := range gs {
		if g.Pending {
			n++
		}
	}
	return n, err
}

// ApproveDestGrant (server admin) lets the org destination reach addr, the
// host:port the admin was shown (DestGrant.Addr); when the endpoint has
// moved since, nothing is approved and it is a Conflict.
func (o *Orchestrator) ApproveDestGrant(ctx context.Context, destID, userID, addr string) (DestGrant, error) {
	d, err := o.backups.Get(ctx, destID)
	if err != nil {
		return DestGrant{}, err
	}
	if d.OrgID == nil || d.Kind != backup.S3 {
		return DestGrant{}, errs.ErrNotFound
	}
	if s3.Addr(d.Endpoint) != addr {
		return DestGrant{}, errs.Conflictf("the endpoint changed; review it again")
	}
	if !needsGrant(d) {
		return DestGrant{}, errs.Conflictf("this destination is not on a private address")
	}
	if err := o.backups.Approve(ctx, &d, addr, userID); err != nil {
		return DestGrant{}, err
	}
	if err := o.noted(ctx, nil, securitylog.GrantApprove, "backup_dest", d.ID, addr); err != nil {
		return DestGrant{}, err
	}
	return DestGrant{DestID: d.ID, Name: d.Name, OrgID: *d.OrgID, Addr: addr, ApprovedBy: userID}, nil
}

// RevokeDestGrant (server admin) takes the approval back: the destination is
// pending again and its runs fail with the waiting message.
func (o *Orchestrator) RevokeDestGrant(ctx context.Context, destID string) (DestGrant, error) {
	d, err := o.backups.Get(ctx, destID)
	if err != nil {
		return DestGrant{}, err
	}
	if d.OrgID == nil || d.Kind != backup.S3 {
		return DestGrant{}, errs.ErrNotFound
	}
	if err := o.backups.Approve(ctx, &d, "", ""); err != nil {
		return DestGrant{}, err
	}
	addr := s3.Addr(d.Endpoint)
	if err := o.noted(ctx, nil, securitylog.GrantRevoke, "backup_dest", d.ID, addr); err != nil {
		return DestGrant{}, err
	}
	return DestGrant{DestID: d.ID, Name: d.Name, OrgID: *d.OrgID, Addr: addr, Pending: true}, nil
}

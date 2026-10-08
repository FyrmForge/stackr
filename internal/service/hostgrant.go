package service

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/hostgrant"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
)

// HostGrant is the elevated access a server admin approved for a stack, one
// "<tile-slug> <perm>" line each (hostgrant.Line): "web host:/a:/b[:ro]",
// "web device:/dev/x", "web privileged", "web lan:10.0.0.5:80", "web port:8080",
// "web network:host". Pending is what its waiting jobs ask for and are not
// yet granted.
type HostGrant struct {
	StackID    string    `json:"stack_id"`
	Granted    bool      `json:"granted"` // false: no row, nothing approved
	Lines      []string  `json:"lines"`
	ApprovedBy string    `json:"approved_by"`
	CreatedAt  time.Time `json:"created_at"`
	Pending    []string  `json:"pending"`
}

// hostAccess is the part of a job payload that says what it waits on.
type hostAccess struct {
	Stack string `json:"stack"`
	What  string `json:"what"`
}

// withApproval records on the job payload the host access the job parks on,
// or (n nil) clears it so a job that later parks on something else is not
// mistaken for one. Every other key stays.
func withApproval(payload string, n *errs.NeedsApproval) string {
	var m map[string]any
	if json.Unmarshal([]byte(payload), &m) != nil || m == nil {
		m = map[string]any{}
	}
	if n == nil {
		delete(m, "host_access")
	} else {
		m["host_access"] = hostAccess{Stack: n.Stack, What: n.What}
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// parkOnApproval keeps the job payload in step with how the handler ended:
// a NeedsApproval stores its ask there (ApproveHostGrant reads it), any
// other end clears an old one.
func parkOnApproval(r *jobs.Run, err error) error {
	if n, ok := errs.IsNeedsApproval(err); ok {
		r.Job.Payload = withApproval(r.Job.Payload, &n)
	} else if strings.Contains(r.Job.Payload, `"host_access"`) {
		r.Job.Payload = withApproval(r.Job.Payload, nil)
	}
	return err
}

// asks are the waiting jobs parked on host access for the stack, with what
// each wants. A job parked on the same text through a dependency counts too.
func (o *Orchestrator) asks(ctx context.Context, stackID string) (jobs []Job, what []string, err error) {
	waiting, err := o.jobRows.List(ctx, job.Waiting)
	if err != nil {
		return nil, nil, err
	}
	for _, j := range waiting {
		var p struct {
			HostAccess *hostAccess `json:"host_access"`
		}
		if json.Unmarshal([]byte(j.Payload), &p) == nil && p.HostAccess != nil && p.HostAccess.Stack == stackID {
			jobs, what = append(jobs, j), append(what, p.HostAccess.What)
		}
	}
	for _, j := range waiting {
		if j.WaitingParam != nil && slices.Contains(what, *j.WaitingParam) && !slices.ContainsFunc(jobs, func(x Job) bool { return x.ID == j.ID }) {
			jobs = append(jobs, j)
		}
	}
	return jobs, what, nil
}

// HostGrant is the stack's host access; no row is an ungranted stack, not an
// error.
func (o *Orchestrator) HostGrant(ctx context.Context, stackID string) (HostGrant, error) {
	out := HostGrant{StackID: stackID, Lines: []string{}, Pending: []string{}}
	g, err := o.hostgrant.Row(ctx, stackID)
	if err == nil {
		have, _ := o.hostgrant.Of(ctx, stackID)
		out.Granted, out.Lines = true, have.Lines
		out.ApprovedBy, out.CreatedAt = g.ApprovedBy, g.CreatedAt
	} else if _, err = o.stacks.Get(ctx, stackID); err != nil {
		return out, err
	}
	_, what, err := o.asks(ctx, stackID)
	for _, w := range what {
		if s, ok := hostgrant.Parse(w); ok {
			out.Pending = append(out.Pending, s.Missing(hostgrant.Set{Lines: out.Lines})...)
		}
	}
	slices.Sort(out.Pending)
	out.Pending = slices.Compact(out.Pending)
	return out, err
}

// ApproveHostGrant (server admin) grants the stack what its waiting jobs ask
// for, adding to what it already has, and requeues those jobs. shown is the
// pending set the admin was shown (HostGrant.Pending); when the current one
// differs, nothing is granted and it is a Conflict, so an ask parked after
// the look is never approved unseen. grant narrows it: empty is all of
// shown, else the subset to approve (a line that was not asked is refused).
// A job whose ask is not fully granted parks again with the remainder.
// Nothing waiting is a Conflict too.
func (o *Orchestrator) ApproveHostGrant(ctx context.Context, stackID, userID string, shown, grant []string) (HostGrant, error) {
	g, err := o.HostGrant(ctx, stackID)
	if err != nil {
		return HostGrant{}, err
	}
	if len(g.Pending) == 0 {
		return HostGrant{}, errs.Conflictf("no job waits on elevated access for this stack")
	}
	if !slices.Equal(g.Pending, slices.Compact(slices.Sorted(slices.Values(shown)))) {
		return HostGrant{}, errs.Conflictf("the ask changed; review it again")
	}
	ask := hostgrant.Set{Lines: g.Pending}
	if len(grant) > 0 {
		for _, l := range grant {
			if !slices.Contains(g.Pending, l) {
				return HostGrant{}, errs.Invalidf("grant", "%q was not asked for", l)
			}
		}
		ask = hostgrant.Set{Lines: grant}.Norm()
	}
	if _, err := o.hostgrant.Approve(ctx, stackID, userID, ask); err != nil {
		return HostGrant{}, err
	}
	o.rerouteVIPsAsync(ctx) // lan access starts now
	waiting, _, err := o.asks(ctx, stackID)
	if err != nil {
		return HostGrant{}, err
	}
	for _, j := range waiting {
		if err := o.jobRows.Requeue(ctx, j); err != nil {
			return HostGrant{}, err
		}
	}
	return o.HostGrant(ctx, stackID)
}

// RevokeHostGrant (server admin) removes one tile's lines from the stack's
// grant, or the whole grant when tileSlug is "". Running tiles stay up; the
// next deploy or promote that needs the access parks again.
func (o *Orchestrator) RevokeHostGrant(ctx context.Context, stackID, tileSlug string) error {
	if err := o.hostgrant.Revoke(ctx, stackID, tileSlug); err != nil {
		return err
	}
	o.rerouteVIPsAsync(ctx) // cut LAN access now, not at the next deploy
	return nil
}

// ListHostGrants (server admin) is every stack's grant, with what its waiting
// jobs still ask for; stacks with neither are left out.
func (o *Orchestrator) ListHostGrants(ctx context.Context) ([]HostGrant, error) {
	rows, err := o.hostgrant.All(ctx)
	if err != nil {
		return nil, err
	}
	out := []HostGrant{}
	seen := map[string]bool{}
	for _, r := range rows {
		g, err := o.HostGrant(ctx, r.StackID)
		if err != nil {
			return nil, err
		}
		out, seen[r.StackID] = append(out, g), true
	}
	waiting, err := o.jobRows.List(ctx, job.Waiting)
	if err != nil {
		return nil, err
	}
	for _, j := range waiting {
		var p struct {
			HostAccess *hostAccess `json:"host_access"`
		}
		if json.Unmarshal([]byte(j.Payload), &p) != nil || p.HostAccess == nil || seen[p.HostAccess.Stack] {
			continue
		}
		g, err := o.HostGrant(ctx, p.HostAccess.Stack)
		if err != nil {
			return nil, err
		}
		out, seen[p.HostAccess.Stack] = append(out, g), true
	}
	return out, nil
}

package orgconf

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/workqueue"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// ApplyKind is the work-queue kind an org config apply runs under. Distinct
// from stackconf.ApplyKind: the queue registers one handler per kind, and the
// two payloads are not interchangeable.
const ApplyKind = "orgconfig.apply"

// ApplyJob is the payload. No Force: an org plan has no unattended path, it is
// approved by an owner or it does not run.
type ApplyJob struct {
	OrgID  string `json:"org_id"`
	PlanID string `json:"plan_id"`
}

// RegisterApply wires the org apply handler onto the queue.
//
// Requeue on restart, for the same reason stackconf.RegisterApply does: an org
// apply re-loads the file, re-diffs against whatever exists now and
// reconciles, so running it again on a half-finished org finishes the job.
// Unlike a promote it is not a decision that could be re-taken wrongly.
func RegisterApply(q *workqueue.Queue, r *Runner) {
	q.Register(ApplyKind, func(ctx context.Context, j *workqueue.Job) error {
		var p ApplyJob
		if err := j.Payload(&p); err != nil {
			return err
		}
		return r.RunApplyJob(ctx, j, p)
	}, workqueue.KindOpts{
		Timeout:   15 * time.Minute,
		OnRestart: workqueue.Requeue,
	})
}

// RunApplyJob is the apply as the queue runs it. Exported so the registration
// above stays a wiring function and this stays testable.
//
// The error lands on the plan row from here, with this job's context. That is
// the whole point of the move: both surfaces used to pass the HTTP request's
// context, so an apply that outlived its request was cancelled mid-way and the
// write recording the failure died silently on the same dead context, leaving
// the row carrying the previous run's message and pending for ever.
//
// Recorded here rather than inside Apply because Apply writes the row on only
// two of its exit paths — the file load and the collected teardown failures.
// A diff error, a plan with errors, a refused rename, a domain or shared-tile
// failure all return bare, and inline those were at least shown as a flash.
// Queued there is no flash: the row is the only record there is.
func (r *Runner) RunApplyJob(ctx context.Context, j *workqueue.Job, p ApplyJob) error {
	org, err := r.Store.GetOrg(ctx, p.OrgID)
	if err != nil {
		return err
	}
	if org == nil {
		return fmt.Errorf("org %s is gone", p.OrgID)
	}
	cp, err := r.Store.GetOrgConfigPlan(ctx, p.PlanID)
	if err != nil {
		return err
	}
	if cp == nil {
		return fmt.Errorf("plan %s is gone", p.PlanID)
	}
	// Decided while this waited in the queue. Not an error: two people
	// pressing Apply is a race, not a fault.
	if cp.Status != "pending" {
		slog.Info("org config apply: the plan was already decided", "plan", cp.ID, "status", cp.Status)
		return nil
	}

	j.SetStep(ctx, "applying")
	if err := r.Apply(ctx, org, cp); err != nil {
		if werr := r.Store.SetOrgConfigPlanError(ctx, cp.ID, err.Error()); werr != nil {
			slog.Error("org config apply: recording the failure on the plan", "plan", cp.ID, "error", werr)
		}
		return err
	}
	j.SetStep(ctx, "applied")
	return nil
}

// EnqueueApply is the one way an org config apply is started. The dedupe key is
// the plan, matching stackconf.EnqueueApply: two people approving one plan is
// one apply.
func EnqueueApply(ctx context.Context, q *workqueue.Queue, org *repo.Org, cp *repo.ConfigPlan) (string, error) {
	if q == nil {
		return "", fmt.Errorf("the job runner is not available")
	}
	return q.Enqueue(ctx, ApplyKind, cp.ID, ApplyJob{OrgID: org.ID, PlanID: cp.ID})
}

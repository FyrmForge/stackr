package stackconf

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/workqueue"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// ApplyKind is the work-queue kind a config apply runs under.
const ApplyKind = "config.apply"

// ApplyJob is the payload. Force is the human approval; false is the
// unattended webhook path, which applies only the environments whose policy is
// auto (Applier.holdManual).
//
// PromoteEnv and PromoteCommit are set by the releases page's "apply the plan
// and promote": the images move only once the whole config has landed, and
// both halves belong to one job so a restart between them cannot leave the
// promote orphaned.
type ApplyJob struct {
	StackID       string `json:"stack_id"`
	PlanID        string `json:"plan_id"`
	Force         bool   `json:"force"`
	PromoteEnv    string `json:"promote_env,omitempty"`
	PromoteCommit string `json:"promote_commit,omitempty"`
}

// RegisterApply wires the apply handler onto the queue.
//
// Requeue on restart, because an apply is convergent by design: it re-loads
// the config, re-diffs against whatever state actually exists now, and
// reconciles, so running it again on a half-finished environment finishes the
// job. That is the same property the walk in execute already relies on.
func RegisterApply(q *workqueue.Queue, a Applier) {
	q.Register(ApplyKind, func(ctx context.Context, j *workqueue.Job) error {
		var p ApplyJob
		if err := j.Payload(&p); err != nil {
			return err
		}
		return a.RunApplyJob(ctx, j, p)
	}, workqueue.KindOpts{
		Timeout:   15 * time.Minute,
		OnRestart: workqueue.Requeue,
	})
}

// RunApplyJob is the apply as the queue runs it. Exported so the registration
// above stays a three-line wiring function and this stays testable.
//
// The error is written onto the plan row from here, with this job's context,
// which is the whole point of the move: every caller used to pass the HTTP
// request's context, so an apply that outlived the request was cancelled
// mid-way and the write recording that failed silently on the same dead
// context. The row kept the previous run's message and stayed pending.
func (a Applier) RunApplyJob(ctx context.Context, j *workqueue.Job, p ApplyJob) error {
	store := a.Planner.Store
	stack, err := store.GetStack(ctx, p.StackID)
	if err != nil {
		return err
	}
	if stack == nil {
		return fmt.Errorf("stack %s is gone", p.StackID)
	}
	cp, err := store.GetConfigPlan(ctx, p.PlanID)
	if err != nil {
		return err
	}
	if cp == nil {
		return fmt.Errorf("plan %s is gone", p.PlanID)
	}
	// A plan somebody decided while this waited in the queue. Not an error:
	// two people pressing Apply is a race, not a fault.
	if cp.Status != "pending" && cp.Status != "clean" {
		slog.Info("config apply: the plan was already decided", "plan", cp.ID, "status", cp.Status)
		return nil
	}

	j.SetStep(ctx, "applying")
	applied, err := a.ApplyPlan(ctx, stack, cp, p.Force)
	if err != nil {
		// Recorded on the row, because that is where it is read.
		if werr := store.SetConfigPlanError(ctx, cp.ID, err.Error()); werr != nil {
			slog.Error("config apply: recording the failure on the plan", "plan", cp.ID, "error", werr)
		}
		return err
	}
	if !applied {
		// Policy held it back, or it needs approval. Nothing failed.
		j.SetStep(ctx, "waiting for approval")
		return nil
	}
	if p.PromoteEnv == "" {
		j.SetStep(ctx, "applied")
		return nil
	}
	j.SetStep(ctx, "promoting "+p.PromoteEnv)
	if err := a.Promote(ctx, stack, p.PromoteEnv, p.PromoteCommit); err != nil {
		return fmt.Errorf("the config applied but the promote failed: %w", err)
	}
	j.SetStep(ctx, "applied and promoted")
	return nil
}

// PromoteWith is the optional second half of an apply: once the whole config
// has landed, move the images built at Commit onto the Env rung. Zero means
// "apply only".
type PromoteWith struct {
	Env    string
	Commit string
}

// EnqueueApply is the one way a config apply is started. The dedupe key is the
// plan, not the stack: one push makes one plan per branch-bound env, and a
// stack-wide key left every plan but the last sitting "pending" with nothing
// to run it. Two approvals of the same plan still collapse into one apply.
//
// The apply-and-promote paths go through here too. Both surfaces used to build
// the ApplyJob by hand under the *stack* id, which threw the plan-id dedupe
// away: a promote queued while an approval of the same plan was already
// waiting ran the apply twice.
func EnqueueApply(ctx context.Context, q *workqueue.Queue, stack *repo.Stack, cp *repo.ConfigPlan, force bool, promote PromoteWith) (string, error) {
	return q.Enqueue(ctx, ApplyKind, cp.ID, ApplyJob{
		StackID: stack.ID, PlanID: cp.ID, Force: force,
		PromoteEnv: promote.Env, PromoteCommit: promote.Commit,
	})
}

// PromoteKind is the work-queue kind a plain promote runs under — a promote
// with no config plan behind it, which is what a code-only push produces.
const PromoteKind = "config.promote"

// PromoteJob is its payload.
type PromoteJob struct {
	StackID string `json:"stack_id"`
	Env     string `json:"env"`
	Commit  string `json:"commit"`
}

// RegisterPromote wires the promote handler onto the queue.
//
// Dropped rather than requeued on restart: a promote is a decision about which
// commit an environment should be running, and re-taking someone's decision
// after a restart could move an environment backwards past whatever was
// promoted in the meantime. An apply is convergent and a promote is not.
func RegisterPromote(q *workqueue.Queue, a Applier) {
	q.Register(PromoteKind, func(ctx context.Context, j *workqueue.Job) error {
		var p PromoteJob
		if err := j.Payload(&p); err != nil {
			return err
		}
		stack, err := a.Planner.Store.GetStack(ctx, p.StackID)
		if err != nil {
			return err
		}
		if stack == nil {
			return fmt.Errorf("stack %s is gone", p.StackID)
		}
		j.SetStep(ctx, "promoting "+p.Env)
		return a.Promote(ctx, stack, p.Env, p.Commit)
	}, workqueue.KindOpts{
		Timeout:     10 * time.Minute,
		OnRestart:   workqueue.Fail,
		RestartFail: "the server restarted mid-promote; promote the commit again",
	})
}

// EnqueuePromote queues a promote with no plan behind it. The dedupe key is
// the environment: two people promoting to staging at once is one promote,
// and the second would otherwise race the first's deploys.
//
// Queued rather than run on the request, which is where it used to run on both
// surfaces. A promote walks every tile of every rung at or above the target
// and resolves each one's image name, so it is a request-time walk of the
// whole stack whose failures had nowhere to be recorded.
func EnqueuePromote(ctx context.Context, q *workqueue.Queue, stack *repo.Stack, envSlug, commit string) (string, error) {
	return q.Enqueue(ctx, PromoteKind, stack.ID+":"+envSlug, PromoteJob{
		StackID: stack.ID, Env: envSlug, Commit: commit,
	})
}

// Queue adapts the work queue to service.ApplyQueue, so the service layer can
// start an apply or a promote without importing this package. Both halves are
// keyed here rather than at the caller, which is the point: the two surfaces
// each chose their own key and lost the dedupe.
type Queue struct{ Q *workqueue.Queue }

func (q Queue) Apply(ctx context.Context, stack *repo.Stack, cp *repo.ConfigPlan, force bool, promoteEnv, promoteCommit string) (string, error) {
	if q.Q == nil {
		return "", fmt.Errorf("the job runner is not available")
	}
	return EnqueueApply(ctx, q.Q, stack, cp, force, PromoteWith{Env: promoteEnv, Commit: promoteCommit})
}

func (q Queue) Promote(ctx context.Context, stack *repo.Stack, envSlug, commit string) (string, error) {
	if q.Q == nil {
		return "", fmt.Errorf("the job runner is not available")
	}
	return EnqueuePromote(ctx, q.Q, stack, envSlug, commit)
}

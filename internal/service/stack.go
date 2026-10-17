package service

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/promote"
	"github.com/FyrmForge/stackr/internal/service/internal/githubapp"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

func (o *Orchestrator) Stacks(ctx context.Context, orgID string) ([]Stack, error) {
	return o.stacks.List(ctx, orgID)
}

func (o *Orchestrator) CreateStack(ctx context.Context, orgID, name, description string) (Stack, error) {
	return o.stacks.Create(ctx, orgID, name, description)
}

// RenameStack moves name and slug, and the auto domains under the stack
// with them.
func (o *Orchestrator) RenameStack(ctx context.Context, id, name string) (Stack, error) {
	st, err := o.stacks.Get(ctx, id)
	if err != nil {
		return st, err
	}
	if st, err = o.stacks.Rename(ctx, st, name); err != nil {
		return st, err
	}
	ts, err := o.tiles.ListByStack(ctx, st.ID)
	if err != nil {
		return st, err
	}
	return st, o.refreshAutoHosts(ctx, ts)
}

// DeleteStack refuses while the stack has environments.
func (o *Orchestrator) DeleteStack(ctx context.Context, id string) error {
	es, err := o.envs.List(ctx, id)
	if err != nil {
		return err
	}
	if len(es) > 0 {
		return errs.Conflictf("Remove this stack's environments first.")
	}
	if err := o.stacks.Delete(ctx, id); err != nil {
		return err
	}
	o.cancelWaiting(ctx, func(j Job) bool {
		var p struct {
			HostAccess *hostAccess `json:"host_access"`
		}
		return json.Unmarshal([]byte(j.Payload), &p) == nil && p.HostAccess != nil && p.HostAccess.Stack == id
	})
	return nil
}

const kindStackTeardown jobs.Kind = "stack-teardown"

type stackJob struct {
	StackID string `json:"stack_id"`
}

// TeardownStack queues one job that removes every tile, then every env,
// then the stack. confirm must be the stack's slug: the caller typed it.
// Volumes are kept, as env delete keeps them. A stack with no envs goes at
// once and the Job is zero.
func (o *Orchestrator) TeardownStack(ctx context.Context, id, confirm string) (Job, error) {
	st, err := o.stacks.Get(ctx, id)
	if err != nil {
		return Job{}, err
	}
	if confirm != st.Slug {
		return Job{}, errs.Invalidf("confirm", "Type the stack's slug, %s, to remove it and everything in it.", st.Slug)
	}
	lock := []string{"stack:" + id}
	es, err := o.envs.List(ctx, id)
	if err != nil {
		return Job{}, err
	}
	if len(es) == 0 { // nothing runs: gone now, no job
		return Job{}, o.DeleteStack(ctx, id)
	}
	for _, e := range es {
		lock = append(lock, "env:"+e.ID)
	}
	ts, err := o.tiles.ListByStack(ctx, id)
	if err != nil {
		return Job{}, err
	}
	lock = append(lock, ids(ts)...)
	return o.enqueue(ctx, kindStackTeardown, stackJob{StackID: id}, lock...)
}

// runStackTeardown is the "stack-teardown" job: each env's tiles go as a
// tile delete takes them, then the env, then the stack.
func (o *Orchestrator) runStackTeardown(ctx context.Context, r *jobs.Run, p stackJob) error {
	es, err := o.envs.List(ctx, p.StackID)
	if err != nil {
		return err
	}
	for _, e := range es {
		ectx, err := o.withEnvNode(tearingDown(ctx), e)
		if err != nil {
			return err
		}
		ts, err := o.tiles.List(ectx, e.ID)
		if err != nil {
			return err
		}
		if len(ts) > 0 {
			if _, err := o.promote.Remove(ectx, e, ts, r.Log); err != nil {
				return err
			}
			for _, t := range ts {
				if err := o.dropTileGrant(ectx, t); err != nil {
					return err
				}
				o.cancelWaitingTile(ectx, t.ID)
			}
			if err := o.dropRuns(ids(ts)); err != nil {
				return err
			}
		}
		if err := o.DeleteEnv(ctx, e.ID); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(r.Log, "removed environment %s and %d tile(s)\n", e.Name, len(ts))
	}
	o.sched.Reload(ctx)
	if err := o.DeleteStack(ctx, p.StackID); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(r.Log, "removed the stack")
	return o.sync.Sync(ctx)
}

// SetConfigRepo points the stack at its stackr-compose.yml (config-as-code).
func (o *Orchestrator) SetConfigRepo(ctx context.Context, id, connectorID, repo, branch, path string) (Stack, error) {
	st, err := o.stacks.Get(ctx, id)
	if err != nil {
		return st, err
	}
	if connectorID != "" && repo != "" {
		if _, err := o.conns.Usable(ctx, st.OrgID, connectorID); err != nil {
			return st, err // another org's connector is not there
		}
	}
	return o.stacks.SetConfigRepo(ctx, st, connectorID, repo, branch, path)
}

// SetStackSettings writes the stack's settings blob and redeploys its
// running tiles (B34).
func (o *Orchestrator) SetStackSettings(ctx context.Context, id, blob string) (Stack, error) {
	st, err := o.stacks.Get(ctx, id)
	if err != nil {
		return st, err
	}
	if err := o.checkBlobLimits(ctx, blob); err != nil {
		return st, err
	}
	if st, err = o.stacks.SetSettings(ctx, st, blob); err != nil {
		return st, err
	}
	ts, err := o.tiles.ListByStack(ctx, id)
	if err != nil {
		return st, err
	}
	return st, o.redeployRunning(ctx, ts)
}

// Webhook delivery errors, re-exported for handlers.
var (
	ErrBadSignature = githubapp.ErrBadSignature
	ErrBadPayload   = githubapp.ErrBadPayload
)

// Webhook takes one GitHub delivery for a connector: a push queues a push
// job per stack of the org that uses the repo, and a plan of the org's
// config file when it lands on the org's binding; a pull_request queues a
// PR-env job. A server connector's delivery queues the server plan when it
// lands on the server file's binding, then does the same for every org the
// connector is shared with (serverWebhook).
// The caller maps ErrBadSignature to 401 and ErrBadPayload to 400.
func (o *Orchestrator) Webhook(ctx context.Context, connectorID, event, signature string, body []byte) error {
	c, secret, err := o.conns.WebhookSecret(ctx, connectorID)
	if err != nil {
		return err
	}
	ev, err := githubapp.Receive(event, signature, body, secret)
	if err != nil {
		return err
	}
	d := delivery{ev: ev}
	switch {
	case ev.Push != nil && !ev.Push.Deleted && strings.HasPrefix(ev.Push.Ref, "refs/heads/"):
		d.repo = ev.Push.Repository.CloneURL
		d.branch = strings.TrimPrefix(ev.Push.Ref, "refs/heads/")
		d.defaultBranch = ev.Push.Repository.DefaultBranch
	case ev.PR != nil:
		d.repo = ev.PR.Repository.CloneURL
	default:
		return nil
	}
	if c.OrgID == nil {
		return o.serverWebhook(ctx, c, d)
	}
	return o.orgWebhook(ctx, *c.OrgID, d)
}

// enqueuePush queues a push job; a newer push of the same stack, repo and
// branch supersedes a queued one.
func (o *Orchestrator) enqueuePush(ctx context.Context, p pushJob) (Job, error) {
	return o.enqueue(
		ctx,
		kindPush,
		p,
		"push:"+p.StackID,
		"push:"+p.StackID+":"+promote.NormalizeRepo(p.Event.Repo)+"@"+p.Event.Branch,
	)
}

// usesRepo: the stack's config repo or any of its tiles builds from repo.
func (o *Orchestrator) usesRepo(ctx context.Context, st store.Stack, repo string) (bool, error) {
	key := promote.NormalizeRepo(repo)
	if st.ConfigRepo != "" && promote.NormalizeRepo(st.ConfigRepo) == key {
		return true, nil
	}
	ts, err := o.tiles.ListByStack(ctx, st.ID)
	if slices.ContainsFunc(ts,
		func(t store.Tile) bool { return t.GitURL != "" && promote.NormalizeRepo(t.GitURL) == key }) {
		return true, nil
	}
	return false, err
}

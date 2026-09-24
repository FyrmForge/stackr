package service

import (
	"context"
	"strconv"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/promote"
	"github.com/FyrmForge/stackr/internal/service/internal/githubapp"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/stack"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

type Reservation = stack.Reservation

func (o *Orchestrator) Stacks(ctx context.Context, orgID string) ([]Stack, error) {
	return o.stacks.List(ctx, orgID)
}

func (o *Orchestrator) CreateStack(ctx context.Context, orgID, name, description string) (Stack, error) {
	return o.stacks.Create(ctx, orgID, name, description)
}

func (o *Orchestrator) RenameStack(ctx context.Context, id, name string) (Stack, error) {
	st, err := o.stacks.Get(ctx, id)
	if err != nil {
		return st, err
	}
	return o.stacks.Rename(ctx, st, name)
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
	return o.stacks.Delete(ctx, id)
}

// SetConfigRepo points the stack at its stackr-compose.yml (config-as-code).
func (o *Orchestrator) SetConfigRepo(ctx context.Context, id, connectorID, repo, branch, path string) (Stack, error) {
	st, err := o.stacks.Get(ctx, id)
	if err != nil {
		return st, err
	}
	if connectorID != "" && repo != "" {
		if _, err := o.conns.Get(ctx, st.OrgID, connectorID); err != nil {
			return st, err // another org's connector is not there
		}
	}
	return o.stacks.SetConfigRepo(ctx, st, connectorID, repo, branch, path)
}

// SetReservations replaces the stack's domain reservations, then re-pushes
// the proxy (ACME accounts come from them).
func (o *Orchestrator) SetReservations(ctx context.Context, id string, rs []Reservation) (Stack, error) {
	st, err := o.stacks.Get(ctx, id)
	if err != nil {
		return st, err
	}
	if st, err = o.stacks.SetReservations(ctx, st, rs); err != nil {
		return st, err
	}
	return st, o.sync.Sync(ctx)
}

// SetStackSettings writes the stack's settings blob and redeploys its
// running tiles (B34).
func (o *Orchestrator) SetStackSettings(ctx context.Context, id, blob string) (Stack, error) {
	st, err := o.stacks.Get(ctx, id)
	if err != nil {
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
// job per stack of the org that uses the repo, a pull_request a PR-env job.
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
	var repo string
	switch {
	case ev.Push != nil && !ev.Push.Deleted && strings.HasPrefix(ev.Push.Ref, "refs/heads/"):
		repo = ev.Push.Repository.CloneURL
	case ev.PR != nil:
		repo = ev.PR.Repository.CloneURL
	default:
		return nil
	}
	sts, err := o.stacks.List(ctx, c.OrgID)
	if err != nil {
		return err
	}
	for _, st := range sts {
		if ok, err := o.usesRepo(ctx, st, repo); err != nil || !ok {
			if err != nil {
				return err
			}
			continue
		}
		if p := ev.Push; p != nil {
			branch := strings.TrimPrefix(p.Ref, "refs/heads/")
			_, err = o.enqueue(ctx, kindPush, pushJob{StackID: st.ID, Event: promote.Event{
				Repo: repo, Branch: branch, Commit: p.After, Changed: p.ChangedFiles()}},
				"push:"+st.ID, "push:"+st.ID+":"+promote.NormalizeRepo(repo)+"@"+branch)
		} else {
			pr := ev.PR
			_, err = o.enqueue(ctx, kindPR, prJob{StackID: st.ID, Action: pr.Action, Number: pr.Number, Repo: repo,
				Head: pr.PullRequest.Head.Ref, SHA: pr.PullRequest.Head.SHA, Base: pr.PullRequest.Base.Ref},
				"push:"+st.ID, "pr:"+st.ID+":"+strconv.Itoa(pr.Number))
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// usesRepo: the stack's config repo or any of its tiles builds from repo.
func (o *Orchestrator) usesRepo(ctx context.Context, st store.Stack, repo string) (bool, error) {
	key := promote.NormalizeRepo(repo)
	if st.ConfigRepo != "" && promote.NormalizeRepo(st.ConfigRepo) == key {
		return true, nil
	}
	ts, err := o.tiles.ListByStack(ctx, st.ID)
	for _, t := range ts {
		if t.GitURL != "" && promote.NormalizeRepo(t.GitURL) == key {
			return true, nil
		}
	}
	return false, err
}

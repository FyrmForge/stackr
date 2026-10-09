package service

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/errs"
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

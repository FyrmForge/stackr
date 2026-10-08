package service

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/promote"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/serverconfig"
	"github.com/FyrmForge/stackr/internal/service/internal/githubapp"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/orgplan"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// serverconfig.go is the server file's service side: the bound repo or a
// local file is planned into server_config_plans, an approve queues the apply
// job (serverconfig_apply.go walks it). Mirrors orgconfig.go.

type (
	// ServerPlan is one stored plan of the server file; its Plan column is a
	// ServerConfigPlan as JSON. Source is "repo" or "local"; a local plan's
	// File holds the bytes it was made from.
	ServerPlan       = store.ServerPlan
	ServerConfigPlan = serverconfig.Plan
)

// ServerBinding is where the server file lives: the five server_config_*
// settings. Repo "" = unbound.
type ServerBinding struct {
	ConnectorID string `json:"connector_id"` // a server connector's id
	Repo        string `json:"repo"`
	Branch      string `json:"branch"` // "" = the repo's default
	Path        string `json:"path"`   // stored "" reads as serverconfig.DefaultPath
	Auto        bool   `json:"auto"`
}

// Job payloads. A server job has no org_id: it lists under /admin/jobs only.
// Lock keys: server-plan "serverconfig"; server-apply "serverconfig",
// "serverplan:<id>", and "orgconfig:<org id>" for each org of the file that
// already exists when the apply is approved.
type (
	serverPlanJob  struct{}
	serverApplyJob struct {
		PlanID string `json:"plan_id"`
		// ApproverID owns the orgs the apply creates; "" for an auto-apply,
		// which never creates one (creating an org is an impact line).
		ApproverID string `json:"approver_id,omitempty"`
	}
)

// ServerConfigBinding is the server file's binding as stored.
func (o *Orchestrator) ServerConfigBinding(ctx context.Context) (ServerBinding, error) {
	var b ServerBinding
	for key, dst := range map[string]*string{
		"server_config_connector": &b.ConnectorID,
		"server_config_repo":      &b.Repo,
		"server_config_branch":    &b.Branch,
		"server_config_path":      &b.Path,
	} {
		v, err := o.settings.Get(ctx, key)
		if err != nil {
			return b, err
		}
		*dst = v
	}
	if b.Repo != "" {
		b.Path = cmp.Or(b.Path, serverconfig.DefaultPath)
	}
	v, err := o.settings.Get(ctx, "server_config_auto")
	b.Auto = v == "true"
	return b, err
}

// BindServerConfig binds the server to its stackr-server.yml and plans it at
// once, as SetOrgConfigRepo does for an org. An empty repo unbinds: the
// settings clear and the plans nobody approved are rejected. The connector is
// a server connector.
func (o *Orchestrator) BindServerConfig(
	ctx context.Context,
	connectorID, repo, branch, path string,
	auto bool,
) (ServerBinding, error) {
	repo = githubapp.RepoURL(repo)
	if repo == "" {
		connectorID, branch, path, auto = "", "", "", false
	} else if _, err := o.conns.Server(ctx, connectorID); err != nil {
		return ServerBinding{}, err // an org's connector, or none, is not there
	}
	err := o.SetSettings(ctx, map[string]string{
		"server_config_connector": connectorID,
		"server_config_repo":      repo,
		"server_config_branch":    branch,
		"server_config_path":      path,
		"server_config_auto":      strconv.FormatBool(auto),
	})
	if err != nil {
		return ServerBinding{}, err
	}
	b, err := o.ServerConfigBinding(ctx)
	if err != nil {
		return b, err
	}
	if repo == "" {
		return b, o.serverPlans.RejectUndecided(ctx)
	}
	_, err = o.planServer(ctx, io.Discard)
	return b, err
}

// PlanServerConfig plans the bound repo's server file at the branch head now
// and stores the row (source "repo").
func (o *Orchestrator) PlanServerConfig(ctx context.Context) (ServerPlan, error) {
	return o.planServer(ctx, io.Discard)
}

// PreviewServerConfig diffs file against the server and stores nothing.
func (o *Orchestrator) PreviewServerConfig(ctx context.Context, file []byte) (ServerConfigPlan, error) {
	f, err := serverconfig.Parse(file)
	if err != nil {
		return ServerConfigPlan{}, errs.Invalidf("file", "%s", err.Error())
	}
	return o.diffServer(ctx, f)
}

// PlanServerFile plans a local file (the CLI's `stackr server apply <file>`)
// and stores the row with source "local" and the bytes in File. Allowed while
// a repo is bound. A file that does not parse is refused, not stored: the
// sender has it in hand.
func (o *Orchestrator) PlanServerFile(ctx context.Context, file []byte) (ServerPlan, error) {
	f, err := serverconfig.Parse(file)
	if err != nil {
		return ServerPlan{}, errs.Invalidf("file", "%s", err.Error())
	}
	p, err := o.diffServer(ctx, f)
	if err != nil {
		return ServerPlan{}, err
	}
	return o.storeServerPlan(ctx, p, ServerPlan{Source: sourceLocal, File: string(file)})
}

// ServerPlans is the newest server plans, newest first, at most limit.
func (o *Orchestrator) ServerPlans(ctx context.Context, limit int) ([]ServerPlan, error) {
	return o.serverPlans.Latest(ctx, limit)
}

func (o *Orchestrator) ServerPlan(ctx context.Context, id string) (ServerPlan, error) {
	return o.serverPlans.Get(ctx, id)
}

// ApproveServerPlan approves a pending plan and queues its apply: the
// contract of ApproveOrgPlan (a blocked plan, an unconfirmed risky one and a
// tick outside the plan's removals are refused).
func (o *Orchestrator) ApproveServerPlan(ctx context.Context, id string, opts ApproveOpts) (Job, error) {
	pl, err := o.serverPlans.Get(ctx, id)
	if err != nil {
		return Job{}, err
	}
	return o.approveServerPlan(ctx, pl, opts)
}

// approveServerPlan stamps the approve and queues the apply, detached from
// ctx like approveOrgPlan. The lock names the plan, so a later apply never
// supersedes it, and each org of the file that exists already, so an apply
// and that org's own plan or apply queue behind each other.
func (o *Orchestrator) approveServerPlan(ctx context.Context, pl ServerPlan, opts ApproveOpts) (Job, error) {
	if pl.Status == orgplan.Pending {
		if err := checkApprove(pl.ID, pl.Plan, opts); err != nil {
			return Job{}, err
		}
	}
	locks, err := o.serverApplyLocks(ctx, pl)
	if err != nil {
		return Job{}, err
	}
	ctx = context.WithoutCancel(ctx)
	if _, err := o.serverPlans.Approve(ctx, pl.ID, opts.Ticked, opts.Confirm); err != nil {
		return Job{}, err
	}
	return o.enqueue(ctx, kindServerApply, serverApplyJob{PlanID: pl.ID, ApproverID: opts.ApproverID}, locks...)
}

// serverApplyLocks is the apply's lock set. A repo plan stores no file, so
// the orgs come off the plan's own org rows: a change whose kind starts "org"
// names the org's slug in Tile. An org the apply creates has no id yet and
// cannot be locked.
func (o *Orchestrator) serverApplyLocks(ctx context.Context, pl ServerPlan) ([]string, error) {
	locks := []string{"serverconfig", "serverplan:" + pl.ID}
	if pl.Status != orgplan.Pending || pl.Plan == "" {
		return locks, nil // an error or decided row: Approve refuses it
	}
	var p serverconfig.Plan
	if err := json.Unmarshal([]byte(pl.Plan), &p); err != nil {
		return nil, err
	}
	slugs := map[string]bool{}
	for _, c := range p.Changes {
		if strings.HasPrefix(c.Kind, "org") && c.Tile != "" {
			slugs[c.Tile] = true
		}
	}
	if len(slugs) == 0 {
		return locks, nil
	}
	orgs, err := o.orgs.ListAll(ctx)
	if err != nil {
		return nil, err
	}
	for _, og := range orgs {
		if slugs[og.Slug] {
			locks = append(locks, "orgconfig:"+og.ID)
		}
	}
	return locks, nil
}

// RejectServerPlan ends a pending plan nobody approved.
func (o *Orchestrator) RejectServerPlan(ctx context.Context, id string) (ServerPlan, error) {
	return o.serverPlans.SetStatus(ctx, id, orgplan.Rejected)
}

// queueServerPlan queues a plan of the server file when a push lands on the
// bound repo and branch through the bound connector (connectorID is the
// webhook's); a newer one supersedes a queued plan. It answers the job, or
// the zero Job when the push is not the server file's. S1's Webhook calls it.
func (o *Orchestrator) queueServerPlan(
	ctx context.Context,
	connectorID, repo, branch, defaultBranch string,
) (Job, error) {
	b, err := o.ServerConfigBinding(ctx)
	if err != nil {
		return Job{}, err
	}
	if b.Repo == "" || b.ConnectorID != connectorID ||
		promote.NormalizeRepo(b.Repo) != promote.NormalizeRepo(repo) ||
		branch != cmp.Or(b.Branch, defaultBranch) {
		return Job{}, nil
	}
	return o.enqueue(ctx, kindServerPlan, serverPlanJob{}, "serverconfig")
}

// ExportServerConfig writes the live server as a stackr-server.yml. A backup
// destination's keys are written as server param refs, so it first stores
// the keys of every destination that has no such param yet (an existing
// param is never overwritten); then the export plans clean on this server.
func (o *Orchestrator) ExportServerConfig(ctx context.Context) ([]byte, error) {
	if err := o.seedDestParams(ctx); err != nil {
		return nil, err
	}
	live, err := o.serverLive(ctx, nil)
	if err != nil {
		return nil, err
	}
	return serverconfig.Export(live)
}

// runServerPlan is the server-plan job: plan the bound repo's file.
func (o *Orchestrator) runServerPlan(ctx context.Context, r *jobs.Run, _ serverPlanJob) error {
	_, err := o.planServer(ctx, r.Log)
	return err
}

// runServerApply is the server-apply job: the plan ends applied, or error
// with what failed. What applied before a failure stays; the next plan shows
// the rest.
func (o *Orchestrator) runServerApply(ctx context.Context, r *jobs.Run, p serverApplyJob) error {
	err := o.applyServerPlan(ctx, p, r.Log)
	done := context.WithoutCancel(ctx)
	if err != nil {
		_, serr := o.serverPlans.SetError(done, p.PlanID, err.Error())
		return errors.Join(err, serr)
	}
	_, err = o.serverPlans.SetStatus(done, p.PlanID, orgplan.Applied)
	return err
}

// Plan sources.
const (
	sourceRepo  = "repo"
	sourceLocal = "local"
)

// planServer plans the bound repo's file at the branch head and stores the
// row, superseding the last undecided one. A file that does not fetch or
// parse stores an error row. With server_config_auto on, a pending plan that
// is AutoOK is approved and its apply queued.
func (o *Orchestrator) planServer(ctx context.Context, log io.Writer) (ServerPlan, error) {
	b, err := o.ServerConfigBinding(ctx)
	if err != nil {
		return ServerPlan{}, err
	}
	if b.Repo == "" {
		return ServerPlan{}, errs.Conflictf("The server has no config repo; bind one first.")
	}
	data, sha, err := o.serverFile(ctx, "", log)
	var f *serverconfig.File
	if err == nil {
		f, err = serverconfig.Parse(data)
	}
	if err != nil {
		_, _ = fmt.Fprintf(log, "server config: %v\n", err)
		return o.createServerPlan(ctx, ServerPlan{
			Source:  sourceRepo,
			Commit:  sha,
			Summary: "server config invalid",
			Status:  orgplan.Error,
			Error:   err.Error(),
		})
	}
	p, err := o.diffServer(ctx, f)
	if err != nil {
		return ServerPlan{}, err
	}
	row, err := o.storeServerPlan(ctx, p, ServerPlan{Source: sourceRepo, Commit: sha})
	_, _ = fmt.Fprintf(log, "server plan at %s: %s\n", sha, p.Summary())
	if err != nil || row.Status != orgplan.Pending || !p.AutoOK() || !b.Auto {
		return row, err
	}
	_, _ = fmt.Fprintf(log, "auto apply is on: approved\n")
	_, err = o.approveServerPlan(ctx, row, ApproveOpts{})
	return row, err
}

// diffServer is f against the server as it is now.
func (o *Orchestrator) diffServer(ctx context.Context, f *serverconfig.File) (serverconfig.Plan, error) {
	live, err := o.serverLive(ctx, f)
	if err != nil {
		return serverconfig.Plan{}, err
	}
	return o.limitBlock(ctx, f, serverconfig.Diff(f, live)), nil
}

// storeServerPlan stores p as the row r describes (Source, Commit, File),
// clean when there is nothing to do.
func (o *Orchestrator) storeServerPlan(ctx context.Context, p serverconfig.Plan, r ServerPlan) (ServerPlan, error) {
	blob, err := json.Marshal(p)
	if err != nil {
		return r, err
	}
	r.Plan, r.Summary, r.Status = string(blob), p.Summary(), orgplan.Pending
	if len(p.Changes) == 0 && !p.Blocked() {
		r.Status = orgplan.Clean
	}
	return o.createServerPlan(ctx, r)
}

// createServerPlan stores a plan; the supersede and the insert are one
// transaction.
func (o *Orchestrator) createServerPlan(ctx context.Context, p ServerPlan) (ServerPlan, error) {
	err := o.store.Tx(ctx, func(tx store.Tx) error {
		var err error
		p, err = orgplan.NewServer(tx.ServerPlans).Create(ctx, p)
		return err
	})
	return p, err
}

package service

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/orgconfig"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/promote"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domainres"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/orgplan"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/params"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/settings"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

type (
	// OrgPlan is one stored plan of the org's config file; its Plan column
	// is an OrgConfigPlan as JSON.
	OrgPlan       = store.OrgPlan
	OrgConfigPlan = orgconfig.Plan
)

type orgPlanJob struct {
	OrgID string `json:"org_id"`
}

type orgApplyJob struct {
	OrgID  string `json:"org_id"`
	PlanID string `json:"plan_id"`
}

// SetOrgConfigRepo binds the org to its stackr-org.yml and plans it at
// once (v0's Save and plan). An empty repo unbinds: the five columns clear
// and the plans nobody approved are rejected.
func (o *Orchestrator) SetOrgConfigRepo(
	ctx context.Context,
	orgID, connectorID, repo, branch, path string,
	auto bool,
) (Org, error) {
	og, err := o.orgs.Get(ctx, orgID)
	if err != nil {
		return og, err
	}
	if connectorID != "" && repo != "" {
		if _, err := o.conns.Get(ctx, og.ID, connectorID); err != nil {
			return og, err // another org's connector is not there
		}
	}
	if og, err = o.orgs.SetConfigRepo(ctx, og, connectorID, repo, branch, path, auto); err != nil {
		return og, err
	}
	if og.ConfigRepo == "" {
		return og, o.orgPlans.RejectPending(ctx, og.ID)
	}
	_, err = o.planOrg(ctx, og.ID, io.Discard)
	return og, err
}

// PlanOrgConfig plans the org's file at the bound branch's head now, as
// PlanPromote does, and stores the row.
func (o *Orchestrator) PlanOrgConfig(ctx context.Context, orgID string) (OrgPlan, error) {
	return o.planOrg(ctx, orgID, io.Discard)
}

// PreviewOrgConfig diffs file against the org and stores nothing.
func (o *Orchestrator) PreviewOrgConfig(ctx context.Context, orgID string, file []byte) (OrgConfigPlan, error) {
	og, err := o.orgs.Get(ctx, orgID)
	if err != nil {
		return OrgConfigPlan{}, err
	}
	if og.ConfigRepo == "" {
		return OrgConfigPlan{}, errs.Conflictf("This organization has no config repo; bind one first.")
	}
	f, err := orgconfig.Parse(file)
	if err != nil {
		return OrgConfigPlan{}, errs.Invalidf("file", "%s", err.Error())
	}
	live, err := o.orgLive(ctx, og)
	if err != nil {
		return OrgConfigPlan{}, err
	}
	return orgconfig.Diff(f, live), nil
}

// OrgPlans is the org's newest plans, newest first, at most limit.
func (o *Orchestrator) OrgPlans(ctx context.Context, orgID string, limit int) ([]OrgPlan, error) {
	return o.orgPlans.ForOrg(ctx, orgID, limit)
}

func (o *Orchestrator) OrgPlan(ctx context.Context, id string) (OrgPlan, error) {
	return o.orgPlans.Get(ctx, id)
}

// ApproveOrgPlan approves a pending plan with no blocker and queues its
// apply. A blocked plan is refused with the blocker text; an approved,
// applied or rejected one is refused by leaf/orgplan.
func (o *Orchestrator) ApproveOrgPlan(ctx context.Context, id string) (Job, error) {
	pl, err := o.orgPlans.Get(ctx, id)
	if err != nil {
		return Job{}, err
	}
	return o.approveOrgPlan(ctx, pl)
}

// RejectOrgPlan ends a pending plan nobody approved.
func (o *Orchestrator) RejectOrgPlan(ctx context.Context, id string) (OrgPlan, error) {
	return o.orgPlans.SetStatus(ctx, id, orgplan.Rejected)
}

// ExportOrgConfig writes the org as a stackr-org.yml (orgconfig.Export).
func (o *Orchestrator) ExportOrgConfig(ctx context.Context, orgID string) ([]byte, error) {
	og, err := o.orgs.Get(ctx, orgID)
	if err != nil {
		return nil, err
	}
	live, err := o.orgLive(ctx, og)
	if err != nil {
		return nil, err
	}
	return orgconfig.Export(live)
}

// queueOrgPlan queues a plan of the org's file when a push lands on its
// repo and branch; a newer one supersedes a queued plan.
func (o *Orchestrator) queueOrgPlan(ctx context.Context, orgID, repo, branch, defaultBranch string) error {
	og, err := o.orgs.Get(ctx, orgID)
	if err != nil {
		return err
	}
	if og.ConfigRepo == "" || promote.NormalizeRepo(og.ConfigRepo) != promote.NormalizeRepo(repo) ||
		branch != cmp.Or(og.ConfigBranch, defaultBranch) {
		return nil
	}
	_, err = o.enqueue(ctx, kindOrgPlan, orgPlanJob{OrgID: og.ID}, "orgconfig:"+og.ID)
	return err
}

// planOrg plans the org's file at the bound branch's head and stores the
// row, superseding the last undecided one. A file that does not fetch or
// parse stores an error row. With config_auto on, a pending plan with no
// blocker is approved and its apply queued.
func (o *Orchestrator) planOrg(ctx context.Context, orgID string, log io.Writer) (OrgPlan, error) {
	og, err := o.orgs.Get(ctx, orgID)
	if err != nil {
		return OrgPlan{}, err
	}
	if og.ConfigRepo == "" {
		return OrgPlan{}, errs.Conflictf("This organization has no config repo; bind one first.")
	}
	data, sha, err := o.orgFile(ctx, og, "", log)
	var f *orgconfig.File
	if err == nil {
		f, err = orgconfig.Parse(data)
	}
	if err != nil {
		_, _ = fmt.Fprintf(log, "org config: %v\n", err)
		return o.createOrgPlan(ctx, OrgPlan{
			OrgID:   og.ID,
			Commit:  sha,
			Summary: "org config invalid",
			Status:  orgplan.Error,
			Error:   err.Error(),
		})
	}
	live, err := o.orgLive(ctx, og)
	if err != nil {
		return OrgPlan{}, err
	}
	p := orgconfig.Diff(f, live)
	blob, err := json.Marshal(p)
	if err != nil {
		return OrgPlan{}, err
	}
	status := orgplan.Pending
	if len(p.Changes) == 0 && !p.Blocked() {
		status = orgplan.Clean
	}
	row, err := o.createOrgPlan(ctx, OrgPlan{
		OrgID:   og.ID,
		Commit:  sha,
		Summary: p.Summary(),
		Plan:    string(blob),
		Status:  status,
	})
	_, _ = fmt.Fprintf(log, "org plan at %s: %s\n", sha, p.Summary())
	if err != nil || status != orgplan.Pending || p.Blocked() || !og.ConfigAuto {
		return row, err
	}
	_, _ = fmt.Fprintf(log, "auto apply is on: approved\n")
	_, err = o.approveOrgPlan(ctx, row)
	return row, err
}

// createOrgPlan stores a plan; the supersede and the insert are one
// transaction.
func (o *Orchestrator) createOrgPlan(ctx context.Context, p OrgPlan) (OrgPlan, error) {
	err := o.store.Tx(ctx, func(tx store.Tx) error {
		var err error
		p, err = orgplan.New(tx.OrgPlans).Create(ctx, p)
		return err
	})
	return p, err
}

// approveOrgPlan stamps the approve and queues the apply, detached from
// ctx: an approved plan with no apply job would sit approved for good.
// The lock names the plan too, so a later apply never supersedes it.
func (o *Orchestrator) approveOrgPlan(ctx context.Context, pl OrgPlan) (Job, error) {
	if pl.Status == orgplan.Pending {
		var p OrgConfigPlan
		if err := json.Unmarshal([]byte(pl.Plan), &p); err != nil {
			return Job{}, fmt.Errorf("org plan %s: %w", pl.ID, err)
		}
		if p.Blocked() {
			return Job{}, errs.Conflictf("This plan is blocked: %s", strings.Join(p.Blockers, "; "))
		}
	}
	ctx = context.WithoutCancel(ctx)
	if _, err := o.orgPlans.Approve(ctx, pl.ID); err != nil {
		return Job{}, err
	}
	return o.enqueue(
		ctx,
		kindOrgApply,
		orgApplyJob{
			OrgID:  pl.OrgID,
			PlanID: pl.ID,
		},
		"orgconfig:"+pl.OrgID,
		"orgplan:"+pl.ID,
	)
}

// orgLive is what the file is diffed against.
func (o *Orchestrator) orgLive(ctx context.Context, og store.Org) (orgconfig.Live, error) {
	live := orgconfig.Live{Org: og}
	var err error
	if live.Orgs, err = o.orgs.ListAll(ctx); err != nil {
		return live, err
	}
	if live.Claims, err = o.claims(ctx); err != nil {
		return live, err
	}
	if live.Params, err = o.params.Values(ctx, params.Scope{Kind: "org", ID: og.ID}, true); err != nil {
		return live, err
	}
	if live.Domains, err = o.domainres.ListAll(ctx); err != nil {
		return live, err
	}
	if live.Connectors, err = o.conns.ListConnected(ctx, og.ID); err != nil {
		return live, err
	}
	sts, err := o.stacks.List(ctx, og.ID)
	if err != nil {
		return live, err
	}
	for _, st := range sts {
		live.Stacks = append(live.Stacks, orgconfig.StackLive{Stack: st})
	}
	return live, nil
}

// runOrgApply is the apply job: the plan ends applied, or error with what
// failed. What applied before a failure stays; the next plan shows the rest.
func (o *Orchestrator) runOrgApply(ctx context.Context, r *jobs.Run, p orgApplyJob) error {
	err := o.applyOrgPlan(ctx, p.PlanID, r.Log)
	done := context.WithoutCancel(ctx)
	if err != nil {
		_, serr := o.orgPlans.SetError(done, p.PlanID, err.Error())
		return errors.Join(err, serr)
	}
	_, err = o.orgPlans.SetStatus(done, p.PlanID, orgplan.Applied)
	return err
}

// applyOrgPlan refetches the file at the plan's commit, diffs it again
// (the org may have moved since the plan), walks the changes, then queues
// a push of each stack it created or rebound so its file makes its envs.
func (o *Orchestrator) applyOrgPlan(ctx context.Context, planID string, log io.Writer) error {
	pl, err := o.orgPlans.Get(ctx, planID)
	if err != nil {
		return err
	}
	og, err := o.orgs.Get(ctx, pl.OrgID)
	if err != nil {
		return err
	}
	data, _, err := o.orgFile(ctx, og, pl.Commit, log)
	if err != nil {
		return err
	}
	f, err := orgconfig.Parse(data)
	if err != nil {
		return err
	}
	live, err := o.orgLive(ctx, og)
	if err != nil {
		return err
	}
	plan := orgconfig.Diff(f, live)
	if plan.Blocked() {
		return fmt.Errorf("blocked: %s", strings.Join(plan.Blockers, "; "))
	}
	bound, err := o.walkOrgPlan(ctx, og, f, live, plan, log)
	if err != nil {
		return err
	}
	// ponytail: a push that fails to queue marks the plan error though the
	// walk applied; the next plan reads clean, so that stack waits for its
	// repo's next webhook push. Retry the push on the next plan if it bites.
	var errList []error
	for _, s := range bound {
		errList = append(errList, o.pushBound(ctx, s, f.Stacks[s.Slug], pl.Commit, log))
	}
	return errors.Join(errList...)
}

// walkOrgPlan applies plan's changes in its order, each through its verb,
// and returns the stacks it created or rebound. A stack's rebind fields
// and a domain's two fields are one call each, and every param, new or
// changed, is one SetParams.
func (o *Orchestrator) walkOrgPlan(
	ctx context.Context,
	og store.Org,
	f *orgconfig.File,
	live orgconfig.Live,
	plan orgconfig.Plan,
	log io.Writer,
) ([]store.Stack, error) {
	stackIDs := map[string]string{}
	for _, s := range live.Stacks {
		stackIDs[s.Stack.Slug] = s.Stack.ID
	}
	var bound []store.Stack
	done := map[string]bool{}
	for _, c := range plan.Changes {
		key := ""
		switch c.Kind {
		case "param", "param-update":
			key = "param"
		case "rebind", "domain-update":
			key = c.Kind + ":" + c.Tile
		}
		if key != "" {
			if done[key] {
				continue
			}
			done[key] = true
		}
		// a param row's New is its value; the key names it
		what := cmp.Or(c.Tile, c.New, c.Field)
		switch c.Kind {
		case "rename":
			what = c.Old + " → " + c.New
		case "param", "param-update":
			what = c.Field
		}
		_, _ = fmt.Fprintf(log, "%s %s\n", c.Kind, what)
		var err error
		switch c.Kind {
		case "rename":
			_, err = o.RenameStack(ctx, stackIDs[c.Old], c.New)
			stackIDs[c.New] = stackIDs[c.Old]
		case "org":
			_, err = o.RenameOrg(ctx, og.ID, c.New)
		case "param", "param-update":
			err = o.SetParams(ctx, ParamScope{Kind: "org", ID: og.ID}, orgParams(f, plan))
		case "defaults":
			_, err = o.SetOrgSettings(ctx, og.ID, settings.Settings(*f.Defaults).JSON())
		case "colors":
			_, err = o.SetOrgEnvColors(ctx, og.ID, c.New)
		case "create", "rebind":
			var st Stack
			if st, err = o.bindOrgStack(ctx, og, stackIDs[c.Tile], c.Tile, f.Stacks[c.Tile]); err == nil {
				stackIDs[c.Tile] = st.ID
				bound = append(bound, st)
			}
		case "domain", "domain-update":
			err = o.putOrgDomain(ctx, og.ID, cmp.Or(c.Tile, c.New), f.Domains, live.Domains)
		}
		if err != nil {
			return bound, fmt.Errorf("%s %s: %w", c.Kind, what, err)
		}
	}
	return bound, nil
}

// orgParams is one entry per param line of the plan, as the file declares
// it: a param with its value, a secret declared (Merge keeps its value).
func orgParams(f *orgconfig.File, plan orgconfig.Plan) []ParamEntry {
	var es []ParamEntry
	for _, c := range plan.Changes {
		if c.Kind != "param" && c.Kind != "param-update" {
			continue
		}
		col, name, _ := strings.Cut(c.Field, ".")
		decl := f.Params[col][name]
		e := ParamEntry{
			Collection: col,
			Name:       name,
			Kind:       decl.Type,
		}
		if decl.Value != nil {
			e.Value = *decl.Value
		}
		es = append(es, e)
	}
	return es
}

// bindOrgStack makes the stack when id is "" and binds it where ref says.
func (o *Orchestrator) bindOrgStack(
	ctx context.Context,
	og store.Org,
	id, slug string,
	ref orgconfig.StackRef,
) (Stack, error) {
	if id == "" {
		st, err := o.CreateStack(ctx, og.ID, slug, "")
		if err != nil {
			return st, err
		}
		id = st.ID
	}
	b := ref.Binding(og)
	return o.SetConfigRepo(ctx, id, b.ConnectorID, b.Repo, b.Branch, b.Path)
}

// pushBound queues the push a webhook would for a stack the apply created
// or rebound, at its config branch's head; a stack whose file is in the
// org repo (path alone) is pushed at the plan's commit.
func (o *Orchestrator) pushBound(
	ctx context.Context,
	st Stack,
	ref orgconfig.StackRef,
	commit string,
	log io.Writer,
) error {
	branch, sha, err := o.configHead(ctx, st)
	if err != nil {
		return fmt.Errorf("stack %s: head of %s: %w", st.Slug, st.ConfigRepo, err)
	}
	if ref.Repo == "" {
		sha = commit
	}
	p := pushJob{
		StackID: st.ID,
		Event: promote.Event{
			Repo:   st.ConfigRepo,
			Branch: branch,
			Commit: sha,
		},
	}
	if st.ConfigBranch == "" {
		p.DefaultBranch = branch
	}
	_, _ = fmt.Fprintf(log, "push %s at %s@%s\n", st.Slug, branch, sha)
	_, err = o.enqueuePush(ctx, p)
	return err
}

// putOrgDomain creates or updates the org domain resource for host (as
// Diff normalized it) from its domains: entry.
func (o *Orchestrator) putOrgDomain(
	ctx context.Context,
	orgID, host string,
	want []orgconfig.Reservation,
	have []store.DomainResource,
) error {
	i := slices.IndexFunc(want, func(r orgconfig.Reservation) bool { return installspec.CleanHost(r.Host) == host })
	if i < 0 {
		return fmt.Errorf("no domains: entry for %s", host)
	}
	r := want[i]
	j := slices.IndexFunc(have, func(d store.DomainResource) bool { return d.Host == host })
	if j < 0 {
		_, err := o.CreateDomainResource(ctx, domainres.Org, orgID, host, r.IncludeEnvOnDefault, r.ACMEEmail)
		return err
	}
	_, err := o.UpdateDomainResource(ctx, have[j].ID, r.IncludeEnvOnDefault, r.ACMEEmail)
	return err
}
